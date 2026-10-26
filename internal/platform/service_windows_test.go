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
	trusted, err := trustedDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if trusted {
		t.Error("a folder this user owns is trusted")
	}
	// ProgramData is SYSTEM's, but every user may create folders and files
	// in the folders under it: a folder that inherited that is not trusted.
	if sys, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, 0); err == nil {
		if ok, err := trustedDir(sys); err != nil || ok {
			t.Errorf("%s: trusted %v, %v; want not trusted", sys, ok, err)
		}
	}
	file := filepath.Join(dir, "f")
	os.WriteFile(file, nil, 0o600)
	if ok, _ := trustedDir(file); ok {
		t.Error("a file is trusted as a folder")
	}
	if _, err := trustedDir(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing folder checked without an error")
	}
}

// Only a folder that SYSTEM or Administrators own and only they can change
// is kept as the service's data folder.
func TestAdminsOnly(t *testing.T) {
	user := windows.GetCurrentProcessToken()
	tu, err := user.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	me := tu.User.Sid.String()
	for _, tc := range []struct {
		sddl string
		want bool
	}{
		{"O:BA" + dataDirSDDL, true},
		{"O:SY" + dataDirSDDL, true},
		{"O:BAD:P(A;OICI;FA;;;SY)(D;OICI;FA;;;WD)(A;OICI;FA;;;BA)", true},   // a deny ACE opens nothing
		{"O:" + me + dataDirSDDL, false},                                    // a user's folder
		{"O:BAD:(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)", false},                   // inherits from its parent
		{"O:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;CI;0x116;;;BU)", false}, // ProgramData's "Users may create"
		{"O:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;" + me + ")", false},
		{"O:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;CO)", false},
		{"O:BAD:P", true}, // nobody at all but the owner's implicit rights
		{"O:BA", false},   // no DACL: everyone has full access
	} {
		sd, err := windows.SecurityDescriptorFromString(tc.sddl)
		if err != nil {
			t.Fatalf("%s: %v", tc.sddl, err)
		}
		if got := adminsOnly(sd); got != tc.want {
			t.Errorf("%s: adminsOnly = %v, want %v", tc.sddl, got, tc.want)
		}
	}
}

// The data folder is created with its access list in the same call, never
// first with the one it would inherit; a folder already there is reported,
// not adopted.
func TestCreateDir(t *testing.T) {
	// The test's own access list keeps the owner (this user) able to clean
	// up; the real one names only SYSTEM and Administrators.
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;OW)")
	if err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	// Make the parent grant Users "create files" the way ProgramData does.
	psd, _ := windows.SecurityDescriptorFromString("D:(A;OICI;FA;;;OW)(A;OICI;FA;;;SY)(A;CI;0x116;;;BU)")
	pdacl, _, _ := psd.DACL()
	if err := windows.SetNamedSecurityInfo(parent, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, pdacl, nil); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(parent, "Lowpath")
	created, err := createDir(dir, sd)
	if err != nil || !created {
		t.Fatalf("createDir = %v, %v", created, err)
	}
	got, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	ctl, _, _ := got.Control()
	if ctl&windows.SE_DACL_PROTECTED == 0 {
		t.Error("the new folder's access list is not protected")
	}
	if s := got.String(); strings.Contains(s, ";;;BU)") {
		t.Errorf("the new folder inherited Users' rights: %s", s)
	}
	if created, err := createDir(dir, sd); err != nil || created {
		t.Errorf("second createDir = %v, %v; want not created", created, err)
	}
	if created, err := createDir(filepath.Join(parent, "no", "such"), sd); err == nil || created {
		t.Errorf("createDir in a missing folder = %v, %v", created, err)
	}
}

// Securing a folder somebody else made first moves it aside instead of
// taking it over. Unelevated, setting the Administrators owner then fails,
// but only after the move.
func TestSecureDirMovesUntrustedAside(t *testing.T) {
	if windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("elevated: this would secure a temporary folder for real")
	}
	parent := t.TempDir()
	dir := filepath.Join(parent, "Lowpath")
	os.Mkdir(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "state.json"), []byte("planted"), 0o600)
	err := secureDirWith(dir, "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;OW)")
	if err == nil {
		t.Fatal("an unelevated process made Administrators an owner")
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json")); err == nil {
		t.Error("the planted state.json is still in the data folder")
	}
	moved, _ := filepath.Glob(dir + ".untrusted-*")
	if len(moved) != 1 {
		t.Errorf("moved aside: %v", moved)
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
