package catalog

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"time"
)

// Source downloads an app's raw SDR config (SDRSource in the app).
type Source interface {
	Fetch(ctx context.Context, appID int) ([]byte, error)
}

// Prober pings a list of POPs (POPProber in the app).
type Prober interface {
	Probe(ctx context.Context, pops []POP, done func(id string, r POPPing))
}

// ValveOptions wires ValveServers. Source and Prober are required.
type ValveOptions struct {
	Source Source
	Prober Prober
	Cache  Cache // nil: nothing kept between runs
	Now    func() time.Time
	Log    *slog.Logger

	// TTL is how long a downloaded list counts as current. Valve's own game
	// coordinator checks for a new config about once an hour. Zero: 1 h.
	TTL time.Duration
	// PingTTL is how old the pings may get before opening the game page
	// measures again. Zero: 5 min.
	PingTTL time.Duration
}

const (
	retryMin = 30 * time.Second
	retryMax = 15 * time.Minute
)

// ValveServers keeps the SDR POP list of each Valve game current and pinged.
// Nothing it does blocks the caller: List answers from memory at once and
// starts a download or a ping round in the background when one is due.
type ValveServers struct {
	src     Source
	prober  Prober
	cache   Cache
	now     func() time.Time
	log     *slog.Logger
	ttl     time.Duration
	pingTTL time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu   sync.Mutex
	apps map[int]*sdrApp
}

// sdrApp is what is known about one app's POPs.
type sdrApp struct {
	id        int
	loaded    bool // the disk cache was read
	cfg       *SDRConfig
	fetchedAt time.Time
	fetching  bool
	lastErr   error
	nextTry   time.Time
	backoff   time.Duration
	pings     map[string]POPPing
	pinging   bool
	pingedAt  time.Time // when the last ping round ended
}

