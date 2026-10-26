// Package fakeplatform is an in-memory platform.Platform for tests: route
// mode, process lists, game detection and elevation all work without admin
// rights, drivers, real games or a particular OS. A test plays the game by
// calling Session.Send and sees what would have been injected back into the
// game's socket on Session.Injected.
package fakeplatform

import (
	"errors"
	"fmt"
	"image"
	"maps"
	"net/netip"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"lowpath/internal/capture"
	"lowpath/internal/compat"
	"lowpath/internal/detect"
	"lowpath/internal/diag"
	"lowpath/internal/owner"
	"lowpath/internal/platform"
)

// Fake holds one fake of each platform part; tests change them through
// their setters while the app runs.
type Fake struct {
	OS         string
	Capture    *Capture
	Owners     *Owners
	Privileges *Privileges
	Games      *Games
	Icons      *Icons
	Paths      *Paths
	Flows      *Flows
	Inventory  *Inventory
}

// New makes a fake platform that reports the host's OS name and keeps its
// folders under dir. Capture is available, nothing runs, nothing is
// installed, and the process is not elevated.
func New(dir string) *Fake {
	return &Fake{
		OS:         runtime.GOOS,
		Capture:    &Capture{},
		Owners:     &Owners{},
		Privileges: &Privileges{},
		Games:      &Games{},
		Icons:      &Icons{},
		Paths:      &Paths{dir: dir},
		Flows:      &Flows{},
		Inventory:  &Inventory{},
	}
}

// Platform returns the fakes as a platform.Platform.
func (f *Fake) Platform() *platform.Platform {
	return &platform.Platform{
		OS: f.OS, Capture: f.Capture, Owners: f.Owners, Privileges: f.Privileges,
		Games: f.Games, Icons: f.Icons, Paths: f.Paths, Flows: f.Flows, Inventory: f.Inventory,
	}
}

// Inventory is the other network software the test says is there. It starts
// empty and offline: no conflicts.
type Inventory struct {
	mu   sync.Mutex
	inv  compat.Inventory
	errs map[string]error
	hang chan struct{}
}

// Set replaces the inventory.
func (i *Inventory) Set(inv compat.Inventory) { i.mu.Lock(); i.inv = inv; i.mu.Unlock() }

// SetErr makes one part fail ("processes", "services", "programs",
// "adapters" or "route"); nil makes it work again.
func (i *Inventory) SetErr(part string, err error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.errs == nil {
		i.errs = map[string]error{}
	}
	i.errs[part] = err
}

// Hang makes every read block until release is closed, like a service
// manager that stopped answering.
func (i *Inventory) Hang(release chan struct{}) { i.mu.Lock(); i.hang = release; i.mu.Unlock() }

func (i *Inventory) read(part string) (compat.Inventory, error) {
	i.mu.Lock()
	hang := i.hang
	i.mu.Unlock()
	if hang != nil {
		<-hang
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.inv, i.errs[part]
}

func (i *Inventory) Processes() ([]string, error) {
	inv, err := i.read("processes")
	return slices.Clone(inv.Processes), err
}

func (i *Inventory) Services() ([]compat.Service, error) {
	inv, err := i.read("services")
	return slices.Clone(inv.Services), err
}

func (i *Inventory) Programs() ([]string, error) {
	inv, err := i.read("programs")
	return slices.Clone(inv.Programs), err
}

func (i *Inventory) Adapters() ([]compat.Adapter, error) {
	inv, err := i.read("adapters")
	return slices.Clone(inv.Adapters), err
}

func (i *Inventory) DefaultRoute() (int, error) {
	inv, err := i.read("route")
	if err == nil && inv.DefaultRoute == 0 {
		err = diag.ErrNoRoute
	}
	return inv.DefaultRoute, err
}

// Capture is a capture backend whose sessions the test drives.
type Capture struct {
	mu         sync.Mutex
	avail      error
	startErr   error
	recoverErr error
	sessions   []*Session
	recovered  []capture.Marker
}

func (c *Capture) Name() string { return "fake" }

// SetAvailable sets what Available (and so Start) reports.
func (c *Capture) SetAvailable(err error) { c.mu.Lock(); c.avail = err; c.mu.Unlock() }

// SetStartErr makes Start fail with err even when available.
func (c *Capture) SetStartErr(err error) { c.mu.Lock(); c.startErr = err; c.mu.Unlock() }

func (c *Capture) Available() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.avail
}

func (c *Capture) Start(cfg capture.Config) (capture.Session, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.avail != nil {
		return nil, c.avail
	}
	if c.startErr != nil {
		return nil, c.startErr
	}
	if cfg.OnPacket == nil {
		return nil, errors.New("fakeplatform: Config.OnPacket is required")
	}
	s := &Session{cfg: cfg, injected: make(chan Injected, 1024), done: make(chan struct{})}
	c.sessions = append(c.sessions, s)
	return s, nil
}

