package catalog

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
	"strings"
	"time"
)

// SteamAPI is where Steam's Web API lives. GetSDRConfig needs no key.
const SteamAPI = "https://api.steampowered.com"

// fetchTimeout bounds one download, DNS and TLS included. The config is
// about 30 KB; a link that can't move that in 15 s is as good as offline.
const fetchTimeout = 15 * time.Second

// HTTPDoer is the part of *http.Client the fetcher uses (tests pass their
// own).
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// SDRSource downloads an app's SDR network config from Steam.
type SDRSource struct {
	client HTTPDoer
	base   string
}

// NewSDRSource fetches from base (SteamAPI in the app, a test server in
// tests) with client; nil client: a client with fetchTimeout.
func NewSDRSource(client HTTPDoer, base string) *SDRSource {
	if client == nil {
		client = &http.Client{Timeout: fetchTimeout}
	}
	return &SDRSource{client: client, base: base}
}

// Fetch returns the raw config of a Steam app, checked to be a parseable SDR
// config.
func (s *SDRSource) Fetch(ctx context.Context, appID int) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	u := s.base + "/ISteamApps/GetSDRConfig/v1/?appid=" + url.QueryEscape(strconv.Itoa(appID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	res, err := s.client.Do(req)
	if err != nil {
		return nil, &FetchError{Err: err}
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
		return nil, &FetchError{Status: res.StatusCode}
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, maxConfigBytes+1))
	if err != nil {
		return nil, &FetchError{Err: err}
	}
	if _, err := ParseSDRConfig(appID, b); err != nil {
		if !errors.Is(err, ErrNotSDR) {
			// HTML from a captive portal, a proxy's error page, a cut-off
			// body: say what it was, not the JSON decoder's words.
			err = fmt.Errorf("%w: %w", errBadReply, err)
		}
		return nil, &FetchError{Err: err}
	}
	return b, nil
}

// errBadReply marks a 200 reply that isn't a usable SDR config.
var errBadReply = errors.New("not a server list")

// FetchError is a failed download, worded for the player.
type FetchError struct {
	Status int   // HTTP status when Steam answered with an error
	Err    error // otherwise what went wrong
}

func (e *FetchError) Unwrap() error { return e.Err }

func (e *FetchError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("Steam answered HTTP %d", e.Status)
	}
	var dns *net.DNSError
	var nerr net.Error
	switch {
	case errors.Is(e.Err, ErrNotSDR):
		return "Steam sent no server list for this game"
	case errors.Is(e.Err, errBadReply):
		return "the reply wasn't Steam's server list (a Wi-Fi login page, proxy or filter may be in the way)"
	case errors.As(e.Err, &dns):
		return "can't look up Steam's address (offline, or DNS isn't answering)"
	case errors.Is(e.Err, context.DeadlineExceeded) || (errors.As(e.Err, &nerr) && nerr.Timeout()):
		return "Steam didn't answer in time"
	case errors.Is(e.Err, context.Canceled):
		return "cancelled"
	}
	var op *net.OpError
	var cert *tls.CertificateVerificationError
	switch {
	case errors.As(e.Err, &op) && op.Op == "proxyconnect":
		return "can't reach the proxy set on this PC"
	case errors.As(e.Err, &op) && op.Op == "dial":
		return "can't connect to Steam (offline, or a firewall is blocking Lowpath)"
	case errors.As(e.Err, &cert):
		return "Steam's certificate wasn't trusted (an antivirus or proxy may be inspecting HTTPS)"
	}
	// The rest in Go's words, without the request URL *url.Error adds.
	err := e.Err
	var uerr *url.Error
	if errors.As(err, &uerr) {
		err = uerr.Err
	}
	return "can't reach Steam: " + strings.TrimSuffix(err.Error(), ".")
}
