package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"lowpath/internal/engine"
	"lowpath/internal/relay"
)

const (
	loopbackID = "loopback"
	// defaultRelayPort is what cmd/relay and deploy-relay.ps1 listen on, so
	// an address typed without a port means that one.
	defaultRelayPort = 40000
	// relayTestFor is how long POST /api/relays/test probes (plus the
	// engine's 1 s grace for late replies).
	relayTestFor = 1500 * time.Millisecond
)

// RelayView is a relay as the UI sees it. Keys never leave the service.
type RelayView struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Region    string `json:"region"`
	Continent string `json:"continent"`
	Addr      string `json:"addr"`
	Egress    string `json:"egress"` // id of the egress relay it forwards to; "" = it is an egress
	HasKey    bool   `json:"has_key"`
	Builtin   bool   `json:"builtin"` // loopback test relay or -relays import: not deletable here
}

// RelayNetwork is the real (non-demo) network: the user's relays from the
// store, relays imported with -relays, and the optional loopback test relay.
// Every relay can carry every game.
type RelayNetwork struct {
	store *Store
	imp   *RelayImport // nil without -relays
	log   *slog.Logger

	mu   sync.Mutex
	loop *relay.Egress // nil unless settings.loopback_relay is on
}

// NewRelayNetwork starts the loopback test relay right away when the setting
// is on.
func NewRelayNetwork(store *Store, imp *RelayImport, log *slog.Logger) (*RelayNetwork, error) {
	if log == nil {
		log = slog.Default()
	}
	n := &RelayNetwork{store: store, imp: imp, log: log}
	if store.Settings().LoopbackRelay {
		if err := n.SetLoopback(true); err != nil {
			return nil, err
		}
	}
	return n, nil
}

func (n *RelayNetwork) Demo() bool { return false }

func (n *RelayNetwork) Paths(string) []engine.PathSpec {
	var out []engine.PathSpec
	for _, e := range n.entries() {
		out = append(out, e.spec)
	}
	return out
}

// Endpoint is the game's server for proxy mode and the Analyze baseline: the
// one named in the -relays import, else the last one route mode saw.
func (n *RelayNetwork) Endpoint(gameID string) (netip.AddrPort, netip.AddrPort, bool) {
	if n.imp != nil {
		if g, ok := n.imp.Games[gameID]; ok && g.Server.IsValid() {
			return g.Server, g.Server, true
		}
	}
	if s := n.store.Servers(gameID); len(s) > 0 {
		return s[0], s[0], true
	}
	return netip.AddrPort{}, netip.AddrPort{}, false
}

// SetLoopback starts or stops the built-in test relay: an open egress on
// 127.0.0.1 inside this process. Traffic through it leaves from this PC, so
// it proves the capture → tunnel → inject pipeline without making anything
// faster.
func (n *RelayNetwork) SetLoopback(on bool) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if on == (n.loop != nil) {
		return nil
	}
	if !on {
		err := n.loop.Close()
		n.loop = nil
		n.log.Info("loopback test relay stopped")
		return err
	}
	e, err := relay.ListenEgress("127.0.0.1:0", n.log.With("component", "loopback-relay"))
	if err != nil {
		return fmt.Errorf("loopback test relay: %w", err)
	}
	go e.Serve(context.Background()) // returns when Close is called
	n.loop = e
	n.log.Info("loopback test relay started", "addr", e.Addr())
	return nil
}

func (n *RelayNetwork) Close() { n.SetLoopback(false) }

type relayEntry struct {
	view RelayView
	spec engine.PathSpec
}

// entries lists the loopback relay, then imported, then stored relays.
func (n *RelayNetwork) entries() []relayEntry {
	var out []relayEntry
	n.mu.Lock()
	if n.loop != nil {
		out = append(out, newEntry(RelayView{ID: loopbackID, Label: "Loopback test relay", Region: "This PC", Continent: "Local", Builtin: true}, n.loop.Addr(), ""))
	}
	n.mu.Unlock()
	if n.imp != nil {
		for _, r := range n.imp.Relays {
			out = append(out, newEntry(RelayView{ID: r.ID, Label: r.Label, Region: r.Region, Continent: r.Continent, Egress: r.Egress, Builtin: true}, r.Addr, r.Key))
		}
	}
	for _, r := range n.store.Relays() {
		out = append(out, newEntry(RelayView{ID: r.ID, Label: r.Label, Region: r.Region, Continent: r.Continent, Egress: r.Egress}, r.Addr, r.Key))
	}
	return out
}

