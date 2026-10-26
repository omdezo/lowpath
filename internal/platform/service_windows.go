package platform

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// serviceSDDL is the service object's security descriptor: the default one
// (SYSTEM: query, start, stop, interrogate; Administrators: everything)
// plus SERVICE_START (RP) for Interactive Users, so the unelevated window
// can start a stopped service without an administrator prompt. Users can't
// stop or reconfigure it (no WP, DC or WD): changing its program would run
// any program as SYSTEM.
const serviceSDDL = "D:(A;;CCLCSWRPWPDTLOCRRC;;;SY)(A;;CCDCLCSWRPWPDTLOCRSDRCWDWO;;;BA)(A;;CCLCSWRPLOCRRC;;;IU)"

// dataDirSDDL is the service data folder's DACL: SYSTEM and Administrators
// only, inherited by everything in it. The folder holds relay keys, and a
// SYSTEM process writing logs into a folder users can change is open to
// link tricks that make it overwrite other files.
const dataDirSDDL = "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"

const (
	serviceDisplayName = "Lowpath service"
	serviceDescription = "Runs Lowpath's game traffic routing with the rights packet capture needs, so the Lowpath window doesn't ask for administrator rights. Development build."
	serviceExeName     = "lowpathsvc.exe"
)

var (
	advapi32                     = windows.NewLazySystemDLL("advapi32.dll")
	procSetServiceObjectSecurity = advapi32.NewProc("SetServiceObjectSecurity")
)

// WindowsService is the ServiceManager for the Windows service control
// manager.
type WindowsService struct {
	name         string
	programFiles func() (string, error)
	elevated     func() bool
}

// NewWindowsService manages the LowpathSvc service.
func NewWindowsService() *WindowsService {
	return &WindowsService{
		name:         ServiceName,
		programFiles: func() (string, error) { return windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, 0) },
		elevated:     func() bool { return windows.GetCurrentProcessToken().IsElevated() },
	}
}

// open connects to the service manager and opens the service with only the
// rights asked for, so querying and starting work without administrator
// rights (mgr.Connect asks for full access).
func (s *WindowsService) open(access uint32) (windows.Handle, *mgr.Service, error) {
	scm, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return 0, nil, fmt.Errorf("service manager: %w", err)
	}
	name, _ := windows.UTF16PtrFromString(s.name)
	h, err := windows.OpenService(scm, name, access)
	if err != nil {
		windows.CloseServiceHandle(scm)
		return 0, nil, err
	}
	return scm, &mgr.Service{Name: s.name, Handle: h}, nil
}

func (s *WindowsService) State() (ServiceState, error) {
	scm, sv, err := s.open(windows.SERVICE_QUERY_STATUS | windows.SERVICE_QUERY_CONFIG)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return ServiceState{}, nil
	}
	if err != nil {
		return ServiceState{}, fmt.Errorf("service %s: %w", s.name, err)
	}
	defer windows.CloseServiceHandle(scm)
	defer sv.Close()
	st, err := sv.Query()
	if err != nil {
		return ServiceState{Installed: true}, fmt.Errorf("service %s: %w", s.name, err)
	}
	out := ServiceState{Installed: true, Running: st.State == svc.Running}
	if out.Running {
		out.PID = st.ProcessId
	}
	if cfg, err := sv.Config(); err == nil {
		if args, err := windows.DecomposeCommandLine(cfg.BinaryPathName); err == nil && len(args) > 0 {
			out.Exe = args[0]
		}
	}
	return out, nil
}

// PID is the running service's process id, for Pipe's server check.
func (s *WindowsService) PID() (uint32, error) {
	st, err := s.State()
	switch {
	case err != nil:
		return 0, err
	case !st.Installed:
		return 0, fmt.Errorf("%w: it isn't installed", ErrServiceNotRunning)
	case !st.Running:
		return 0, ErrServiceNotRunning
	}
	return st.PID, nil
}

