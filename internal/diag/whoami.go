// Package diag finds out where lag comes from on the user's side of the
// connection: who the user's ISP is, what their link is doing, and so on.
// Every check fails open: a measurement that can't be made comes back as
// "unknown" with a reason, never as an invented number.
package diag

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"lowpath/internal/stun"
)

// Family picks IPv4 or IPv6.
type Family string

const (
	IPv4 Family = "ipv4"
	IPv6 Family = "ipv6"
)

// Of returns the family of a.
func Of(a netip.Addr) Family {
	if a.Unmap().Is4() {
		return IPv4
	}
	return IPv6
}

// PublicAddr is the address the internet sees this PC as, and who said so.
type PublicAddr struct {
	Addr   netip.Addr
	Source string // e.g. "stun stun.l.google.com:19302"
}

// AddrFinder learns this PC's public address in one family.
type AddrFinder interface {
	PublicAddr(ctx context.Context, f Family) (PublicAddr, error)
}

// NetworkInfo is what the routing registry knows about a public address.
// Empty fields are unknown.
type NetworkInfo struct {
	ASN     uint32       `json:"asn,omitempty"`
	ASNs    []uint32     `json:"asns,omitempty"` // every origin AS (a prefix can have several)
	Holder  string       `json:"holder,omitempty"`
	ASName  string       `json:"as_name,omitempty"` // the AS handle, e.g. "OMANTEL-NAP-AS"
	ISP     string       `json:"isp,omitempty"`     // the holder's name, e.g. "Oman Telecommunications Company (S.A.O.G)"
	Prefix  netip.Prefix `json:"prefix,omitzero"`
	Country string       `json:"country,omitempty"` // ISO 3166 alpha-2
	City    string       `json:"city,omitempty"`
	// Partial says that part of the lookup failed (say, the city); what is
	// filled in is still right.
	Partial string `json:"partial,omitempty"`
}

// NetworkLookup maps a public address to its network.
type NetworkLookup interface {
	LookupNetwork(ctx context.Context, a netip.Addr) (NetworkInfo, error)
}

// Endpoint is the public address of one family with its network.
type Endpoint struct {
	IP     netip.Addr `json:"ip"`
	Source string     `json:"source"`
	NetworkInfo
	// LookupError says why the network is unknown; "" when it is known.
	LookupError string `json:"lookup_error,omitempty"`
}

// Location states.
const (
	StateOK      = "ok"      // public address and its network are known
	StatePartial = "partial" // public address known, its network (partly) not
	StateOffline = "offline" // no public address: no internet, or STUN and HTTPS both blocked
)

// Location answers "where am I on the internet": the public IPv4 and IPv6
// addresses and the ISP, AS and city behind them.
type Location struct {
	State   string    `json:"state"`
	Message string    `json:"message,omitempty"` // a plain-language reason when State is not ok
	At      time.Time `json:"at"`
	IPv4    *Endpoint `json:"ipv4"`
	IPv6    *Endpoint `json:"ipv6"`
	// IPv6Note says why there is no IPv6 address (usually: the connection
	// has none, which is common and fine).
	IPv6Note string `json:"ipv6_note,omitempty"`
}

// Redacted is the location with the host part of each address cleared
// (IPv4 to /24, IPv6 to /48), for reports a user may hand to someone else.
func (l Location) Redacted() Location {
	red := func(e *Endpoint, bits int) *Endpoint {
		if e == nil {
			return nil
		}
		c := *e
		if p, err := c.IP.Prefix(bits); err == nil {
			c.IP = p.Addr()
		}
		return &c
	}
	l.IPv4, l.IPv6 = red(l.IPv4, 24), red(l.IPv6, 48)
	return l
}

// LocatorConfig wires a Locator. Finders are tried in order per family:
// the first one that answers wins. Lookup may be nil (addresses only).
type LocatorConfig struct {
	Finders []AddrFinder
	Lookup  NetworkLookup
	// MaxAge is how long a result is reused before Locate looks again
	// (default 2 min). Refresh ignores it.
	MaxAge time.Duration
	// Timeout bounds one whole lookup (default 12 s): addresses, then the
	// network of each.
	Timeout time.Duration
	Now     func() time.Time
}

// Locator finds the user's public addresses and their network. Locate calls
// share one lookup at a time, and a fresh result is reused, so the UI can
// ask as often as it likes.
type Locator struct {
	finders []AddrFinder
	lookup  NetworkLookup
	maxAge  time.Duration
	timeout time.Duration
	now     func() time.Time

	mu       sync.Mutex
	last     *Location
	inflight chan struct{} // closed when the running lookup ends
}