// SetRecoverErr makes Recover fail with err.
func (c *Capture) SetRecoverErr(err error) { c.mu.Lock(); c.recoverErr = err; c.mu.Unlock() }

// Recover records the marker it was asked to clean up after.
func (c *Capture) Recover(m capture.Marker) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recovered = append(c.recovered, m)
	return c.recoverErr
}

// Recovered lists the markers Recover got, oldest first.
func (c *Capture) Recovered() []capture.Marker {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.recovered)
}

// Sessions lists every session started so far, oldest first.
func (c *Capture) Sessions() []*Session {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.sessions)
}

// Last is the newest session, or nil when none was started.
func (c *Capture) Last() *Session {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.sessions) == 0 {
		return nil
	}
	return c.sessions[len(c.sessions)-1]
}

// Injected is one reply the app delivered to the game.
type Injected struct {
	Local, Remote netip.AddrPort
	Payload       string
}

// Session is a running fake capture.
type Session struct {
	cfg                capture.Config
	injected           chan Injected
	captured, injCount atomic.Uint64
	dropped            atomic.Uint64
	closed             atomic.Bool
	done               chan struct{}
	stopOnce           sync.Once
	reason             atomic.Pointer[error]
}

// Config is what the app asked to capture.
func (s *Session) Config() capture.Config { return s.cfg }

// Send plays one outbound game packet through the capture callback and
// reports whether the app consumed it (false: it would have gone out
// directly). A closed session captures nothing.
func (s *Session) Send(local, remote netip.AddrPort, payload string) bool {
	if s.closed.Load() {
		return false
	}
	s.captured.Add(1)
	return s.cfg.OnPacket(local, remote, []byte(payload))
}

// Injected delivers the replies the app injected, in order.
func (s *Session) Injected() <-chan Injected { return s.injected }

func (s *Session) Inject(local, remote netip.AddrPort, payload []byte) error {
	if s.closed.Load() {
		return capture.ErrClosed
	}
	select {
	case s.injected <- Injected{local, remote, string(payload)}:
		s.injCount.Add(1)
		return nil
	default:
		s.dropped.Add(1)
		return errors.New("fakeplatform: injected queue full")
	}
}

func (s *Session) Stats() capture.Stats {
	st := capture.Stats{Captured: s.captured.Load(), Injected: s.injCount.Load(), Errors: s.dropped.Load(), GamePIDs: []uint32{}}
	if st.Errors > 0 {
		st.LastError = "fakeplatform: injected queue full"
	}
	return st
}

func (s *Session) Close() error {
	s.stop()
	return nil
}

func (s *Session) stop() {
	s.stopOnce.Do(func() {
		s.closed.Store(true)
		close(s.done)
	})
}

// Fail makes the session stop on its own with err, the way the capture
// watchdog fails open.
func (s *Session) Fail(err error) {
	s.reason.CompareAndSwap(nil, &err)
	s.stop()
}

func (s *Session) Done() <-chan struct{} { return s.done }

func (s *Session) Err() error {
	if e := s.reason.Load(); e != nil {
		return *e
	}
	return nil
}

// Closed reports whether the session stopped: closed by the app, or Fail.
func (s *Session) Closed() bool { return s.closed.Load() }

// Owners is a socket-owner table the test fills in.
type Owners struct {
	mu      sync.Mutex
	sockets []owner.Socket
	names   map[uint32]string
	err     error
}

// Set replaces the socket table and the process list.
func (o *Owners) Set(sockets []owner.Socket, names map[uint32]string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.sockets, o.names = slices.Clone(sockets), maps.Clone(names)
}

// SetErr makes both lookups fail with err (nil: work again).
func (o *Owners) SetErr(err error) { o.mu.Lock(); o.err = err; o.mu.Unlock() }

func (o *Owners) UDPSockets() ([]owner.Socket, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.sockets), o.err
}

func (o *Owners) ProcessNames() (map[uint32]string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.names == nil {
		return map[uint32]string{}, o.err
	}
	return maps.Clone(o.names), o.err
}

// Privileges records relaunch requests instead of showing a prompt.
type Privileges struct {
	mu          sync.Mutex
	elevated    bool
	canElevate  bool
	relaunchErr error
	relaunches  [][]string
}

func (p *Privileges) SetElevated(v bool)   { p.mu.Lock(); p.elevated = v; p.mu.Unlock() }
func (p *Privileges) SetCanElevate(v bool) { p.mu.Lock(); p.canElevate = v; p.mu.Unlock() }

// SetRelaunchErr is what Relaunch returns (platform.ErrElevateCancelled to
// play a declined prompt).
func (p *Privileges) SetRelaunchErr(err error) { p.mu.Lock(); p.relaunchErr = err; p.mu.Unlock() }

func (p *Privileges) Elevated() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.elevated
}

func (p *Privileges) CanElevate() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.canElevate
}

func (p *Privileges) Relaunch(args []string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.canElevate {
		return fmt.Errorf("fakeplatform: no prompt: %w", errors.ErrUnsupported)
	}
	p.relaunches = append(p.relaunches, slices.Clone(args))
	return p.relaunchErr
}