func (s *WindowsService) Start() error {
	scm, sv, err := s.open(windows.SERVICE_START | windows.SERVICE_QUERY_STATUS)
	if err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return fmt.Errorf("start service %s: %w (reinstall it to let users start it)", s.name, err)
		}
		return fmt.Errorf("start service %s: %w", s.name, err)
	}
	defer windows.CloseServiceHandle(scm)
	defer sv.Close()
	if err := sv.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		return fmt.Errorf("start service %s: %w", s.name, err)
	}
	return nil
}

// Install copies the program under Program Files\Lowpath, secures the data
// folder, creates (or updates) the service, sets it to start at boot and to
// restart after a crash, lets interactive users start it, and starts it.
func (s *WindowsService) Install(cfg ServiceConfig) error {
	if !s.elevated() {
		return fmt.Errorf("install service %s: %w", s.name, ErrNeedAdmin)
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("service manager: %w", err)
	}
	defer m.Disconnect()

	existing, err := m.OpenService(s.name)
	if err == nil {
		// The copy below replaces the program, which can't happen while it
		// runs.
		if err := stopAndWait(existing, 20*time.Second); err != nil {
			existing.Close()
			return err
		}
	}
	exe, err := s.placeProgram(cfg.Exe)
	if err != nil {
		if existing != nil {
			existing.Close()
		}
		return err
	}
	if cfg.DataDir != "" {
		err := secureDir(cfg.DataDir)
		if err == nil {
			err = writeSeed(cfg.DataDir, cfg.Seed)
		}
		if err != nil {
			if existing != nil {
				existing.Close()
			}
			return err
		}
	}

	c := mgr.Config{
		StartType:    mgr.StartAutomatic,
		ErrorControl: mgr.ErrorNormal,
		DisplayName:  serviceDisplayName,
		Description:  serviceDescription,
		SidType:      windows.SERVICE_SID_TYPE_UNRESTRICTED,
	}
	var sv *mgr.Service
	if existing != nil {
		sv = existing
		cur, err := sv.Config()
		if err != nil {
			sv.Close()
			return fmt.Errorf("service %s: %w", s.name, err)
		}
		c.ServiceType = cur.ServiceType
		c.BinaryPathName = commandLine(exe, cfg.Args)
		if err := sv.UpdateConfig(c); err != nil {
			sv.Close()
			return fmt.Errorf("update service %s: %w", s.name, err)
		}
	} else if sv, err = m.CreateService(s.name, exe, c, cfg.Args...); err != nil {
		return fmt.Errorf("create service %s: %w", s.name, err)
	}
	defer sv.Close()

	// A crash, a kill or run returning an error restarts the service: the
	// capture's own fail-open has already sent the game direct by then.
	if err := sv.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 2 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
	}, uint32((24 * time.Hour).Seconds())); err != nil {
		return fmt.Errorf("service %s recovery: %w", s.name, err)
	}
	if err := sv.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return fmt.Errorf("service %s recovery: %w", s.name, err)
	}
	if err := setServiceSecurity(sv.Handle, serviceSDDL); err != nil {
		return fmt.Errorf("service %s security: %w", s.name, err)
	}
	if err := sv.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		return fmt.Errorf("start service %s: %w", s.name, err)
	}
	return nil
}

func (s *WindowsService) Uninstall() error {
	if !s.elevated() {
		return fmt.Errorf("uninstall service %s: %w", s.name, ErrNeedAdmin)
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("service manager: %w", err)
	}
	defer m.Disconnect()
	sv, err := m.OpenService(s.name)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("service %s: %w", s.name, err)
	}
	defer sv.Close()
	exe := ""
	if cfg, err := sv.Config(); err == nil {
		if args, err := windows.DecomposeCommandLine(cfg.BinaryPathName); err == nil && len(args) > 0 {
			exe = args[0]
		}
	}
	if err := stopAndWait(sv, 20*time.Second); err != nil {
		return err
	}
	if err := sv.Delete(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
		return fmt.Errorf("delete service %s: %w", s.name, err)
	}
	// Best effort: the program may be the one running this uninstall.
	if exe != "" && strings.EqualFold(filepath.Base(exe), serviceExeName) {
		os.Remove(exe)
	}
	return nil
}

func (s *WindowsService) SecureDataDir(dir string) error {
	if !s.elevated() {
		return fmt.Errorf("secure %s: %w", dir, ErrNeedAdmin)
	}
	return secureDir(dir)
}