func NewLocator(cfg LocatorConfig) *Locator {
	l := &Locator{finders: cfg.Finders, lookup: cfg.Lookup, maxAge: cfg.MaxAge, timeout: cfg.Timeout, now: cfg.Now}
	if l.maxAge <= 0 {
		l.maxAge = 2 * time.Minute
	}
	if l.timeout <= 0 {
		l.timeout = 12 * time.Second
	}
	if l.now == nil {
		l.now = time.Now
	}
	return l
}

// Last returns the latest result without looking anything up.
func (l *Locator) Last() (Location, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last == nil {
		return Location{}, false
	}
	return *l.last, true
}

// Locate returns the location, looking it up when there is no result
// younger than MaxAge or refresh is set. A lookup keeps running when ctx
// ends (another caller may be waiting for it, and the result is cached);
// Locate then returns ctx's error.
func (l *Locator) Locate(ctx context.Context, refresh bool) (Location, error) {
	l.mu.Lock()
	if !refresh && l.last != nil && l.now().Sub(l.last.At) < l.maxAge {
		loc := *l.last
		l.mu.Unlock()
		return loc, nil
	}
	wait := l.inflight
	if wait == nil {
		wait = make(chan struct{})
		l.inflight = wait
		go l.run(wait)
	}
	l.mu.Unlock()
	select {
	case <-wait:
	case <-ctx.Done():
		return Location{}, ctx.Err()
	}
	loc, _ := l.Last()
	return loc, nil
}

func (l *Locator) run(done chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), l.timeout)
	defer cancel()
	loc := l.locate(ctx)
	l.mu.Lock()
	l.last = &loc
	l.inflight = nil
	l.mu.Unlock()
	close(done)
}

func (l *Locator) locate(ctx context.Context) Location {
	var v4, v6 *Endpoint
	var err4, err6 error
	var wg sync.WaitGroup
	wg.Go(func() { v4, err4 = l.endpoint(ctx, IPv4) })
	wg.Go(func() { v6, err6 = l.endpoint(ctx, IPv6) })
	wg.Wait()

	loc := Location{At: l.now(), IPv4: v4, IPv6: v6}
	if v6 == nil {
		loc.IPv6Note = "This connection has no public IPv6 address (common, and fine for Lowpath)."
		if err6 != nil && !onlyNoRoute(err6) {
			loc.IPv6Note = "No public IPv6 address found: " + describe(err6)
		}
	}
	// The state follows the address games use: IPv4 (all Lowpath routes
	// today), or IPv6 on an IPv6-only line. A failed lookup of the other
	// one stays in its own lookup_error.
	main := v4
	if main == nil {
		main = v6
	}
	switch {
	case main == nil:
		loc.State = StateOffline
		loc.Message = "Couldn't find your public IP address: " + describe(err4) +
			". Check that you are online; firewalls that block UDP and HTTPS also cause this."
	case main.LookupError == "":
		loc.State = StateOK
	default:
		loc.State = StatePartial
		loc.Message = "Your ISP couldn't be looked up: " + main.LookupError
	}
	return loc
}

// endpoint finds the public address of one family, then its network.
func (l *Locator) endpoint(ctx context.Context, f Family) (*Endpoint, error) {
	var pa PublicAddr
	var errs []error
	for _, fd := range l.finders {
		got, err := fd.PublicAddr(ctx, f)
		if err == nil && got.Addr.IsValid() && Of(got.Addr) == f {
			pa = got
			break
		}
		if err == nil {
			err = errNoAnswer
		}
		errs = append(errs, err)
		if ctx.Err() != nil {
			break
		}
	}
	if !pa.Addr.IsValid() {
		if len(errs) == 0 {
			return nil, errNoAnswer
		}
		return nil, errors.Join(errs...)
	}
	e := &Endpoint{IP: pa.Addr.Unmap(), Source: pa.Source}
	switch {
	case !isPublic(e.IP):
		// A STUN server on the LAN, or an unusual NAT: nothing to look up.
		e.LookupError = "the address the server saw (" + e.IP.String() + ") is not a public internet address"
	case l.lookup == nil:
		e.LookupError = "ISP lookup is off"
	default:
		ni, err := l.lookup.LookupNetwork(ctx, e.IP)
		if err != nil {
			e.LookupError = describe(err)
		} else {
			e.NetworkInfo = ni
		}
	}
	return e, nil
}

