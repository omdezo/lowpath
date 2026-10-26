package fakeplatform

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"sync"

	"lowpath/internal/platform"
)

// Service is a service manager the test sets the state of. It starts out
// not installed.
type Service struct {
	mu         sync.Mutex
	state      platform.ServiceState
	stateErr   error
	startErr   error
	installErr error
	onStart    func()
	starts     int
	installs   []platform.ServiceConfig
	uninstalls int
	secureErr  error
	secured    []SecuredDir
}

// SecuredDir is one SecureDataDir call.
type SecuredDir struct {
	Dir string
	// Empty: the folder held nothing yet when it was secured.
	Empty bool
}

// Set replaces what State reports, and err its error.
func (s *Service) Set(st platform.ServiceState, err error) {
	s.mu.Lock()
	s.state, s.stateErr = st, err
	s.mu.Unlock()
}

// SetStartErr is what Start returns; nil starts the service.
func (s *Service) SetStartErr(err error) { s.mu.Lock(); s.startErr = err; s.mu.Unlock() }

// SetInstallErr is what Install and Uninstall return.
func (s *Service) SetInstallErr(err error) { s.mu.Lock(); s.installErr = err; s.mu.Unlock() }

// OnStart runs fn (outside the lock) each time Start succeeds, so the test
// can bring its service up the way the service manager would.
func (s *Service) OnStart(fn func()) { s.mu.Lock(); s.onStart = fn; s.mu.Unlock() }

func (s *Service) State() (platform.ServiceState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state, s.stateErr
}

func (s *Service) Start() error {
	s.mu.Lock()
	if s.startErr != nil {
		err := s.startErr
		s.mu.Unlock()
		return err
	}
	if !s.state.Installed {
		s.mu.Unlock()
		return errors.New("fakeplatform: the service is not installed")
	}
	s.starts++
	s.state.Running = true
	fn := s.onStart
	s.mu.Unlock()
	if fn != nil {
		fn()
	}
	return nil
}

// Starts counts successful Start calls.
func (s *Service) Starts() int { s.mu.Lock(); defer s.mu.Unlock(); return s.starts }

func (s *Service) Install(cfg platform.ServiceConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.installErr != nil {
		return s.installErr
	}
	cfg.Args = slices.Clone(cfg.Args)
	s.installs = append(s.installs, cfg)
	s.state = platform.ServiceState{Installed: true, Running: true, Exe: cfg.Exe}
	return nil
}

// Installs lists every Install call's config.
func (s *Service) Installs() []platform.ServiceConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.installs)
}

func (s *Service) Uninstall() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.installErr != nil {
		return s.installErr
	}
	s.uninstalls++
	s.state = platform.ServiceState{}
	return nil
}

// SetSecureErr is what SecureDataDir returns.
func (s *Service) SetSecureErr(err error) { s.mu.Lock(); s.secureErr = err; s.mu.Unlock() }

// SecureDataDir records the call and makes the folder (with no access
// list: tests run unelevated).
func (s *Service) SecureDataDir(dir string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.secureErr != nil {
		return s.secureErr
	}
	entries, _ := os.ReadDir(dir)
	s.secured = append(s.secured, SecuredDir{Dir: dir, Empty: len(entries) == 0})
	return os.MkdirAll(dir, 0o700)
}

// Secured lists every SecureDataDir call.
func (s *Service) Secured() []SecuredDir {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.secured)
}

// Uninstalls counts Uninstall calls.
func (s *Service) Uninstalls() int { s.mu.Lock(); defer s.mu.Unlock(); return s.uninstalls }

// ServiceAPI is an in-memory ServiceAPI: Dial reaches whatever Listen
// returned last, until that listener closes.
type ServiceAPI struct {
	mu      sync.Mutex
	ln      *memListener
	dialErr error
	dials   int
}

func NewServiceAPI() *ServiceAPI { return &ServiceAPI{} }

func (a *ServiceAPI) String() string { return "fake service API" }

func (a *ServiceAPI) Listen() (net.Listener, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ln != nil && !a.ln.isClosed() {
		return nil, errors.New("fakeplatform: the service API is already taken")
	}
	a.ln = &memListener{conns: make(chan net.Conn), done: make(chan struct{})}
	return a.ln, nil
}

// SetDialErr makes Dial fail with err (for example platform.ErrNotService);
// nil makes it work again.
func (a *ServiceAPI) SetDialErr(err error) { a.mu.Lock(); a.dialErr = err; a.mu.Unlock() }

// Dials counts Dial calls.
func (a *ServiceAPI) Dials() int { a.mu.Lock(); defer a.mu.Unlock(); return a.dials }

func (a *ServiceAPI) Dial(ctx context.Context) (net.Conn, error) {
	a.mu.Lock()
	a.dials++
	ln, err := a.ln, a.dialErr
	a.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if ln == nil {
		return nil, fmt.Errorf("%w (fake)", platform.ErrServiceNotRunning)
	}
	client, server := net.Pipe()
	select {
	case ln.conns <- server:
		return client, nil
	case <-ln.done:
		client.Close()
		server.Close()
		return nil, fmt.Errorf("%w (fake)", platform.ErrServiceNotRunning)
	case <-ctx.Done():
		client.Close()
		server.Close()
		return nil, ctx.Err()
	}
}

type memListener struct {
	conns chan net.Conn
	once  sync.Once
	done  chan struct{}
}

func (l *memListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *memListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *memListener) isClosed() bool {
	select {
	case <-l.done:
		return true
	default:
		return false
	}
}

func (l *memListener) Addr() net.Addr { return memAddr{} }

type memAddr struct{}

func (memAddr) Network() string { return "memory" }
func (memAddr) String() string  { return "fake service API" }