func newEntry(v RelayView, addr netip.AddrPort, key string) relayEntry {
	v.Addr = addr.String()
	v.HasKey = key != ""
	// The engine only duplicates packets inside one egress group: an egress
	// relay is its own group, a forwarding hop joins its egress's.
	group := v.Egress
	if group == "" {
		group = v.ID
	}
	return relayEntry{
		view: v,
		spec: engine.PathSpec{ID: v.ID, Label: v.Label, Region: v.Region, Continent: v.Continent, Addr: addr, Key: key, Egress: group},
	}
}

func (n *RelayNetwork) Views() []RelayView {
	out := []RelayView{}
	for _, e := range n.entries() {
		out = append(out, e.view)
	}
	return out
}

// Endpoints are the addresses of every relay, which capture must never take.
func (n *RelayNetwork) Endpoints() []netip.AddrPort {
	var out []netip.AddrPort
	for _, e := range n.entries() {
		out = append(out, e.spec.Addr)
	}
	return out
}

func (n *RelayNetwork) find(id string) (relayEntry, bool) {
	for _, e := range n.entries() {
		if e.view.ID == id {
			return e, true
		}
	}
	return relayEntry{}, false
}

// taken reports ids the store must not hand out: the loopback relay's
// (even while it is off) and the imported relays'.
func (n *RelayNetwork) taken(id string) bool {
	if id == loopbackID {
		return true
	}
	if n.imp != nil {
		for _, r := range n.imp.Relays {
			if r.ID == id {
				return true
			}
		}
	}
	return false
}

// --- HTTP ------------------------------------------------------------------

func (s *Server) handleRelays(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"relays": s.relays.Views()})
}

type relayReq struct {
	Label     string `json:"label"`
	Region    string `json:"region"`
	Continent string `json:"continent"`
	Addr      string `json:"addr"`
	Key       string `json:"key"`
	Egress    string `json:"egress"`
}

func (s *Server) handleAddRelay(w http.ResponseWriter, r *http.Request) {
	var req relayReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, "bad_request", err)
		return
	}
	sr, err := s.relays.validate(r.Context(), req)
	if err != nil {
		writeErr(w, 400, "bad_relay", err)
		return
	}
	saved, err := s.store.AddRelay(sr, s.relays.taken)
	if errors.Is(err, ErrRelayExists) {
		writeErr(w, 409, "relay_exists", err)
		return
	}
	if err != nil {
		writeErr(w, 500, "failed", err)
		return
	}
	s.log.Info("relay added", "id", saved.ID, "addr", saved.Addr, "egress", saved.Egress, "key", saved.Key != "")
	e, _ := s.relays.find(saved.ID)
	writeJSON(w, 200, e.view)
}

// validate checks a new relay and resolves its address.
func (n *RelayNetwork) validate(ctx context.Context, req relayReq) (StoredRelay, error) {
	r := StoredRelay{
		Label:     strings.TrimSpace(req.Label),
		Region:    strings.TrimSpace(req.Region),
		Continent: strings.TrimSpace(req.Continent),
		Host:      strings.TrimSpace(req.Addr),
		Key:       strings.TrimSpace(req.Key),
		Egress:    strings.TrimSpace(req.Egress),
		CreatedAt: time.Now().UTC(),
	}
	switch {
	case len(r.Label) > 64:
		return r, errors.New("label is too long (64 characters at most)")
	case len(r.Region) > 48 || len(r.Continent) > 48:
		return r, errors.New("region and continent must be 48 characters at most")
	case len(r.Key) > 256:
		return r, errors.New("key is too long (256 characters at most)")
	}
	if r.Egress != "" {
		e, ok := n.find(r.Egress)
		if !ok || e.view.ID == loopbackID {
			return r, fmt.Errorf("unknown egress relay %q", r.Egress)
		}
		if e.view.Egress != "" {
			return r, fmt.Errorf("%s forwards to another relay itself; pick the egress relay it ends at", e.view.Label)
		}
	}
	var err error
	if r.Addr, err = resolveRelayAddr(ctx, r.Host); err != nil {
		return r, err
	}
	if r.Label == "" {
		r.Label = r.Host
	}
	r.ID = slug(r.Label, 32)
	if r.ID == "" {
		r.ID = "relay"
	}
	return r, nil
}

