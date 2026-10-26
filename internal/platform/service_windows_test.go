package platform

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

func testService(name string, elevated bool) *WindowsService {
	s := NewWindowsService()
	s.name = name
	s.elevated = func() bool { return elevated }
	return s
}

// Querying the service manager needs no administrator rights: a service
// that isn't installed is "not installed", not an error.
func TestWindowsServiceNotInstalled(t *testing.T) {
	s := testService("LowpathSvc-test-not-installed", false)
	st, err := s.State()
	if err != nil || st.Installed || st.Running || st.PID != 0 {
		t.Fatalf("State = %+v, %v; want not installed", st, err)
	}
	if _, err := s.PID(); !errors.Is(err, ErrServiceNotRunning) {
		t.Errorf("PID: %v, want ErrServiceNotRunning", err)
	}
	if err := s.Start(); err == nil {
		t.Error("Start of a service that isn't installed succeeded")
	}
}

// A service that runs (the event log always does) is read the same way an
// unelevated window reads LowpathSvc.
func TestWindowsServiceState(t *testing.T) {
	s := testService("EventLog", false)
	st, err := s.State()
	if err != nil {
		t.Fatal(err)
	}
	if !st.Installed || !st.Running || st.PID == 0 || !strings.EqualFold(filepath.Base(st.Exe), "svchost.exe") {
		t.Fatalf("EventLog state %+v, want installed, running, a PID and svchost.exe", st)
	}
	if pid, err := s.PID(); err != nil || pid != st.PID {
		t.Errorf("PID = %d, %v; want %d", pid, err, st.PID)
	}
}

func TestWindowsServiceNeedsAdmin(t *testing.T) {
	s := testService("LowpathSvc-test-needs-admin", false)
	if err := s.Install(ServiceConfig{Exe: os.Args[0]}); !errors.Is(err, ErrNeedAdmin) {
		t.Errorf("Install without admin: %v, want ErrNeedAdmin", err)
	}
	if err := s.Uninstall(); !errors.Is(err, ErrNeedAdmin) {
		t.Errorf("Uninstall without admin: %v, want ErrNeedAdmin", err)
	}
}

// The security descriptors parse, and the service's lets interactive users
// query and start it, but not stop, reconfigure or delete it.
func TestServiceSecurityDescriptors(t *testing.T) {
	for _, sddl := range []string{pipeSDDL, serviceSDDL, dataDirSDDL} {
		if _, err := windows.SecurityDescriptorFromString(sddl); err != nil {
			t.Errorf("%s: %v", sddl, err)
		}
	}
	sd, _ := windows.SecurityDescriptorFromString(serviceSDDL)
	iu, _ := windows.CreateWellKnownSid(windows.WinInteractiveSid)
	mask := aceMasks(t, sd)[iu.String()]
	want := uint32(windows.SERVICE_START | windows.SERVICE_QUERY_STATUS | windows.SERVICE_QUERY_CONFIG)
	if mask&want != want {
		t.Errorf("Interactive Users %#x: can't query and start", mask)
	}
	for _, bad := range []uint32{windows.SERVICE_STOP, windows.SERVICE_CHANGE_CONFIG, windows.DELETE, windows.WRITE_DAC, windows.WRITE_OWNER} {
		if mask&bad != 0 {
			t.Errorf("Interactive Users %#x include %#x", mask, bad)
		}
	}
	sd, _ = windows.SecurityDescriptorFromString(dataDirSDDL)
	for sid := range aceMasks(t, sd) {
		s, _ := windows.StringToSid(sid)
		if !s.IsWellKnown(windows.WinLocalSystemSid) && !s.IsWellKnown(windows.WinBuiltinAdministratorsSid) {
			t.Errorf("data folder grants %s", sid)
		}
	}
}

func TestCommandLine(t *testing.T) {
	got := commandLine(`C:\Program Files\Lowpath\lowpathsvc.exe`, []string{"-data", `C:\ProgramData\Lowpath`, `a "b"`})
	args, err := windows.DecomposeCommandLine(got)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`C:\Program Files\Lowpath\lowpathsvc.exe`, "-data", `C:\ProgramData\Lowpath`, `a "b"`}
	if strings.Join(args, "|") != strings.Join(want, "|") {
		t.Errorf("commandLine round trip %q, want %q", args, want)
	}
}