// placeProgram copies src to <Program Files>\Lowpath\lowpathsvc.exe, which
// only administrators can change, and returns that path.
func (s *WindowsService) placeProgram(src string) (string, error) {
	pf, err := s.programFiles()
	if err != nil {
		return "", fmt.Errorf("find Program Files: %w", err)
	}
	dir := filepath.Join(pf, appDir)
	dst := filepath.Join(dir, serviceExeName)
	if a, err := filepath.Abs(src); err == nil && strings.EqualFold(a, dst) {
		return dst, nil // reinstalling from the installed copy
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("install folder: %w", err)
	}
	in, err := os.Open(src)
	if err != nil {
		return "", fmt.Errorf("install: %w", err)
	}
	defer in.Close()
	tmp, err := os.CreateTemp(dir, "lowpathsvc-*.tmp")
	if err != nil {
		return "", fmt.Errorf("install: %w", err)
	}
	_, err = io.Copy(tmp, in)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), dst)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("install %s: %w", dst, err)
	}
	return dst, nil
}

// stopAndWait stops a running service and waits until it has stopped.
func stopAndWait(sv *mgr.Service, timeout time.Duration) error {
	st, err := sv.Query()
	if err != nil {
		return fmt.Errorf("service %s: %w", sv.Name, err)
	}
	if st.State == svc.Stopped {
		return nil
	}
	if st.State != svc.StopPending {
		if _, err := sv.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
			return fmt.Errorf("stop service %s: %w", sv.Name, err)
		}
	}
	deadline := time.Now().Add(timeout)
	for {
		st, err := sv.Query()
		if err != nil {
			return fmt.Errorf("service %s: %w", sv.Name, err)
		}
		if st.State == svc.Stopped {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("service %s did not stop within %s", sv.Name, timeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// commandLine is the service's command line, quoted as CreateService does.
func commandLine(exe string, args []string) string {
	parts := []string{syscall.EscapeArg(exe)}
	for _, a := range args {
		parts = append(parts, syscall.EscapeArg(a))
	}
	return strings.Join(parts, " ")
}

func setServiceSecurity(h windows.Handle, sddl string) error {
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	if err := procSetServiceObjectSecurity.Find(); err != nil {
		return err
	}
	r, _, e := procSetServiceObjectSecurity.Call(uintptr(h), uintptr(windows.DACL_SECURITY_INFORMATION), uintptr(unsafe.Pointer(sd)))
	if r == 0 {
		return e
	}
	return nil
}

// secureDir creates dir, or checks the one there, and makes Administrators
// its owner and SYSTEM and Administrators the only accounts with access.
//
// The folder is created with its access list in one step, so no user can
// slip in between: ProgramData lets every user create folders and files in
// the folders under it, and a user who got in first could plant a link
// that makes the SYSTEM service write wherever it points. For the same
// reason a folder that is already there is kept only when it is a real
// folder that SYSTEM or Administrators own and only they can change (a
// folder this function secured). Anything else (made by a user, made
// with ProgramData's inherited access list, a file or a link) is moved
// aside rather than taken over: a planted state.json could send game
// traffic through any relay.
func secureDir(dir string) error {
	return secureDirWith(dir, dataDirSDDL)
}

// secureAttempts bounds moving aside a folder that keeps coming back.
const secureAttempts = 3

func secureDirWith(dir, sddl string) error {
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return fmt.Errorf("service data folder: %w", err)
	}
	for attempt := 1; ; attempt++ {
		created, err := createDir(dir, sd)
		if err != nil {
			return fmt.Errorf("service data folder: %w", err)
		}
		if created {
			break
		}
		trusted, err := trustedDir(dir)
		if err != nil {
			return err
		}
		if trusted {
			break
		}
		if attempt == secureAttempts {
			return fmt.Errorf("service data folder %s: another program keeps making it", dir)
		}
		aside := fmt.Sprintf("%s.untrusted-%d", dir, time.Now().UnixNano())
		if err := os.Rename(dir, aside); err != nil {
			return fmt.Errorf("move aside %s, which another user could change: %w", dir, err)
		}
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return err
	}
	// Owner and access list again: a folder made by an administrator's
	// process may have another owner, and this resets the files in it.
	err = windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		admins, nil, dacl, nil)
	if err != nil {
		return fmt.Errorf("secure %s: %w", dir, err)
	}
	return nil
}

// createDir makes dir with the security descriptor sd in one call, so it
// never exists with the access list it would inherit. created is false,
// with no error, when something is there already.
func createDir(dir string, sd *windows.SECURITY_DESCRIPTOR) (created bool, err error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return false, err
	}
	sa := windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(sa))
	err = windows.CreateDirectory(p, &sa)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("create %s: %w", dir, err)
	}
	return true, nil
}

