package fakeplatform

import (
	"errors"
	"net/netip"
	"testing"

	"lowpath/internal/capture"
	"lowpath/internal/compat"
	"lowpath/internal/diag"
	"lowpath/internal/owner"
	"lowpath/internal/platform"
)

var (
	_ platform.CaptureBackend  = (*Capture)(nil)
	_ platform.CaptureSession  = (*Session)(nil)
	_ platform.OwnerResolver   = (*Owners)(nil)
	_ platform.Privileges      = (*Privileges)(nil)
	_ platform.GameDetector    = (*Games)(nil)
	_ platform.IconProvider    = (*Icons)(nil)
	_ platform.Paths           = (*Paths)(nil)
	_ platform.SystemInventory = (*Inventory)(nil)
)

func TestFakeRoundTrip(t *testing.T) {
	f := New(t.TempDir())
	if err := f.Platform().Check(); err != nil {
		t.Fatal(err)
	}
	local, remote := netip.MustParseAddrPort("10.0.0.2:5000"), netip.MustParseAddrPort("203.0.113.9:27015")
	var seen string
	s, err := f.Capture.Start(capture.Config{OnPacket: func(l, r netip.AddrPort, p []byte) bool {
		seen = string(p)
		return true
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !f.Capture.Last().Send(local, remote, "hello") || seen != "hello" {
		t.Fatalf("Send did not reach OnPacket (%q)", seen)
	}
	if err := s.Inject(local, remote, []byte("reply")); err != nil {
		t.Fatal(err)
	}
	if got := <-f.Capture.Last().Injected(); got != (Injected{local, remote, "reply"}) {
		t.Fatalf("injected %+v", got)
	}
	if st := s.Stats(); st.Captured != 1 || st.Injected != 1 {
		t.Fatalf("stats %+v", st)
	}
	s.Close()
	if err := s.Inject(local, remote, nil); !errors.Is(err, capture.ErrClosed) || f.Capture.Last().Send(local, remote, "x") {
		t.Fatal("closed session still works")
	}

	f.Capture.SetAvailable(capture.ErrNeedAdmin)
	if _, err := f.Capture.Start(capture.Config{}); !errors.Is(err, capture.ErrNeedAdmin) {
		t.Fatalf("unavailable Start: %v", err)
	}
}

func TestFakeParts(t *testing.T) {
	f := New(t.TempDir())
	f.Owners.Set([]owner.Socket{{Local: netip.MustParseAddrPort("0.0.0.0:7000"), PID: 10}}, map[uint32]string{10: "cs2.exe"})
	if procs, err := owner.ListProcesses(f.Owners, 1); err != nil || len(procs) != 1 || procs[0].Name != "cs2.exe" {
		t.Fatalf("processes %v, %v", procs, err)
	}
	f.Games.SetRunning("CS2.exe")
	f.Games.SetExe("cs2.exe", "/games/cs2.exe")
	if !f.Games.Running()["cs2.exe"] || f.Games.RunningExe([]string{"Cs2.exe"}) != "/games/cs2.exe" {
		t.Fatal("running game not reported")
	}
	if err := f.Privileges.Relaunch([]string{"-x"}); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("Relaunch without a prompt: %v", err)
	}
	f.Privileges.SetCanElevate(true)
	f.Privileges.SetRelaunchErr(platform.ErrElevateCancelled)
	if err := f.Privileges.Relaunch([]string{"-x"}); !errors.Is(err, platform.ErrElevateCancelled) || len(f.Privileges.Relaunches()) != 1 {
		t.Fatalf("declined prompt: %v", err)
	}
}

func TestFakeInventory(t *testing.T) {
	inv := New(t.TempDir()).Inventory
	if _, err := inv.DefaultRoute(); !errors.Is(err, diag.ErrNoRoute) {
		t.Errorf("empty inventory: route %v, want offline", err)
	}
	inv.Set(compat.Inventory{Processes: []string{"exitlag.exe"}, DefaultRoute: 3})
	denied := errors.New("access denied")
	inv.SetErr("programs", denied)
	if p, err := inv.Processes(); err != nil || len(p) != 1 {
		t.Errorf("Processes = %q, %v", p, err)
	}
	if _, err := inv.Programs(); err != denied {
		t.Errorf("Programs: %v", err)
	}
	if i, err := inv.DefaultRoute(); i != 3 || err != nil {
		t.Errorf("DefaultRoute = %d, %v", i, err)
	}
}
