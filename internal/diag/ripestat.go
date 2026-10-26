package diag

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RIPEstat looks addresses up in the RIPEstat Data API
// (https://stat.ripe.net/docs/data-api/), which is free, needs no account
// and covers every registry, not only RIPE's region:
//
//   - network-info: the routed prefix and origin AS of an address
//   - as-overview: the AS's registered holder ("HANDLE Name")
//   - maxmind-geo-lite: country and city of the address (GeoLite2 data)
//   - whats-my-ip: the caller's address, a fallback finder when UDP (and so
//     STUN) is blocked
//
// It is also an AddrFinder: PublicAddr asks whats-my-ip over a connection
// pinned to the requested family.
//
// RIPEstat's proxy sometimes holds a request for 5 to 20 s and then answers
// 502, while the same query sent again answers in 0.4 s (its process_time
// is ~10 ms either way, so the stall is not the lookup). Registry lookups
// are therefore hedged: a second copy goes out on a fresh connection when
// the first has no reply after HedgeAfter, and the first good reply wins.
type RIPEstat struct {
	base       string
	sourceApp  string
	clients    map[Family]*http.Client
	hedge      *http.Client // its own connections: a stalled one is not reused
	hedgeAfter time.Duration
	slots      slots
}

// RIPEstatOptions tune RIPEstat; zero fields take the defaults.
type RIPEstatOptions struct {
	BaseURL string // default "https://stat.ripe.net"
	// SourceApp names the caller to RIPE NCC, as the API asks of
	// regular users. Default "lowpath".
	SourceApp string
	Timeout   time.Duration // per request, default 6 s
	// HedgeAfter is how long a registry lookup waits for its first reply
	// before a second copy is sent (default 2.5 s, well above the usual
	// 0.4 to 2 s; negative: never). A copy is also sent at once when the
	// first fails with a timeout, a reset or HTTP 500/502/503/504. Each
	// lookup sends at most two copies.
	HedgeAfter time.Duration
	// MaxInFlight caps concurrent requests (default 6; RIPEstat allows 8
	// per address). A hedge is skipped when no slot is free.
	MaxInFlight int
	// Dial opens TCP connections (default a *net.Dialer). network is
	// "tcp4" or "tcp6" when PublicAddr asks for one family, else "tcp".
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

func NewRIPEstat(o RIPEstatOptions) *RIPEstat {
	if o.BaseURL == "" {
		o.BaseURL = "https://stat.ripe.net"
	}
	if o.SourceApp == "" {
		o.SourceApp = "lowpath"
	}
	if o.Timeout <= 0 {
		o.Timeout = 6 * time.Second
	}
	if o.Dial == nil {
		d := &net.Dialer{Timeout: 4 * time.Second}
		o.Dial = d.DialContext
	}
	if o.HedgeAfter == 0 {
		o.HedgeAfter = 2500 * time.Millisecond
	}
	if o.MaxInFlight <= 0 {
		o.MaxInFlight = 6
	}
	r := &RIPEstat{
		base:       strings.TrimRight(o.BaseURL, "/"),
		sourceApp:  o.SourceApp,
		clients:    map[Family]*http.Client{},
		hedgeAfter: o.HedgeAfter,
		slots:      make(slots, o.MaxInFlight),
	}
	newClient := func(f Family) *http.Client {
		network := map[Family]string{IPv4: "tcp4", IPv6: "tcp6", anyFamily: "tcp"}[f]
		dial := o.Dial
		// whats-my-ip (the family clients) must go direct: through an
		// HTTP(S)_PROXY it would report the proxy's address, which is not
		// the one a game's UDP traffic uses, and the family pin would only
		// pick how the proxy is reached. Registry lookups may use a proxy.
		proxy := http.ProxyFromEnvironment
		if f != anyFamily {
			proxy = nil
		}
		t := &http.Transport{
			Proxy: proxy,
			DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
				return dial(ctx, network, addr)
			},
			TLSHandshakeTimeout:   4 * time.Second,
			ResponseHeaderTimeout: o.Timeout,
			IdleConnTimeout:       30 * time.Second,
			MaxIdleConnsPerHost:   2,
			ForceAttemptHTTP2:     true,
		}
		return &http.Client{Transport: t, Timeout: o.Timeout}
	}
	for _, f := range []Family{IPv4, IPv6, anyFamily} {
		r.clients[f] = newClient(f)
	}
	// HTTP/2 would put the hedge on the stalled connection; a transport of
	// its own makes it a new connection, possibly to another front end.
	r.hedge = newClient(anyFamily)
	return r
}