// A data folder this user made (as one could in ProgramData before the
// install) or a link is not trusted; the install moves it aside.
func TestTrustedDir(t *testing.T) {
	dir := t.TempDir()
	fi, _ := os.Lstat(dir)
	trusted, err := trustedDir(dir, fi)
	if err != nil {
		t.Fatal(err)
	}
	if trusted && !windows.GetCurrentProcessToken().IsElevated() {
		t.Error("a folder owned by this (unelevated) user is trusted")
	}
	if sys, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, 0); err == nil { // owned by SYSTEM
		fi, _ := os.Lstat(sys)
		if ok, err := trustedDir(sys, fi); err != nil || !ok {
			t.Errorf("%s: trusted %v, %v; want trusted", sys, ok, err)
		}
	}
	file := filepath.Join(dir, "f")
	os.WriteFile(file, nil, 0o600)
	fi, _ = os.Lstat(file)
	if ok, _ := trustedDir(file, fi); ok {
		t.Error("a file is trusted as a folder")
	}
}

func TestWriteSeed(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "kept.json"), []byte("service's own"), 0o600)
	err := writeSeed(dir, map[string][]byte{"state.json": []byte("user's"), "kept.json": []byte("user's")})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "state.json")); string(b) != "user's" {
		t.Errorf("state.json = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "kept.json")); string(b) != "service's own" {
		t.Errorf("an existing file was overwritten: %q", b)
	}
	if err := writeSeed(dir, map[string][]byte{`..\x.json`: nil}); err == nil {
		t.Error("a seed name with a path was written")
	}
}

func TestSCMRunnerOutsideService(t *testing.T) {
	ok, err := NewServiceRunner().Run("LowpathSvc-test", func(context.Context) error {
		t.Error("run called outside the service manager")
		return nil
	})
	if ok || err != nil {
		t.Errorf("Run = %v, %v; want false, nil from a test process", ok, err)
	}
}

// The handler reports Running, answers Interrogate, and on Stop reports
// StopPending, ends run's context and waits for it.
func TestSCMHandlerStop(t *testing.T) {
	stopped := make(chan struct{})
	h := &scmHandler{run: func(ctx context.Context) error {
		<-ctx.Done()
		close(stopped)
		return nil
	}}
	req := make(chan svc.ChangeRequest)
	status := make(chan svc.Status, 10)
	done := make(chan uint32)
	go func() { _, code := h.Execute(nil, req, status); done <- code }()

	if s := <-status; s.State != svc.StartPending {
		t.Fatalf("first status %v", s.State)
	}
	running := <-status
	if running.State != svc.Running || running.Accepts&svc.AcceptStop == 0 || running.Accepts&svc.AcceptShutdown == 0 {
		t.Fatalf("running status %+v", running)
	}
	req <- svc.ChangeRequest{Cmd: svc.Interrogate, CurrentStatus: running}
	if s := <-status; s.State != svc.Running {
		t.Fatalf("interrogate answered %v", s.State)
	}
	req <- svc.ChangeRequest{Cmd: svc.Stop}
	if s := <-status; s.State != svc.StopPending || s.WaitHint == 0 {
		t.Fatalf("after Stop: %+v", s)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit code %d after a stop", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Execute did not return after Stop")
	}
	select {
	case <-stopped:
	default:
		t.Error("run's context did not end")
	}
}

// A service whose run fails exits with a code, so the recovery actions
// restart it.
func TestSCMHandlerRunFails(t *testing.T) {
	h := &scmHandler{run: func(context.Context) error { return errors.New("pipe taken") }}
	status := make(chan svc.Status, 10)
	_, code := h.Execute(nil, make(chan svc.ChangeRequest), status)
	if code == 0 || h.err == nil {
		t.Errorf("exit code %d, err %v; want non-zero and the error", code, h.err)
	}
}
