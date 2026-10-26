package diag

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Loader fills the connection in one direction until ctx ends, calling add
// with every chunk of bytes moved. It returns nil when ctx ended after the
// line was loaded, and an error when the line could not be loaded at all
// (offline, server refused).
type Loader interface {
	Download(ctx context.Context, add func(n int64)) error
	Upload(ctx context.Context, add func(n int64)) error
	// Endpoint is the load server's host:port, also a latency target.
	Endpoint() string
}

// DefaultLoadURL is Cloudflare's public speed-test endpoint (the one
// speed.cloudflare.com and its MIT-licensed @cloudflare/speedtest use):
// GET /__down?bytes=N sends N bytes, POST /__up takes a body.
const DefaultLoadURL = "https://speed.cloudflare.com"

// HTTPLoaderConfig tunes HTTPLoader. Zero values get the defaults.
type HTTPLoaderConfig struct {
	BaseURL       string // DefaultLoadURL
	DownStreams   int    // parallel downloads (6)
	UpStreams     int    // parallel uploads (4)
	DownChunk     int64  // bytes per download request (25 MB)
	UpChunk       int64  // bytes per upload request (10 MB)
	MaxDownBytes  int64  // stop downloading after this many bytes in one call (750 MB)
	MaxUpBytes    int64  // same for uploads (250 MB)
	UserAgent     string
	DialTimeout   time.Duration // 5 s
	HeaderTimeout time.Duration // 10 s
}

// HTTPLoader fills the line with several parallel HTTP/1.1 transfers. Each
// transfer is its own TCP connection: separate flows fill the bottleneck
// queue the way a game launcher's download does, which one multiplexed
// HTTP/2 connection would not.
type HTTPLoader struct {
	cfg    HTTPLoaderConfig
	base   *url.URL
	client *http.Client
}

// NewHTTPLoader checks the URL and builds the HTTP client.
func NewHTTPLoader(cfg HTTPLoaderConfig) (*HTTPLoader, error) {
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultLoadURL
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("diag: bad load server URL %q", cfg.BaseURL)
	}
	def := func(v *int, d int) {
		if *v <= 0 {
			*v = d
		}
	}
	def64 := func(v *int64, d int64) {
		if *v <= 0 {
			*v = d
		}
	}
	def(&cfg.DownStreams, 6)
	def(&cfg.UpStreams, 4)
	def64(&cfg.DownChunk, 25e6)
	def64(&cfg.UpChunk, 10e6)
	def64(&cfg.MaxDownBytes, 750e6)
	def64(&cfg.MaxUpBytes, 250e6)
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 5 * time.Second
	}
	if cfg.HeaderTimeout <= 0 {
		cfg.HeaderTimeout = 10 * time.Second
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "Lowpath (bufferbloat test)"
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: cfg.DialTimeout, KeepAlive: 15 * time.Second}).DialContext,
		TLSHandshakeTimeout:   cfg.DialTimeout,
		ResponseHeaderTimeout: cfg.HeaderTimeout,
		// An empty TLSNextProto map turns HTTP/2 off: one TCP flow per stream.
		TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{},
		ForceAttemptHTTP2:   false,
		MaxIdleConnsPerHost: max(cfg.DownStreams, cfg.UpStreams),
		IdleConnTimeout:     30 * time.Second,
		DisableCompression:  true, // count bytes as they cross the wire
	}
	return &HTTPLoader{cfg: cfg, base: u, client: &http.Client{Transport: tr}}, nil
}

func (l *HTTPLoader) Endpoint() string {
	if p := l.base.Port(); p != "" {
		return l.base.Host
	}
	port := "443"
	if l.base.Scheme == "http" {
		port = "80"
	}
	return net.JoinHostPort(l.base.Hostname(), port)
}

// Close drops idle connections (call after a test).
func (l *HTTPLoader) Close() { l.client.CloseIdleConnections() }