// slots is a counting semaphore for requests in flight.
type slots chan struct{}

func (s slots) acquire(ctx context.Context) error {
	select {
	case s <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s slots) tryAcquire() bool {
	select {
	case s <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s slots) release() { <-s }

// transientError is a failure that says nothing about the address looked
// up: a stalled or reset connection, or a busy server. Sending the same
// request again may well work.
type transientError struct{ error }

func (e transientError) Unwrap() error { return e.error }

// transient reports whether err is a timeout or a transientError.
func transient(err error) bool {
	if err == nil {
		return false
	}
	var te transientError
	var ne net.Error
	return errors.As(err, &te) || errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
}

// permanent reports transport failures that a second copy a moment later
// cannot fix: a certificate that does not verify (an intercepting proxy or
// antivirus, a wrong clock) or a host name that does not exist. A DNS
// timeout or temporary failure is not one of them.
func permanent(err error) bool {
	var cv *tls.CertificateVerificationError
	var ua x509.UnknownAuthorityError
	var he x509.HostnameError
	var ci x509.CertificateInvalidError
	var dns *net.DNSError
	return errors.As(err, &cv) || errors.As(err, &ua) || errors.As(err, &he) || errors.As(err, &ci) ||
		(errors.As(err, &dns) && dns.IsNotFound)
}

// anyFamily lets a request use whichever family reaches RIPEstat.
const anyFamily Family = ""

// ripeResponse is the envelope every data call shares.
type ripeResponse[T any] struct {
	Status   string          `json:"status"`
	Messages json.RawMessage `json:"messages"` // [[level, text], ...]
	Data     T               `json:"data"`
}

// call fetches /data/<name>/data.json?resource=<resource> over family f
// and decodes its data object. Registry lookups (anyFamily) are hedged;
// whats-my-ip is not: a slow reply there only delays a fallback finder.
func call[T any](ctx context.Context, r *RIPEstat, f Family, name, resource string) (T, error) {
	var zero T
	var data json.RawMessage
	var err error
	if f == anyFamily {
		data, err = r.hedged(ctx, name, resource)
	} else {
		data, err = r.attempt(ctx, r.clients[f], name, resource, true)
	}
	if err != nil {
		return zero, err
	}
	var out T
	if err := json.Unmarshal(data, &out); err != nil {
		return zero, fmt.Errorf("RIPEstat %s: unreadable reply: %w", name, err)
	}
	return out, nil
}

// hedged sends one request and, when it has no reply after hedgeAfter or
// fails transiently, a second copy on the hedge client. The first good
// reply wins and cancels the other. When both fail, the first error is
// returned. Answers that are not transient (a 429, an error status) are
// never sent again.
func (r *RIPEstat) hedged(ctx context.Context, name, resource string) (json.RawMessage, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		data json.RawMessage
		err  error
	}
	results := make(chan result, 2)
	send := func(c *http.Client, wait bool) {
		go func() {
			d, err := r.attempt(ctx, c, name, resource, wait)
			results <- result{d, err}
		}()
	}
	send(r.clients[anyFamily], true)
	pending, hedgedYet := 1, r.hedgeAfter < 0
	hedge := func() {
		hedgedYet = true
		if ctx.Err() == nil && r.slots.tryAcquire() {
			pending++
			send(r.hedge, false)
		}
	}
	var timer <-chan time.Time
	if !hedgedYet {
		t := time.NewTimer(r.hedgeAfter)
		defer t.Stop()
		timer = t.C
	}
	var firstErr error
	for {
		select {
		case <-timer:
			timer = nil
			if !hedgedYet {
				hedge()
			}
		case got := <-results:
			pending--
			if got.err == nil {
				return got.data, nil
			}
			if firstErr == nil {
				firstErr = got.err
			}
			if !hedgedYet && transient(got.err) {
				hedge()
			}
			if pending == 0 {
				return nil, firstErr
			}
		}
	}
}

// attempt does one GET of /data/<name>/data.json and returns the reply's
// data object. With wait it queues for a request slot; without, the caller
// already holds one. Transport failures and HTTP 500/502/503/504 come back
// as a transientError, unless ctx ended (that is the caller's doing) or the
// failure is permanent.
func (r *RIPEstat) attempt(ctx context.Context, c *http.Client, name, resource string, wait bool) (json.RawMessage, error) {
	if wait {
		if err := r.slots.acquire(ctx); err != nil {
			return nil, fmt.Errorf("RIPEstat %s: %w", name, err)
		}
	}
	defer r.slots.release()
	q := url.Values{"sourceapp": {r.sourceApp}}
	if resource != "" {
		q.Set("resource", resource)
	}
	u := r.base + "/data/" + name + "/data.json?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	fail := func(err error) error {
		wrapped := fmt.Errorf("RIPEstat %s: %w", name, err)
		if ctx.Err() == nil && !permanent(err) {
			return transientError{wrapped}
		}
		return wrapped
	}
	resp, err := c.Do(req)
	if err != nil {
		// A *url.Error repeats the URL, whose query holds the address being
		// looked up; that text reaches the UI and the diagnostics report.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fail(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fail(err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return nil, fail(fmt.Errorf("HTTP %d", resp.StatusCode))
	default:
		return nil, fmt.Errorf("RIPEstat %s: HTTP %d", name, resp.StatusCode)
	}
	var out ripeResponse[json.RawMessage]
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("RIPEstat %s: unreadable reply: %w", name, err)
	}
	if out.Status != "ok" {
		msg := out.Status
		var msgs [][]string
		json.Unmarshal(out.Messages, &msgs) // best effort: the status alone is enough
		for _, m := range msgs {
			if len(m) == 2 && m[0] == "error" {
				msg = clip(m[1], 200)
			}
		}
		return nil, fmt.Errorf("RIPEstat %s: %s", name, msg)
	}
	return out.Data, nil
}

