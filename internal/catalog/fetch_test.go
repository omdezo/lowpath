package catalog

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFetchConfig(t *testing.T) {
	fixture := loadFixture(t)
	var gotPath, gotApp string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotApp = r.URL.Path, r.URL.Query().Get("appid")
		w.Write(fixture)
	}))
	defer ts.Close()
	b, err := NewSDRSource(nil, ts.URL).Fetch(context.Background(), 730)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/ISteamApps/GetSDRConfig/v1/" || gotApp != "730" {
		t.Fatalf("asked for %s ?appid=%s", gotPath, gotApp)
	}
	if string(b) != string(fixture) {
		t.Fatal("body changed on the way")
	}
}

func TestFetchErrors(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{"server error", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "busy", 503) }, "HTTP 503"},
		{"no config", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"success":false,"message":"Failed to get appinfo"}`))
		}, "no server list"},
		{"html", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html>captive portal</html>")) }, "Wi-Fi login page"},
		{"empty object", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) }, "no server list"},
		{"too big", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"pops":{"a":{"desc":"` + strings.Repeat("x", maxConfigBytes) + `"}}}`))
		}, "wasn't Steam's server list"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ts := httptest.NewServer(c.handler)
			defer ts.Close()
			_, err := NewSDRSource(nil, ts.URL).Fetch(context.Background(), 730)
			var fe *FetchError
			if !errors.As(err, &fe) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want a FetchError saying %q", err, c.want)
			}
		})
	}
}

func TestFetchTimeout(t *testing.T) {
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer ts.Close()
	defer close(release)
	start := time.Now()
	_, err := NewSDRSource(&http.Client{Timeout: 200 * time.Millisecond}, ts.URL).Fetch(context.Background(), 730)
	if err == nil || !strings.Contains(err.Error(), "in time") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("timeout not honored")
	}
}

func TestFetchErrorWording(t *testing.T) {
	dns := &FetchError{Err: &net.DNSError{Err: "no such host", Name: "api.steampowered.com", IsNotFound: true}}
	if !strings.Contains(dns.Error(), "offline") {
		t.Errorf("DNS failure: %q", dns.Error())
	}
	refused := &FetchError{Err: &url.Error{Op: "Get", URL: "https://api.steampowered.com/x", Err: &net.OpError{Op: "dial", Err: errors.New("connection refused")}}}
	if got := refused.Error(); !strings.HasPrefix(got, "can't connect to Steam") || strings.Contains(got, "https://") {
		t.Errorf("refused: %q", got)
	}
	proxy := &FetchError{Err: &url.Error{Op: "Get", URL: "https://api.steampowered.com/x", Err: &net.OpError{Op: "proxyconnect", Err: errors.New("connection refused")}}}
	if got := proxy.Error(); !strings.Contains(got, "proxy") {
		t.Errorf("proxy: %q", got)
	}
	cert := &FetchError{Err: &url.Error{Op: "Get", URL: "https://api.steampowered.com/x", Err: &tls.CertificateVerificationError{Err: errors.New("x509: unknown authority")}}}
	if got := cert.Error(); !strings.Contains(got, "certificate") {
		t.Errorf("certificate: %q", got)
	}
	other := &FetchError{Err: &url.Error{Op: "Get", URL: "https://api.steampowered.com/x", Err: errors.New("unexpected EOF.")}}
	if got := other.Error(); got != "can't reach Steam: unexpected EOF" {
		t.Errorf("other: %q", got)
	}
	if c := (&FetchError{Err: context.Canceled}); c.Error() != "cancelled" {
		t.Errorf("cancelled: %q", c.Error())
	}
}

func TestDirCache(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "catalog")
	c := NewDirCache(dir)
	if _, _, err := c.Load("sdr-730"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty cache: %v", err)
	}
	if err := c.Save("sdr-730", []byte(`{"x":1}`)); err != nil {
		t.Fatal(err)
	}
	b, at, err := c.Load("sdr-730")
	if err != nil || string(b) != `{"x":1}` || time.Since(at) > time.Minute {
		t.Fatalf("load = %q %v %v", b, at, err)
	}
	for _, k := range []string{"../evil", "", "A", strings.Repeat("a", 65)} {
		if err := c.Save(k, nil); err == nil {
			t.Errorf("key %q accepted", k)
		}
	}
	// A second save replaces the first, and no temp file is left behind.
	if err := c.Save("sdr-730", []byte(`{"x":2}`)); err != nil {
		t.Fatal(err)
	}
	if b, _, _ := c.Load("sdr-730"); string(b) != `{"x":2}` {
		t.Fatalf("after a second save: %q", b)
	}
	if tmp, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(tmp) != 0 {
		t.Errorf("temp files left behind: %v", tmp)
	}
}