func (l *HTTPLoader) endpoint(p string, q url.Values) string {
	u := *l.base
	u.Path = p
	u.RawQuery = q.Encode()
	return u.String()
}

func (l *HTTPLoader) Download(ctx context.Context, add func(int64)) error {
	return l.run(ctx, l.cfg.DownStreams, l.cfg.MaxDownBytes, add, func(ctx context.Context, count func(int64) bool) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.endpoint("/__down", url.Values{"bytes": {strconv.FormatInt(l.cfg.DownChunk, 10)}}), nil)
		if err != nil {
			return err
		}
		res, err := l.do(req)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		buf := make([]byte, 64<<10)
		for {
			n, err := res.Body.Read(buf)
			if n > 0 && !count(int64(n)) {
				return nil // byte cap reached
			}
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
		}
	})
}

func (l *HTTPLoader) Upload(ctx context.Context, add func(int64)) error {
	return l.run(ctx, l.cfg.UpStreams, l.cfg.MaxUpBytes, add, func(ctx context.Context, count func(int64) bool) error {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		body := &countingBody{left: l.cfg.UpChunk, count: count, stop: cancel}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.endpoint("/__up", nil), body)
		if err != nil {
			return err
		}
		req.ContentLength = l.cfg.UpChunk
		req.Header.Set("Content-Type", "application/octet-stream")
		res, err := l.do(req)
		if err != nil {
			if body.capped.Load() {
				return nil
			}
			return err
		}
		io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
		res.Body.Close()
		return nil
	})
}

func (l *HTTPLoader) do(req *http.Request) (*http.Response, error) {
	req.Header.Set("User-Agent", l.cfg.UserAgent)
	res, err := l.client.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		return nil, fmt.Errorf("load server answered HTTP %d", res.StatusCode)
	}
	return res, nil
}

// errNoLoad is returned when every transfer failed before moving a byte.
var errNoLoad = errors.New("no data could be transferred")

// run keeps streams transfers going until ctx ends or limit bytes moved.
// One transfer is one call of xfer; count reports bytes and says whether
// to keep going.
func (l *HTTPLoader) run(ctx context.Context, streams int, limit int64, add func(int64), xfer func(context.Context, func(int64) bool) error) error {
	var total atomic.Int64
	count := func(n int64) bool {
		add(n)
		return total.Add(n) < limit
	}
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		lastErr error
	)
	for range streams {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fails := 0
			var mine atomic.Int64 // bytes this stream moved (upload bodies are read on another goroutine)
			mineCount := func(n int64) bool {
				mine.Add(n)
				return count(n)
			}
			for ctx.Err() == nil && total.Load() < limit {
				before := mine.Load()
				err := xfer(ctx, mineCount)
				if ctx.Err() != nil {
					return
				}
				moved := mine.Load() > before
				if err == nil && !moved {
					err = errors.New("load server sent nothing")
				}
				if err != nil {
					mu.Lock()
					lastErr = err
					mu.Unlock()
				}
				if moved {
					fails = 0
					continue
				}
				if fails++; fails >= 3 {
					return // this stream can't get anywhere; let the others try
				}
				select {
				case <-ctx.Done():
				case <-time.After(200 * time.Millisecond):
				}
			}
		}()
	}
	wg.Wait()
	if total.Load() == 0 {
		if lastErr != nil {
			return fmt.Errorf("%w: %v", errNoLoad, lastErr)
		}
		return errNoLoad
	}
	return nil
}

// countingBody is an upload body of incompressible-enough bytes that counts
// what the transport reads from it.
type countingBody struct {
	left   int64
	count  func(int64) bool
	stop   context.CancelFunc
	capped atomic.Bool
	seed   byte
}

func (b *countingBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	for i := range p {
		b.seed = b.seed*31 + 7
		p[i] = b.seed
	}
	b.left -= int64(len(p))
	if !b.count(int64(len(p))) {
		b.capped.Store(true)
		b.stop()
	}
	return len(p), nil
}