// clip shortens s to at most n runes, for remote text shown in the UI.
func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// PublicAddr asks whats-my-ip which address the request came from.
func (r *RIPEstat) PublicAddr(ctx context.Context, f Family) (PublicAddr, error) {
	d, err := call[struct {
		IP string `json:"ip"`
	}](ctx, r, f, "whats-my-ip", "")
	if err != nil {
		return PublicAddr{}, err
	}
	a, err := netip.ParseAddr(d.IP)
	if err != nil {
		return PublicAddr{}, fmt.Errorf("RIPEstat whats-my-ip: bad address %q", d.IP)
	}
	return PublicAddr{Addr: a.Unmap(), Source: "https stat.ripe.net"}, nil
}

// LookupNetwork finds the prefix and origin AS of a, then the AS holder and
// the address's country and city in parallel. Only a failed network-info
// is an error; the other two leave their fields empty and say so in Partial.
func (r *RIPEstat) LookupNetwork(ctx context.Context, a netip.Addr) (NetworkInfo, error) {
	a = a.Unmap()
	f := anyFamily // the address is the query, not the transport: any route to RIPEstat will do
	ni, err := call[struct {
		ASNs   []string `json:"asns"`
		Prefix string   `json:"prefix"`
	}](ctx, r, f, "network-info", a.String())
	if err != nil {
		return NetworkInfo{}, err
	}
	var info NetworkInfo
	for _, s := range ni.ASNs {
		if n, err := strconv.ParseUint(strings.TrimPrefix(strings.ToUpper(s), "AS"), 10, 32); err == nil {
			info.ASNs = append(info.ASNs, uint32(n))
		}
	}
	if p, err := netip.ParsePrefix(ni.Prefix); err == nil {
		info.Prefix = p
	}
	if len(info.ASNs) == 0 {
		return NetworkInfo{}, errors.New("RIPEstat: " + a.String() + " is not announced on the internet (no origin AS)")
	}
	info.ASN = info.ASNs[0]

	var wg sync.WaitGroup
	var asErr, geoErr error
	wg.Go(func() {
		ov, err := call[struct {
			Holder string `json:"holder"`
		}](ctx, r, f, "as-overview", "AS"+strconv.FormatUint(uint64(info.ASN), 10))
		if err != nil {
			asErr = err
			return
		}
		info.Holder = clip(ov.Holder, 200)
		info.ASName, info.ISP = splitHolder(info.Holder)
	})
	var g geoPlace
	wg.Go(func() {
		g, geoErr = r.geo(ctx, f, a)
	})
	wg.Wait()
	info.Country, info.City = g.country, g.city
	if g.city != "" && g.located {
		info.Lat, info.Lon = &g.lat, &g.lon
	}
	var partial []string
	if asErr != nil {
		partial = append(partial, "ISP name unknown ("+describe(asErr)+")")
	}
	if geoErr != nil {
		partial = append(partial, "city unknown ("+describe(geoErr)+")")
	}
	info.Partial = strings.Join(partial, "; ")
	info.transient = transient(asErr) || transient(geoErr)
	return info, nil
}

