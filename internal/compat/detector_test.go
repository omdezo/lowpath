package compat

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"lowpath/internal/diag"
)

// static is a System that answers with one inventory, plus per-part errors.
type static struct {
	inv  Inventory
	errs map[string]error
}

func (s static) Processes() ([]string, error) { return s.inv.Processes, s.errs["processes"] }
func (s static) Services() ([]Service, error) { return s.inv.Services, s.errs["services"] }
func (s static) Programs() ([]string, error)  { return s.inv.Programs, s.errs["programs"] }
func (s static) Adapters() ([]Adapter, error) { return s.inv.Adapters, s.errs["adapters"] }
func (s static) DefaultRoute() (int, error) {
	if err := s.errs["route"]; err != nil {
		return 0, err
	}
	if s.inv.DefaultRoute == 0 {
		return 0, diag.ErrNoRoute
	}
	return s.inv.DefaultRoute, nil
}

var ownDir = filepath.Join("C:", "Program Files", "Lowpath", "driver")

func detector(sys System) *Detector {
	return NewDetector(Config{System: sys, OwnDriverDirs: []string{ownDir}, Now: func() time.Time { return time.Unix(1700000000, 0) }})
}

func scan(inv Inventory) Report { return detector(static{inv: inv}).Scan(context.Background()) }

func find(r Report, id string) (Conflict, bool) {
	i := slices.IndexFunc(r.Conflicts, func(c Conflict) bool { return c.ID == id })
	if i < 0 {
		return Conflict{}, false
	}
	return r.Conflicts[i], true
}

func ids(r Report) []string {
	var out []string
	for _, c := range r.Conflicts {
		out = append(out, c.ID+":"+string(c.Level))
	}
	return out
}

// ethernet is a plain home connection: the default route on Ethernet.
var ethernet = Adapter{Index: 7, Name: "Ethernet", Description: "Example Gigabit Network Connection", Type: 6, Up: true}

func TestCleanSystemHasNoConflicts(t *testing.T) {
	r := scan(Inventory{
		Processes:    []string{"steam.exe", "cs2.exe", "chrome.exe", "discord.exe", "svchost.exe", "lowpath.exe", "explorer.exe"},
		Services:     []Service{{Name: "Tcpip", Driver: true, Running: true}, {Name: "Dnscache", Display: "DNS Client", Running: true}},
		Programs:     []string{"Steam", "Discord", "Microsoft Edge", "Lowpath"},
		Adapters:     []Adapter{ethernet, {Index: 1, Name: "Loopback Pseudo-Interface 1", Type: 24, Up: true}},
		DefaultRoute: 7,
	})
	if len(r.Conflicts) != 0 || len(r.Unchecked) != 0 {
		t.Fatalf("clean system: conflicts %v, unchecked %v", ids(r), r.Unchecked)
	}
	if r.Conflicts == nil || r.Unchecked == nil {
		t.Error("empty lists must encode as [], not null")
	}
	if !r.At.Equal(time.Unix(1700000000, 0)) {
		t.Errorf("At = %v", r.At)
	}
}

// The acceptance case: with ExitLag running, Boost has to name it; with it
// only installed, the UI still does.
func TestExitLagRunningBlocksAndInstalledIsNamed(t *testing.T) {
	r := scan(Inventory{Processes: []string{"exitlag.exe"}, Programs: []string{"ExitLag"}})
	c, ok := find(r, "exitlag")
	if !ok || c.Level != LevelBlock || c.Name != "ExitLag" || c.Kind != KindBooster {
		t.Fatalf("ExitLag running: %+v (%v)", c, ids(r))
	}
	if !slices.Contains(c.Found, "exitlag.exe is running") || !slices.Contains(c.Found, "installed (ExitLag)") {
		t.Errorf("Found = %q", c.Found)
	}
	if !strings.HasPrefix(c.Action, "Close ExitLag first") || !strings.Contains(c.Why, "ExitLag") || strings.Contains(c.Why, "%s") {
		t.Errorf("Action %q, Why %q", c.Action, c.Why)
	}
	if b := r.Blocking(); len(b) != 1 || b[0].ID != "exitlag" {
		t.Errorf("Blocking = %v", b)
	}

	r = scan(Inventory{Programs: []string{"ExitLag"}, Services: []Service{{Name: "ExitLagSvc", Display: "ExitLag Service"}}})
	c, ok = find(r, "exitlag")
	if !ok || c.Level != LevelInfo || len(r.Blocking()) != 0 || len(r.Active()) != 0 {
		t.Fatalf("ExitLag installed: %+v (%v)", c, ids(r))
	}
	if !slices.Contains(c.Found, "its service ExitLag Service (ExitLagSvc) is installed") || !strings.Contains(c.Action, "Nothing to do") {
		t.Errorf("installed: Found %q, Action %q", c.Found, c.Action)
	}

	r = scan(Inventory{Services: []Service{{Name: "ExitLagSvc", Running: true}}})
	if c, _ := find(r, "exitlag"); c.Level != LevelWarn || !slices.Contains(c.Found, "its service ExitLagSvc is running") || !strings.HasPrefix(c.Action, "Keep ExitLag itself closed") {
		t.Errorf("service only: %+v", c)
	}
}