// errNoAnswer: a finder had nothing for this family (for example a host
// with no IPv6 connectivity).
var errNoAnswer = errors.New("no answer")

// describe turns lookup errors into short text for the UI.
func describe(err error) string {
	if err == nil {
		return "unknown error"
	}
	var dnsErr *net.DNSError
	switch {
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, stun.ErrTimeout):
		return "no reply in time"
	case errors.As(err, &dnsErr):
		return "couldn't resolve " + dnsErr.Name + " (DNS)"
	case errors.Is(err, errNoAnswer):
		return "no answer"
	}
	// errors.Join gives one line per finder; the first is the most telling.
	msg, _, _ := strings.Cut(err.Error(), "\n")
	return msg
}

// cgnat is RFC 6598 shared address space, which netip doesn't flag.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

func isPublic(a netip.Addr) bool {
	return a.IsGlobalUnicast() && !a.IsPrivate() && !cgnat.Contains(a)
}

// STUNFinder asks public STUN servers for the address they see. All
// servers are asked at once and the first answer wins, so one slow or
// blocked server costs nothing when another answers.
type STUNFinder struct {
	client  *stun.Client
	servers []string
	timeout time.Duration
}

// DefaultSTUNServers are public STUN servers on UDP. Each name resolves to
// IPv4 and IPv6 addresses.
func DefaultSTUNServers() []string {
	return []string{"stun.l.google.com:19302", "stun.cloudflare.com:3478"}
}

// NewSTUNFinder asks servers ("host:port") with client. timeout bounds one
// PublicAddr call, DNS included (0: 5 s), so a slow resolver leaves time for
// the next finder.
func NewSTUNFinder(client *stun.Client, servers []string, timeout time.Duration) *STUNFinder {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &STUNFinder{client: client, servers: slices.Clone(servers), timeout: timeout}
}

func (s *STUNFinder) PublicAddr(ctx context.Context, f Family) (PublicAddr, error) {
	if len(s.servers) == 0 {
		return PublicAddr{}, errors.New("no STUN servers configured")
	}
	network := "udp4"
	if f == IPv6 {
		network = "udp6"
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	type result struct {
		server string
		b      stun.Binding
		err    error
	}
	results := make(chan result, len(s.servers))
	for _, srv := range s.servers {
		go func() {
			b, err := s.client.Bind(ctx, network, srv)
			results <- result{srv, b, err}
		}()
	}
	var errs []error
	for range s.servers {
		r := <-results
		if r.err == nil && r.b.Mapped.IsValid() {
			return PublicAddr{Addr: r.b.Mapped.Addr(), Source: "stun " + r.server}, nil
		}
		if r.err == nil {
			r.err = stun.ErrNoAddress
		}
		errs = append(errs, fmt.Errorf("%s: %w", r.server, r.err))
	}
	return PublicAddr{}, errors.Join(errs...)
}

// onlyNoRoute reports whether every error joined in err says this host has
// no route, address or answer in that family, which is how "this connection
// has no IPv6" looks.
func onlyNoRoute(err error) bool {
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		for _, e := range j.Unwrap() {
			if !onlyNoRoute(e) {
				return false
			}
		}
		return true
	}
	return isLeafNoRoute(err)
}

func isLeafNoRoute(err error) bool {
	var dnsErr *net.DNSError
	var opErr *net.OpError
	switch {
	case errors.Is(err, errNoAnswer), errors.Is(err, stun.ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		// A host with only link-local IPv6 may "send" and never hear back.
		return true
	case errors.As(err, &dnsErr):
		return true
	case errors.As(err, &opErr) && opErr.Op == "dial":
		return true
	}
	return false
}

// NewInternetLocator is the Locator the app uses: STUN first (what a game's
// UDP traffic would see), RIPEstat's whats-my-ip over HTTPS when UDP is
// blocked, and RIPEstat for the network, cached.
func NewInternetLocator() *Locator {
	ripe := NewRIPEstat(RIPEstatOptions{})
	return NewLocator(LocatorConfig{
		Finders: []AddrFinder{NewSTUNFinder(stun.NewClient(stun.ClientOptions{}), DefaultSTUNServers(), 0), ripe},
		Lookup:  NewCachedLookup(ripe, CacheOptions{}),
	})
}
