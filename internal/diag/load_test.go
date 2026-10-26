package diag

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// speedServer mimics the two endpoints of Cloudflare's speed test.
func speedServer(t *testing.T) (*httptest.Server, *atomic.Int64) {
	var uploaded atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("GET /__down", func(w http.ResponseWriter, r *http.Request) {
		n, err := strconv.ParseInt(r.URL.Query().Get("bytes"), 10, 64)
		if err != nil {
			http.Error(w, "bad bytes", 400)
			return
		}
		w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
		buf := make([]byte, 32<<10)
		for n > 0 && r.Context().Err() == nil {
			k := min(int64(len(buf)), n)
			if _, err := w.Write(buf[:k]); err != nil {
				return
			}
			n -= k
		}
	})
	mux.HandleFunc("POST /__up", func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		uploaded.Add(n)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &uploaded
}

func TestHTTPLoaderMovesBytesUntilCancelled(t *testing.T) {
	t.Parallel()
	srv, uploaded := speedServer(t)
	l, err := NewHTTPLoader(HTTPLoaderConfig{BaseURL: srv.URL, DownChunk: 1 << 20, UpChunk: 1 << 20, DownStreams: 3, UpStreams: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if l.Endpoint() != srv.Listener.Addr().String() {
		t.Fatalf("endpoint = %q", l.Endpoint())
	}
	for _, dir := range []string{"down", "up"} {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		var moved atomic.Int64
		start := time.Now()
		fn := l.Download
		if dir == "up" {
			fn = l.Upload
		}
		err := fn(ctx, func(n int64) { moved.Add(n) })
		cancel()
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		if moved.Load() < 1<<20 {
			t.Fatalf("%s: moved only %d bytes", dir, moved.Load())
		}
		if el := time.Since(start); el > 2*time.Second {
			t.Fatalf("%s: took %v after the context ended", dir, el)
		}
	}
	if uploaded.Load() == 0 {
		t.Fatal("server received no upload")
	}
}

func TestHTTPLoaderStopsAtByteCap(t *testing.T) {
	t.Parallel()
	srv, _ := speedServer(t)
	l, _ := NewHTTPLoader(HTTPLoaderConfig{BaseURL: srv.URL, DownChunk: 256 << 10, MaxDownBytes: 1 << 20, DownStreams: 2})
	defer l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	var moved atomic.Int64
	if err := l.Download(ctx, func(n int64) { moved.Add(n) }); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("the cap did not end the download early")
	}
	if m := moved.Load(); m < 1<<20 || m > 2<<20 {
		t.Fatalf("moved %d bytes with a 1 MiB cap", m)
	}
}

func TestHTTPLoaderReportsRefusal(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	l, _ := NewHTTPLoader(HTTPLoaderConfig{BaseURL: srv.URL, DownStreams: 2})
	defer l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := l.Download(ctx, func(int64) {})
	if !errors.Is(err, errNoLoad) {
		t.Fatalf("err = %v, want errNoLoad", err)
	}
	if ctx.Err() != nil {
		t.Fatal("a refusing server must fail fast, not run to the deadline")
	}
}

// An upload writes body bytes before the server's refusal comes back (Go's
// server even drains them). Those bytes must not turn a refused test into
// a "loaded" one.
func TestHTTPLoaderReportsRefusedUploads(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	l, _ := NewHTTPLoader(HTTPLoaderConfig{BaseURL: srv.URL, UpStreams: 2, UpChunk: 1 << 20})
	defer l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := l.Upload(ctx, func(int64) {})
	if !errors.Is(err, errNoLoad) || !strings.Contains(err.Error(), "429") {
		t.Fatalf("err = %v, want errNoLoad with the 429", err)
	}
	if ctx.Err() != nil {
		t.Fatal("a refusing server must fail fast, not run to the deadline")
	}
}

func TestHTTPLoaderOffline(t *testing.T) {
	t.Parallel()
	// A port nothing listens on: connection refused at once.
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	l, _ := NewHTTPLoader(HTTPLoaderConfig{BaseURL: url, UpStreams: 1})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := l.Upload(ctx, func(int64) {}); !errors.Is(err, errNoLoad) {
		t.Fatalf("err = %v", err)
	}
}

func TestHTTPLoaderEndpointDefaultsPort(t *testing.T) {
	t.Parallel()
	for u, want := range map[string]string{"https://speed.example": "speed.example:443", "http://[::1]": "[::1]:80", "https://h:8443/x": "h:8443"} {
		l, err := NewHTTPLoader(HTTPLoaderConfig{BaseURL: u})
		if err != nil {
			t.Fatal(err)
		}
		if l.Endpoint() != want {
			t.Errorf("%s: endpoint %q, want %q", u, l.Endpoint(), want)
		}
	}
}

func TestHTTPLoaderRejectsBadURL(t *testing.T) {
	t.Parallel()
	for _, u := range []string{"ftp://x", "not a url", "https://"} {
		if _, err := NewHTTPLoader(HTTPLoaderConfig{BaseURL: u}); err == nil {
			t.Errorf("%q accepted", u)
		}
	}
}