// trustedDir reports whether the existing data folder is a real folder
// owned by SYSTEM or Administrators that only they can change.
func trustedDir(dir string) (bool, error) {
	fi, err := os.Lstat(dir)
	if err != nil {
		return false, fmt.Errorf("check %s: %w", dir, err)
	}
	if !fi.IsDir() || fi.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return false, nil // a file, a symbolic link or a junction
	}
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false, fmt.Errorf("check %s: %w", dir, err)
	}
	return adminsOnly(sd), nil
}

// adminsOnly reports whether sd is owned by SYSTEM or Administrators and
// has a protected access list that allows nobody else anything: no
// inherited "Users may create files" from ProgramData, no ACE a user added.
func adminsOnly(sd *windows.SECURITY_DESCRIPTOR) bool {
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !isAdminSID(owner) {
		return false
	}
	ctl, _, err := sd.Control()
	if err != nil || ctl&windows.SE_DACL_PROTECTED == 0 {
		return false
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return false // no DACL at all gives everyone full access
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return false
		}
		if ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue // denying never opens anything up
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return false // object or callback ACEs: not something this code writes
		}
		if !isAdminSID((*windows.SID)(unsafe.Pointer(&ace.SidStart))) {
			return false
		}
	}
	return true
}

func isAdminSID(s *windows.SID) bool {
	return s.IsWellKnown(windows.WinBuiltinAdministratorsSid) || s.IsWellKnown(windows.WinLocalSystemSid)
}

// writeSeed writes the seed files that dir doesn't have yet. dir is secured
// already, so nothing in it can be a link a user planted.
func writeSeed(dir string, seed map[string][]byte) error {
	for name, b := range seed {
		if filepath.Base(name) != name {
			return fmt.Errorf("seed file %q is not a plain name", name)
		}
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue // the service's own settings win
		}
		if err != nil {
			return fmt.Errorf("copy %s: %w", name, err)
		}
		_, err = f.Write(b)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("copy %s: %w", name, err)
		}
	}
	return nil
}

// SCMRunner is the Windows ServiceRunner (the service control manager).
type SCMRunner struct{}

func NewServiceRunner() SCMRunner { return SCMRunner{} }

func (SCMRunner) Run(name string, run func(ctx context.Context) error) (bool, error) {
	is, err := svc.IsWindowsService()
	if err != nil || !is {
		return false, err
	}
	h := &scmHandler{run: run}
	if err := svc.Run(name, h); err != nil {
		return true, err
	}
	return true, h.err
}

// scmHandler reports the service's state to the service manager: Running
// while run runs, StopPending once a stop or shutdown is asked for. A
// service that stops on its own with an error exits with a non-zero code,
// which the recovery actions restart.
type scmHandler struct {
	run func(ctx context.Context) error
	err error
}

// stopWait is how long a stop may take before the service reports stopped
// anyway; the capture is closed first, well within it.
const stopWait = 15 * time.Second

func (h *scmHandler) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.run(ctx) }()
	running := svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	status <- running
	for {
		select {
		case err := <-done:
			h.err = err
			if err != nil {
				return false, 1
			}
			return false, 0
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending, WaitHint: uint32(stopWait / time.Millisecond)}
				cancel()
				select {
				case h.err = <-done:
				case <-time.After(stopWait):
					h.err = errors.New("the service took too long to stop")
				}
				return false, 0
			}
		}
	}
}
