package compat

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"lowpath/internal/diag"
)

type noProcs struct{}

func (noProcs) Running() map[string]bool { return map[string]bool{"lowpath.test.exe": true} }

// Every part reads without administrator rights. Assertions stay on what
// every Windows PC has, and nothing found is printed: the test runs on
// players' and contributors' own PCs.
func TestWindowsSystemWithoutAdmin(t *testing.T) {
	sys := NewSystem(noProcs{}, diag.NewNetStats())

	svcs, err := sys.Services()
	if err != nil {
		t.Fatalf("Services: %v", err)
	}
	i := slices.IndexFunc(svcs, func(s Service) bool { return strings.EqualFold(s.Name, "Tcpip") })
	if i < 0 || !svcs[i].Driver || !svcs[i].Running {
		t.Errorf("the TCP/IP driver is missing or not seen as a running driver (%d services)", len(svcs))
	}
	if !slices.ContainsFunc(svcs, func(s Service) bool { return !s.Driver && s.Running && s.Display != "" }) {
		t.Error("no running Win32 service with a display name")
	}

	if _, err := sys.Programs(); err != nil {
		t.Errorf("Programs: %v", err)
	}

	ads, err := sys.Adapters()
	if err != nil {
		t.Fatalf("Adapters: %v", err)
	}
	if !slices.ContainsFunc(ads, func(a Adapter) bool { return a.Type == 24 && a.Index > 0 && a.Name != "" }) {
		t.Errorf("no loopback adapter among %d", len(ads))
	}

	if p, _ := sys.Processes(); !slices.Equal(p, []string{"lowpath.test.exe"}) {
		t.Errorf("Processes = %q", p)
	}

	start := time.Now()
	r := NewDetector(Config{System: sys}).Scan(context.Background())
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("scan took %v", d)
	}
	for _, u := range r.Unchecked {
		if !strings.HasPrefix(u, "the WinDivert driver's folder") {
			t.Errorf("unchecked without admin: %s", u)
		}
	}
}