func NewValveServers(o ValveOptions) *ValveServers {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.TTL <= 0 {
		o.TTL = time.Hour
	}
	if o.PingTTL <= 0 {
		o.PingTTL = 5 * time.Minute
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &ValveServers{
		src: o.Source, prober: o.Prober, cache: o.Cache, now: o.Now, log: o.Log,
		ttl: o.TTL, pingTTL: o.PingTTL, ctx: ctx, cancel: cancel, apps: map[int]*sdrApp{},
	}
}

// Close stops downloads and ping rounds and waits for them to end. The
// cancel happens under v.mu, so no List running at the same time can start
// new work (a WaitGroup Add racing the Wait) once Close has begun.
func (v *ValveServers) Close() {
	v.mu.Lock()
	v.cancel()
	v.mu.Unlock()
	v.wg.Wait()
}

func cacheKey(appID int) string { return "sdr-" + strconv.Itoa(appID) }

// app returns the state of appID, reading the disk cache the first time.
// Called with v.mu held.
func (v *ValveServers) app(appID int) *sdrApp {
	a := v.apps[appID]
	if a == nil {
		a = &sdrApp{id: appID, pings: map[string]POPPing{}}
		v.apps[appID] = a
	}
	if !a.loaded && v.cache != nil {
		a.loaded = true
		if b, at, err := v.cache.Load(cacheKey(appID)); err == nil {
			if cfg, err := ParseSDRConfig(appID, b); err == nil {
				a.cfg, a.fetchedAt = cfg, at
			} else {
				v.log.Warn("ignoring the saved Valve server list", "app", appID, "err", err)
			}
		}
	}
	return a
}

// List is the app's POP list as it stands, with whatever pings have come
// back. It starts a download when the list is missing or old, and a ping
// round when the pings are old.
func (v *ValveServers) List(appID int) ServerList {
	v.mu.Lock()
	defer v.mu.Unlock()
	a := v.app(appID)
	now := v.now()
	if !a.fetching && (a.cfg == nil || v.old(a, now)) && !now.Before(a.nextTry) {
		v.startFetch(a)
	}
	if a.cfg != nil && (a.pingedAt.IsZero() || now.Sub(a.pingedAt) >= v.pingTTL) {
		v.startPing(a)
	}
	return v.view(a, now)
}

// old says the list is due for a refresh: older than the TTL, or saved "in
// the future" (the PC's clock was wrong when it was written, or the file's
// time was changed), which would otherwise keep it current for years.
func (v *ValveServers) old(a *sdrApp, now time.Time) bool {
	age := now.Sub(a.fetchedAt)
	return age >= v.ttl || age < -time.Minute
}

// Ping measures every POP of the app again now (when its list is known and
// no round is running) and returns the list. Without a list it is the
// player's "try again": a download starts now instead of after the backoff.
func (v *ValveServers) Ping(appID int) ServerList {
	v.mu.Lock()
	defer v.mu.Unlock()
	a := v.app(appID)
	if a.cfg != nil {
		v.startPing(a)
	} else if !a.fetching {
		v.startFetch(a)
	}
	return v.view(a, v.now())
}

// startFetch downloads the app's config in the background. v.mu held.
func (v *ValveServers) startFetch(a *sdrApp) {
	if v.ctx.Err() != nil {
		return
	}
	a.fetching = true
	v.wg.Add(1)
	go func() {
		defer v.wg.Done()
		b, err := v.src.Fetch(v.ctx, a.id)
		var cfg *SDRConfig
		if err == nil {
			cfg, err = ParseSDRConfig(a.id, b)
		}
		v.mu.Lock()
		defer v.mu.Unlock()
		a.fetching = false
		if err != nil {
			a.lastErr = err
			a.backoff = min(max(2*a.backoff, retryMin), retryMax)
			a.nextTry = v.now().Add(a.backoff)
			v.log.Warn("couldn't download Valve's server list", "app", a.id, "err", err, "retry_in", a.backoff)
			return
		}
		a.cfg, a.fetchedAt, a.lastErr, a.backoff, a.nextTry = cfg, v.now(), nil, 0, time.Time{}
		v.log.Info("Valve server list updated", "app", a.id, "revision", cfg.Revision, "pops", len(cfg.POPs))
		if v.cache != nil {
			if err := v.cache.Save(cacheKey(a.id), b); err != nil {
				v.log.Warn("couldn't save Valve's server list", "app", a.id, "err", err)
			}
		}
		// A new list may have new POPs: measure them now.
		v.startPing(a)
	}()
}

// startPing runs a ping round over the app's POPs. v.mu held.
func (v *ValveServers) startPing(a *sdrApp) {
	if a.pinging || v.ctx.Err() != nil {
		return
	}
	a.pinging = true
	cfg := a.cfg
	pops := slices.Clone(cfg.POPs)
	v.wg.Add(1)
	go func() {
		defer v.wg.Done()
		v.prober.Probe(v.ctx, pops, func(id string, r POPPing) {
			v.mu.Lock()
			a.pings[id] = r
			v.mu.Unlock()
		})
		v.mu.Lock()
		defer v.mu.Unlock()
		a.pinging = false
		a.pingedAt = v.now()
		// A new list arrived during this round (its own startPing found
		// this one running): measure the new POPs too, or they would wait
		// for the next round with no number and no reason.
		if a.cfg != cfg {
			v.startPing(a)
		}
	}()
}

// ServerList is the JSON the game page shows.
type ServerList struct {
	Source    string     `json:"source"` // "valve_sdr", or "" when the game has no list
	AppID     int        `json:"app_id,omitempty"`
	State     string     `json:"state"` // unsupported | loading | ready | error
	Message   string     `json:"message,omitempty"`
	Revision  uint64     `json:"revision,omitempty"`
	FetchedAt *time.Time `json:"fetched_at,omitempty"`
	// Stale: the list shown is older than the TTL, or the last refresh
	// failed; it is still Valve's data, from an earlier download.
	Stale    bool       `json:"stale"`
	Pinging  bool       `json:"pinging"`
	PingedAt *time.Time `json:"pinged_at,omitempty"`
	POPs     []POPView  `json:"pops"`
	// NoRelay counts POPs listed without a public relay address (partner
	// data centers): there is nothing to ping there.
	NoRelay int `json:"no_relay"`
}

// POPView is one row of the list. Numbers that weren't measured are null.
type POPView struct {
	ID       string     `json:"id"`
	City     string     `json:"city"`
	Country  string     `json:"country,omitempty"`
	Desc     string     `json:"desc"`
	Lat      *float64   `json:"lat"`
	Lon      *float64   `json:"lon"`
	GeoFix   GeoFix     `json:"geo_fix,omitempty"`
	Relays   int        `json:"relays"`
	PingMS   *float64   `json:"ping_ms"`
	LossPct  *float64   `json:"loss_pct"`
	Sent     int        `json:"sent"`
	Received int        `json:"received"`
	Note     string     `json:"note,omitempty"`
	PingedAt *time.Time `json:"pinged_at,omitempty"`
}

// Unsupported is the answer for a game without a public server list.
func Unsupported(msg string) ServerList {
	return ServerList{State: "unsupported", Message: msg, POPs: []POPView{}}
}

func ptr[T any](v T) *T { return &v }

// view builds the JSON. v.mu held.
func (v *ValveServers) view(a *sdrApp, now time.Time) ServerList {
	l := ServerList{Source: "valve_sdr", AppID: a.id, Pinging: a.pinging, POPs: []POPView{}}
	if !a.pingedAt.IsZero() {
		l.PingedAt = ptr(a.pingedAt)
	}
	if a.cfg == nil {
		if a.lastErr != nil && !a.fetching {
			l.State = "error"
			l.Message = fmt.Sprintf("Couldn't get Valve's server list: %v. Trying again in %s.", a.lastErr, retryIn(a.nextTry.Sub(now)))
		} else {
			l.State = "loading"
		}
		return l
	}
	l.State = "ready"
	l.Revision = a.cfg.Revision
	l.FetchedAt = ptr(a.fetchedAt)
	l.Stale = a.lastErr != nil || v.old(a, now)
	if a.lastErr != nil {
		l.Message = fmt.Sprintf("Couldn't refresh the list (%v); showing the copy saved earlier.", a.lastErr)
	}
	for _, p := range a.cfg.POPs {
		if len(p.Relays) == 0 {
			l.NoRelay++
			continue
		}
		pv := POPView{ID: p.ID, City: p.City, Country: p.Country, Desc: p.Desc, GeoFix: p.GeoFix, Relays: len(p.Relays)}
		if p.Geo.Known {
			pv.Lat, pv.Lon = ptr(p.Geo.Lat), ptr(p.Geo.Lon)
		}
		if r, ok := a.pings[p.ID]; ok {
			pv.Sent, pv.Received, pv.Note, pv.PingedAt = r.Sent, r.Received, r.Note, ptr(r.At)
			if r.Received > 0 {
				pv.PingMS = ptr(float64(r.RTT.Microseconds()) / 1000)
			}
			if r.Sent > 0 {
				pv.LossPct = ptr(r.LossPct())
			}
		}
		l.POPs = append(l.POPs, pv)
	}
	slices.SortStableFunc(l.POPs, func(x, y POPView) int {
		if c := cmp.Compare(rank(x), rank(y)); c != 0 {
			return c
		}
		if x.PingMS != nil && y.PingMS != nil {
			if c := cmp.Compare(*x.PingMS, *y.PingMS); c != 0 {
				return c
			}
		}
		return cmp.Compare(x.ID, y.ID)
	})
	return l
}

// rank orders rows: measured first, then not measured yet, then no reply.
func rank(p POPView) int {
	switch {
	case p.PingMS != nil:
		return 0
	case p.PingedAt == nil:
		return 1
	default:
		return 2
	}
}

func retryIn(d time.Duration) string {
	if d < time.Second {
		return "a moment"
	}
	if d < time.Minute {
		return strconv.Itoa(int(d.Round(time.Second)/time.Second)) + " s"
	}
	return strconv.Itoa(int((d+30*time.Second)/time.Minute)) + " min"
}