func TestBoostersAndToolsByName(t *testing.T) {
	for _, tc := range []struct {
		proc, id string
		level    Level
	}{
		{"NoPing.exe", "noping", LevelBlock},
		{"WTFast.exe", "wtfast", LevelBlock},
		{"mudfish.exe", "mudfish", LevelBlock},
		{"LagoFast.exe", "lagofast", LevelBlock},
		{"GearUP Booster.exe", "gearup", LevelBlock},
		{"Haste.exe", "haste", LevelBlock},
		{"pingzapper.exe", "pingzapper", LevelBlock},
		{"clumsy.exe", "clumsy", LevelBlock},
		{"goodbyedpi.exe", "goodbyedpi", LevelWarn},
		{"winws.exe", "zapret", LevelWarn},
	} {
		r := scan(Inventory{Processes: []string{strings.ToLower(tc.proc)}})
		if c, ok := find(r, tc.id); !ok || c.Level != tc.level || len(r.Conflicts) != 1 {
			t.Errorf("%s: %v, want only %s:%s", tc.proc, ids(r), tc.id, tc.level)
		}
	}
	// Short names match whole names only.
	for _, p := range []string{"hastebin.exe", "chaste.exe", "clumsybird.exe", "winwsx.exe", "outfoxed.exe"} {
		if r := scan(Inventory{Processes: []string{p}}); len(r.Conflicts) != 0 {
			t.Errorf("%s matched %v", p, ids(r))
		}
	}
}

func TestWinpkFilterDriver(t *testing.T) {
	r := scan(Inventory{Services: []Service{{Name: "ndisrd", Display: "WinpkFilter Service", Driver: true, Running: true}}})
	c, ok := find(r, "winpkfilter")
	if !ok || c.Level != LevelWarn || c.Kind != KindPacketFilter || !strings.Contains(c.Action, "uninstall") {
		t.Fatalf("loaded: %+v", c)
	}
	if !slices.Contains(c.Found, "its driver WinpkFilter Service (ndisrd) is running") {
		t.Errorf("Found = %q", c.Found)
	}
	r = scan(Inventory{Services: []Service{{Name: "NDISRD", Driver: true}}, Programs: []string{"Windows Packet Filter 3.6.2"}})
	if c, _ := find(r, "winpkfilter"); c.Level != LevelInfo || !strings.Contains(c.Action, "uninstall") || len(c.Found) != 2 {
		t.Errorf("left behind: %+v", c)
	}
}