// Relaunches lists the arguments of every Relaunch call.
func (p *Privileges) Relaunches() [][]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.relaunches)
}

// Games is a game detector with a process list and Steam folders the test
// sets.
type Games struct {
	mu      sync.Mutex
	running map[string]bool
	exes    map[string]string
	libs    []string
	roots   []string
	wine    []string
	install []string
}

// SetRunning replaces the running processes (image names, any case).
func (g *Games) SetRunning(names ...string) {
	m := map[string]bool{}
	for _, n := range names {
		m[strings.ToLower(n)] = true
	}
	g.mu.Lock()
	g.running = m
	g.mu.Unlock()
}

// SetExe says where a running process's program lives.
func (g *Games) SetExe(name, path string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.exes == nil {
		g.exes = map[string]string{}
	}
	g.exes[strings.ToLower(name)] = path
}

// SetSteam sets the Steam library and client folders.
func (g *Games) SetSteam(libraries, roots []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.libs, g.roots = slices.Clone(libraries), slices.Clone(roots)
}

// SetWine sets the Wine prefixes' drive_c folders and the launcher install
// folders.
func (g *Games) SetWine(driveC, installs []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.wine, g.install = slices.Clone(driveC), slices.Clone(installs)
}

func (g *Games) Running() map[string]bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.running == nil {
		return map[string]bool{}
	}
	return maps.Clone(g.running)
}

func (g *Games) RunningExe(names []string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, n := range names {
		n = strings.ToLower(n)
		if g.running[n] {
			if p := g.exes[n]; p != "" {
				return p
			}
		}
	}
	return ""
}

func (g *Games) SteamLibraries() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.libs)
}

func (g *Games) SteamRoots() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.roots)
}

func (g *Games) WinePrefixes() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.wine)
}

func (g *Games) LauncherInstalls() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.install)
}

// Icons hands out icons the test registered per program path.
type Icons struct {
	mu    sync.Mutex
	icons map[string]image.Image
}

func (i *Icons) Set(path string, img image.Image) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.icons == nil {
		i.icons = map[string]image.Image{}
	}
	i.icons[path] = img
}

func (i *Icons) ExeIcon(path string, size int) (image.Image, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if img, ok := i.icons[path]; ok {
		return img, nil
	}
	return nil, fmt.Errorf("fakeplatform: no icon for %s", path)
}

// Paths keeps everything under one folder.
type Paths struct{ dir string }

func (p *Paths) DataDir() (string, error)  { return filepath.Join(p.dir, "data"), nil }
func (p *Paths) CacheDir() (string, error) { return filepath.Join(p.dir, "cache"), nil }
func (p *Paths) DriverDirs(bool) ([]string, []error) {
	return []string{filepath.Join(p.dir, "driver")}, nil
}

// Flows is a flow watcher the test feeds with Emit.
type Flows struct {
	mu      sync.Mutex
	err     error
	onEvent func(detect.FlowEvent)
	watch   *flowWatch
	starts  int
}

// SetErr makes Watch fail with err (detect.ErrNeedAdmin to play an
// unelevated Windows; nil: work again).
func (f *Flows) SetErr(err error) { f.mu.Lock(); f.err = err; f.mu.Unlock() }

func (f *Flows) Watch(onEvent func(detect.FlowEvent)) (detect.FlowWatch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	if f.onEvent != nil {
		return nil, errors.New("fakeplatform: a flow watch is already running")
	}
	f.onEvent = onEvent
	f.starts++
	f.watch = &flowWatch{f: f, done: make(chan struct{})}
	return f.watch, nil
}

// End stops the open watch the way the OS can on its own (an ETW session
// stopped from outside): events stop and Done is closed, but the watch
// still waits for its owner's Close.
func (f *Flows) End() {
	f.mu.Lock()
	w := f.watch
	f.onEvent, f.watch = nil, nil
	f.mu.Unlock()
	if w != nil {
		w.end()
	}
}

// Watching reports whether a watch is open.
func (f *Flows) Watching() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.onEvent != nil }

// Starts counts the watches opened so far.
func (f *Flows) Starts() int { f.mu.Lock(); defer f.mu.Unlock(); return f.starts }

// Emit delivers ev to the open watch, as the OS would; false when none is
// open.
func (f *Flows) Emit(ev detect.FlowEvent) bool {
	f.mu.Lock()
	on := f.onEvent
	f.mu.Unlock()
	if on == nil {
		return false
	}
	on(ev)
	return true
}

type flowWatch struct {
	f    *Flows
	done chan struct{}
	once sync.Once
}

func (w *flowWatch) end() { w.once.Do(func() { close(w.done) }) }

func (w *flowWatch) Done() <-chan struct{} { return w.done }

func (w *flowWatch) Close() error {
	w.f.mu.Lock()
	if w.f.watch == w {
		w.f.onEvent, w.f.watch = nil, nil
	}
	w.f.mu.Unlock()
	w.end()
	return nil
}