// resolveRelayAddr turns "host[:port]" into an address, preferring IPv4
// (capture is IPv4-only, and so is most of the internet's game traffic).
func resolveRelayAddr(ctx context.Context, s string) (netip.AddrPort, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return netip.AddrPort{}, errors.New("enter the relay's address, e.g. 203.0.113.10:40000")
	}
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		host, port = strings.Trim(s, "[]"), strconv.Itoa(defaultRelayPort)
	}
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil || p == 0 {
		return netip.AddrPort{}, fmt.Errorf("bad port %q (1-65535)", port)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		ips, lerr := net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
		if lerr != nil || len(ips) == 0 {
			ips, lerr = net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
		if lerr != nil || len(ips) == 0 {
			return netip.AddrPort{}, fmt.Errorf("can't resolve %q: %v", host, lerr)
		}
		ip = ips[0]
	}
	ip = ip.Unmap().WithZone("")
	if ip.IsUnspecified() || ip.IsMulticast() {
		return netip.AddrPort{}, fmt.Errorf("%s is not a relay address", ip)
	}
	return netip.AddrPortFrom(ip, uint16(p)), nil
}

func (s *Server) handleDeleteRelay(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == loopbackID {
		writeErr(w, 400, "builtin", errors.New("turn the loopback test relay off in Settings instead"))
		return
	}
	if s.relays.taken(id) {
		writeErr(w, 400, "builtin", errors.New("this relay comes from the -relays file; edit that file instead"))
		return
	}
	switch err := s.store.DeleteRelay(id); {
	case errors.Is(err, ErrNotFound):
		writeErr(w, 404, "not_found", errors.New("unknown relay"))
	case errors.Is(err, ErrRelayInUse):
		writeErr(w, 409, "relay_in_use", err)
	case err != nil:
		writeErr(w, 500, "failed", err)
	default:
		s.log.Info("relay deleted", "id", id)
		writeJSON(w, 200, map[string]bool{"ok": true})
	}
}

type relayTest struct {
	OK      bool    `json:"ok"`
	RTT     float64 `json:"rtt_ms"`
	Loss    float64 `json:"loss_pct"`
	Message string  `json:"message"`
}

// handleTestRelay probes one relay (saved, or not yet saved: addr + key) the
// way Analyze does, so a wrong key shows up as "no reply" before Apply.
func (s *Server) handleTestRelay(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID   string `json:"id"`
		Addr string `json:"addr"`
		Key  string `json:"key"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, "bad_request", err)
		return
	}
	var spec engine.PathSpec
	if req.ID != "" {
		e, ok := s.relays.find(req.ID)
		if !ok {
			writeErr(w, 404, "not_found", errors.New("unknown relay"))
			return
		}
		spec = e.spec
	} else {
		a, err := resolveRelayAddr(r.Context(), req.Addr)
		if err != nil {
			writeErr(w, 400, "bad_relay", err)
			return
		}
		spec = engine.PathSpec{ID: "test", Label: a.String(), Addr: a, Key: strings.TrimSpace(req.Key)}
	}
	res, err := engine.Analyze(r.Context(), []engine.PathSpec{spec}, engine.AnalyzeOptions{}, relayTestFor)
	if err != nil {
		writeErr(w, 500, "test_failed", err)
		return
	}
	st := res.Paths[spec.ID]
	out := relayTest{OK: st.Samples > 0, RTT: st.RTT, Loss: st.Loss}
	if out.OK {
		rtt := fmt.Sprintf("%.1f ms", st.RTT)
		if st.RTT < 0.1 {
			rtt = "under 0.1 ms" // loopback; the clock can't resolve it
		}
		out.Message = fmt.Sprintf("Relay answered: %s, %.0f%% loss", rtt, st.Loss)
	} else {
		out.Message = fmt.Sprintf("No reply from %s. Check the address and the access key, and that UDP port %d is open on the server.", spec.Addr, spec.Addr.Port())
	}
	writeJSON(w, 200, out)
}

// slug lower-cases s and keeps letters and digits, joining runs of anything
// else with single dashes.
func slug(s string, max int) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(r)
			dash = false
		} else {
			dash = true
		}
	}
	out := b.String() // ASCII only, so cutting at a byte offset is safe
	if len(out) > max {
		out = out[:max]
	}
	return strings.TrimRight(out, "-")
}