func TestForeignWinDivert(t *testing.T) {
	ours := filepath.Join(ownDir, "WinDivert64.sys")
	for _, tc := range []struct {
		name      string
		svc       Service
		want      bool
		unchecked bool
	}{
		{"Lowpath's own", Service{Name: "WinDivert", Driver: true, Running: true, Path: `\??\` + ours}, false, false},
		{"Lowpath's own, other case", Service{Name: "WinDivert", Driver: true, Running: true, Path: strings.ToUpper(ours)}, false, false},
		{"another program", Service{Name: "WinDivert", Driver: true, Running: true, Path: `\??\C:\Tools\goodbyedpi\x86_64\WinDivert64.sys`}, true, false},
		{"old version", Service{Name: "WinDivert1.4", Driver: true, Running: true, Path: `"C:\Games\Tool\WinDivert64.sys"`}, true, false},
		{"unloaded", Service{Name: "WinDivert", Driver: true, Path: `C:\Tools\WinDivert64.sys`}, false, false},
		{"unreadable", Service{Name: "WinDivert", Driver: true, Running: true}, false, true},
	} {
		r := scan(Inventory{Services: []Service{tc.svc}})
		c, ok := find(r, "windivert")
		if ok != tc.want || (len(r.Unchecked) > 0) != tc.unchecked {
			t.Errorf("%s: %v, unchecked %q", tc.name, ids(r), r.Unchecked)
			continue
		}
		if ok && (c.Level != LevelWarn || !strings.Contains(c.Found[0], `C:\`) || strings.Contains(c.Found[0], `\??\`)) {
			t.Errorf("%s: %+v", tc.name, c)
		}
	}
}

func TestVPNOnlyWhenItCarriesTheInternet(t *testing.T) {
	wg := Adapter{Index: 12, Name: "office", Description: "WireGuard Tunnel", Type: 53, Up: true}
	for _, tc := range []struct {
		name     string
		adapters []Adapter
		route    int
		want     string // conflict name, "" for none
	}{
		{"WireGuard full tunnel", []Adapter{ethernet, wg}, 12, "WireGuard"},
		{"split tunnel", []Adapter{ethernet, wg}, 7, ""},
		{"offline", []Adapter{ethernet, wg}, 0, ""},
		{"VPN adapter down", []Adapter{{Index: 12, Name: "office", Description: "WireGuard Tunnel", Type: 53}}, 12, ""},
		{"NordVPN", []Adapter{{Index: 3, Name: "NordLynx", Description: "NordLynx Tunnel", Type: 53, Up: true}}, 3, "NordVPN"},
		{"OpenVPN TAP", []Adapter{{Index: 4, Name: "Ethernet 3", Description: "TAP-Windows Adapter V9", Type: 6, Up: true}}, 4, "OpenVPN"},
		{"Windows VPN client", []Adapter{{Index: 5, Name: "Work", Description: "WAN Miniport (IKEv2)", Type: 23, Up: true}}, 5, "Your VPN"},
		{"PPPoE DSL is not a VPN", []Adapter{{Index: 5, Name: "Broadband", Description: "WAN Miniport (PPPOE)", Type: 23, Up: true}}, 5, ""},
		{"Teredo is not a VPN", []Adapter{{Index: 9, Name: "Teredo Tunneling Pseudo-Interface", Type: 131, Up: true}}, 9, ""},
		{"Linux wg0", []Adapter{{Index: 4, Name: "wg0", Up: true}}, 4, "Your VPN"},
		{"macOS utun", []Adapter{{Index: 15, Name: "utun4", Up: true}}, 15, "Your VPN"},
		{"Linux eth0", []Adapter{{Index: 2, Name: "eth0", Up: true}}, 2, ""},
		{"a name that only starts like one", []Adapter{{Index: 2, Name: "tunnelbridge", Up: true}}, 2, ""},
	} {
		r := scan(Inventory{Adapters: tc.adapters, DefaultRoute: tc.route})
		c, ok := find(r, "vpn")
		if got := c.Name; ok != (tc.want != "") || got != tc.want {
			t.Errorf("%s: %v (%q), want %q", tc.name, ids(r), got, tc.want)
			continue
		}
		if ok && (c.Level != LevelWarn || c.Kind != KindVPN || len(c.Found) != 1 || !strings.Contains(c.Action, "split tunnelling")) {
			t.Errorf("%s: %+v", tc.name, c)
		}
	}
}

func TestWorstFirst(t *testing.T) {
	r := scan(Inventory{
		Processes:    []string{"noping.exe"},
		Programs:     []string{"WTFast"},
		Services:     []Service{{Name: "ndisrd", Driver: true, Running: true}},
		Adapters:     []Adapter{{Index: 3, Name: "NordLynx", Type: 53, Up: true}},
		DefaultRoute: 3,
	})
	levels := []Level{}
	for _, c := range r.Conflicts {
		levels = append(levels, c.Level)
	}
	if !slices.Equal(levels, []Level{LevelBlock, LevelWarn, LevelWarn, LevelInfo}) || r.Conflicts[0].ID != "noping" {
		t.Errorf("order: %v", ids(r))
	}
	if Names(r.Conflicts) != "NoPing, Windows Packet Filter, NordVPN and WTFast" || Names(r.Conflicts[:1]) != "NoPing" || Names(r.Conflicts[:2]) != "NoPing and Windows Packet Filter" || Names(nil) != "" {
		t.Errorf("Names: %q", Names(r.Conflicts))
	}
	if len(r.Active()) != 3 {
		t.Errorf("Active = %v", r.Active())
	}
}

// A part that can't be read is said so; the rest is still checked. A part
// the OS doesn't have is skipped quietly; offline isn't a failure.
func TestPartsFailOnTheirOwn(t *testing.T) {
	denied := fmt.Errorf("open key: %w", errors.New("Access is denied."))
	sys := static{
		inv: Inventory{Processes: []string{"exitlag.exe"}},
		errs: map[string]error{
			"programs": denied,
			"services": fmt.Errorf("x: %w", errors.ErrUnsupported),
			"route":    diag.ErrNoRoute,
		},
	}
	r := detector(sys).Scan(context.Background())
	if _, ok := find(r, "exitlag"); !ok {
		t.Errorf("ExitLag lost: %v", ids(r))
	}
	if len(r.Unchecked) != 1 || !strings.HasPrefix(r.Unchecked[0], "installed programs: ") || !strings.Contains(r.Unchecked[0], "Access is denied") {
		t.Errorf("Unchecked = %q", r.Unchecked)
	}
	sys.errs = map[string]error{"route": errors.New("GetBestInterfaceEx failed"), "adapters": errors.New("boom")}
	if r := detector(sys).Scan(context.Background()); len(r.Unchecked) != 2 {
		t.Errorf("Unchecked = %q", r.Unchecked)
	}
}

func TestScanStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := detector(static{inv: Inventory{Processes: []string{"exitlag.exe"}}}).Scan(ctx)
	if len(r.Conflicts) != 0 || len(r.Unchecked) != 1 || !strings.Contains(r.Unchecked[0], "cut short") {
		t.Errorf("cancelled scan: %v %q", ids(r), r.Unchecked)
	}
}

func TestFileSystem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inventory.json")
	d := detector(NewFileSystem(path))
	if r := d.Scan(context.Background()); len(r.Conflicts) != 0 || len(r.Unchecked) != 0 {
		t.Fatalf("missing file: %v %q", ids(r), r.Unchecked)
	}
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"processes":["ExitLag.exe"],"adapters":[{"index":12,"name":"office","description":"WireGuard Tunnel","type":53,"up":true}],"default_route":12}`)
	r := d.Scan(context.Background())
	if !slices.Equal(ids(r), []string{"exitlag:block", "vpn:warn"}) {
		t.Errorf("from file: %v %q", ids(r), r.Unchecked)
	}
	write(`{`)
	if r := d.Scan(context.Background()); len(r.Conflicts) != 0 || len(r.Unchecked) != 5 {
		t.Errorf("broken file: %v %q", ids(r), r.Unchecked)
	}
}

func TestRedacted(t *testing.T) {
	r := Report{Conflicts: []Conflict{{ID: "windivert", Found: []string{`the WinDivert driver was loaded from C:\Users\Example User\Downloads\tool`, "/home/example/bin and /Users/other/x"}}}}
	got := r.Redacted().Conflicts[0].Found
	if got[0] != `the WinDivert driver was loaded from C:\Users\…\Downloads\tool` || got[1] != "/home/…/bin and /Users/…/x" {
		t.Errorf("Redacted = %q", got)
	}
	if !strings.Contains(r.Conflicts[0].Found[0], "Example User") {
		t.Error("Redacted changed the original")
	}
}

func TestProductTable(t *testing.T) {
	d := NewDetector(Config{System: static{}})
	seen := map[string]bool{}
	for _, p := range d.products {
		if p.ID == "" || p.Name == "" || p.Why == "" || seen[p.ID] {
			t.Errorf("product %+v: missing field or duplicate id", p)
		}
		seen[p.ID] = true
		if p.matchName("Lowpath") || p.matchName("lowpath.exe") {
			t.Errorf("%s matches Lowpath itself", p.ID)
		}
		for _, k := range append(slices.Clone(p.Keys), p.Exact...) {
			if k != normalize(k) {
				t.Errorf("%s: key %q is not normalized", p.ID, k)
			}
		}
		if strings.Contains(p.action(), "%") || strings.Contains(p.why(), "%") {
			t.Errorf("%s: unfilled text", p.ID)
		}
	}
}
