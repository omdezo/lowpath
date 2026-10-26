package diag

import (
	"context"
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
type RIPEstat struct {
	base      string
	sourceApp string
	clients   map[Family]*http.Client
}

// RIPEstatOptions tune RIPEstat; zero fields take the defaults.
type RIPEstatOptions struct {
	BaseURL string // default "https://stat.ripe.net"
	// SourceApp names the caller to RIPE NCC, as the API asks of
	// regular users. Default "lowpath".
	SourceApp string
	Timeout   time.Duration // per request, default 6 s
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
	r := &RIPEstat{base: strings.TrimRight(o.BaseURL, "/"), sourceApp: o.SourceApp, clients: map[Family]*http.Client{}}
	for f, network := range map[Family]string{IPv4: "tcp4", IPv6: "tcp6", anyFamily: "tcp"} {
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
		r.clients[f] = &http.Client{Transport: t, Timeout: o.Timeout}
	}
	return r
}

// anyFamily lets a request use whichever family reaches RIPEstat.
const anyFamily Family = ""

// ripeResponse is the envelope every data call shares.
type ripeResponse[T any] struct {
	Status   string          `json:"status"`
	Messages json.RawMessage `json:"messages"` // [[level, text], ...]
	Data     T               `json:"data"`
}

// call fetches /data/<name>/data.json?resource=<resource> over family f.
func call[T any](ctx context.Context, r *RIPEstat, f Family, name, resource string) (T, error) {
	var zero T
	q := url.Values{"sourceapp": {r.sourceApp}}
	if resource != "" {
		q.Set("resource", resource)
	}
	u := r.base + "/data/" + name + "/data.json?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return zero, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := r.clients[f].Do(req)
	if err != nil {
		// A *url.Error repeats the URL, whose query holds the address being
		// looked up; that text reaches the UI and the diagnostics report.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return zero, fmt.Errorf("RIPEstat %s: %w", name, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return zero, fmt.Errorf("RIPEstat %s: %w", name, err)
	}
	if resp.StatusCode != http.StatusOK {
		return zero, fmt.Errorf("RIPEstat %s: HTTP %d", name, resp.StatusCode)
	}
	var out ripeResponse[T]
	if err := json.Unmarshal(body, &out); err != nil {
		return zero, fmt.Errorf("RIPEstat %s: unreadable reply: %w", name, err)
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
		return zero, fmt.Errorf("RIPEstat %s: %s", name, msg)
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
	var country, city string
	wg.Go(func() {
		country, city, geoErr = r.geo(ctx, f, a)
	})
	wg.Wait()
	info.Country, info.City = country, city
	var partial []string
	if asErr != nil {
		partial = append(partial, "ISP name unknown ("+asErr.Error()+")")
	}
	if geoErr != nil {
		partial = append(partial, "city unknown ("+geoErr.Error()+")")
	}
	info.Partial = strings.Join(partial, "; ")
	return info, nil
}

// geo picks the location covering most of the address's space. City is ""
// when GeoLite only knows the country, which is common.
func (r *RIPEstat) geo(ctx context.Context, f Family, a netip.Addr) (country, city string, err error) {
	g, err := call[struct {
		Located []struct {
			Locations []struct {
				Country string  `json:"country"`
				City    string  `json:"city"`
				Covered float64 `json:"covered_percentage"`
			} `json:"locations"`
		} `json:"located_resources"`
	}](ctx, r, f, "maxmind-geo-lite", a.String())
	if err != nil {
		return "", "", err
	}
	best := -1.0
	for _, lr := range g.Located {
		for _, l := range lr.Locations {
			if l.Covered > best && (l.Country != "" || l.City != "") {
				best, country, city = l.Covered, strings.TrimSpace(l.Country), strings.TrimSpace(l.City)
			}
		}
	}
	return country, city, nil
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
