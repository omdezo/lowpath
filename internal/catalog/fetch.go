package catalog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
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
		return nil, &FetchError{Err: err}
	}
	return b, nil
}

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
	case errors.As(e.Err, &dns):
		return "can't look up Steam's address (offline, or DNS isn't answering)"
	case errors.Is(e.Err, context.DeadlineExceeded) || (errors.As(e.Err, &nerr) && nerr.Timeout()):
		return "Steam didn't answer in time"
	case errors.Is(e.Err, context.Canceled):
		return "cancelled"
	default:
		return "can't reach Steam: " + e.Err.Error()
	}
}