// geoPlace is one GeoLite2 location.
type geoPlace struct {
	country, city string
	lat, lon      float64
	located       bool // lat and lon were given and are in range
}

// geo picks the location covering most of the address's space. City is ""
// when GeoLite only knows the country, which is common (anycast services,
// cloud ranges); its coordinates are then the country's center.
func (r *RIPEstat) geo(ctx context.Context, f Family, a netip.Addr) (geoPlace, error) {
	g, err := call[struct {
		Located []struct {
			Locations []struct {
				Country string   `json:"country"`
				City    string   `json:"city"`
				Lat     *float64 `json:"latitude"`
				Lon     *float64 `json:"longitude"`
				Covered float64  `json:"covered_percentage"`
			} `json:"locations"`
		} `json:"located_resources"`
	}](ctx, r, f, "maxmind-geo-lite", a.String())
	if err != nil {
		return geoPlace{}, err
	}
	var out geoPlace
	best := -1.0
	for _, lr := range g.Located {
		for _, l := range lr.Locations {
			if l.Covered > best && (l.Country != "" || l.City != "") {
				best = l.Covered
				out = geoPlace{country: strings.TrimSpace(l.Country), city: strings.TrimSpace(l.City)}
				if l.Lat != nil && l.Lon != nil && *l.Lat >= -90 && *l.Lat <= 90 && *l.Lon >= -180 && *l.Lon <= 180 {
					out.lat, out.lon, out.located = *l.Lat, *l.Lon, true
				}
			}
		}
	}
	return out, nil
}

// splitHolder splits an AS holder string into its handle and name. RIPEstat
// gives "OMANTEL-NAP-AS Oman Telecommunications Company (S.A.O.G)" for RIPE
// and APNIC networks, "GOOGLE - Google LLC" for ARIN ones, and sometimes the
// handle alone.
func splitHolder(h string) (handle, name string) {
	h = strings.TrimSpace(h)
	handle, rest, ok := strings.Cut(h, " ")
	if !isHandle(handle) {
		return "", h
	}
	if !ok {
		return handle, handle
	}
	rest = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rest), "-"))
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return handle, handle
	}
	return handle, rest
}

// isHandle: an AS handle is one word of letters, digits, '-', '_' and '.'
// in a single case ("OMANTEL-NAP-AS", and the odd lowercase "examplenet"); a capitalised
// word such as "Some" starts a name instead.
func isHandle(s string) bool {
	if s == "" {
		return false
	}
	upper, lower := false, false
	for _, c := range s {
		switch {
		case c >= 'A' && c <= 'Z':
			upper = true
		case c >= 'a' && c <= 'z':
			lower = true
		case c >= '0' && c <= '9', c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return !(upper && lower)
}
