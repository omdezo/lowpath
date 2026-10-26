package compat

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"lowpath/internal/diag"
)

// Detector turns what a System reports into conflicts.
type Detector struct {
	sys      System
	ownDirs  []string
	products []Product
	now      func() time.Time
}

// Config wires a Detector.
type Config struct {
	System System
	// OwnDriverDirs are the folders Lowpath loads its WinDivert driver
	// from: a WinDivert driver loaded from anywhere else belongs to another
	// program.
	OwnDriverDirs []string
	Now           func() time.Time // nil: time.Now
}

// NewDetector returns a detector for the known boosters, packet tools,
// WinpkFilter, other WinDivert users and full-tunnel VPNs.
func NewDetector(cfg Config) *Detector {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	products := append(boosters(), packetTools()...)
	products = append(products, winpkFilter())
	return &Detector{sys: cfg.System, ownDirs: cfg.OwnDriverDirs, products: products, now: cfg.Now}
}

// Scan reads the system and reports every conflict found, worst first. It
// never fails: a part of the system that can't be read is listed in
// Report.Unchecked and the rest is still checked. Scan stops early (with
// what it has) when ctx ends.
func (d *Detector) Scan(ctx context.Context) Report {
	var inv Inventory
	var unchecked []string
	note := func(what string, err error) bool {
		switch {
		case err == nil:
			return true
		case errors.Is(err, errors.ErrUnsupported):
			// This OS doesn't have it (no services on Linux): nothing to
			// check, so nothing to say.
		default:
			unchecked = append(unchecked, fmt.Sprintf("%s: %v", what, err))
		}
		return false
	}
	steps := []func(){
		func() {
			p, err := d.sys.Processes()
			if note("running programs", err) {
				inv.Processes = p
			}
		},
		func() {
			s, err := d.sys.Services()
			if note("services and drivers", err) {
				inv.Services = s
			}
		},
		func() {
			p, err := d.sys.Programs()
			if note("installed programs", err) {
				inv.Programs = p
			}
		},
		func() {
			a, err := d.sys.Adapters()
			if note("network adapters", err) {
				inv.Adapters = a
			}
		},
		func() {
			i, err := d.sys.DefaultRoute()
			if errors.Is(err, diag.ErrNoRoute) {
				return // offline: no route for a VPN to take
			}
			if note("the internet route", err) {
				inv.DefaultRoute = i
			}
		},
	}
	for _, step := range steps {
		if ctx.Err() != nil {
			unchecked = append(unchecked, "the check was cut short: "+context.Cause(ctx).Error())
			break
		}
		step()
	}
	r := d.Evaluate(inv)
	r.Unchecked = append(r.Unchecked, unchecked...)
	return r
}

// Evaluate applies the rules to an inventory.
func (d *Detector) Evaluate(inv Inventory) Report {
	var cs []Conflict
	for _, p := range d.products {
		if c, ok := d.product(p, inv); ok {
			cs = append(cs, c)
		}
	}
	var unchecked []string
	if c, ok, why := d.foreignDivert(inv.Services); ok {
		cs = append(cs, c)
	} else if why != "" {
		unchecked = append(unchecked, why)
	}
	if c, ok := vpnConflict(inv); ok {
		cs = append(cs, c)
	}
	slices.SortStableFunc(cs, func(a, b Conflict) int { return b.Level.rank() - a.Level.rank() })
	if cs == nil {
		cs = []Conflict{}
	}
	if unchecked == nil {
		unchecked = []string{}
	}
	return Report{At: d.now(), Conflicts: cs, Unchecked: unchecked}
}

// product finds one known program: its process (worst), its running
// service, or it only being installed.
func (d *Detector) product(p Product, inv Inventory) (Conflict, bool) {
	c := Conflict{ID: p.ID, Name: p.Name, Kind: p.Kind, Action: p.action(), Why: p.why()}
	raise := func(l Level, found string) {
		if l.rank() > c.Level.rank() {
			c.Level = l
		}
		if !slices.Contains(c.Found, found) {
			c.Found = append(c.Found, found)
		}
	}
	running := false
	for _, name := range sortedCopy(inv.Processes) {
		if p.matchName(strings.TrimSuffix(strings.ToLower(name), ".exe")) {
			raise(p.Running, name+" is running")
			running = true
		}
	}
	for _, s := range inv.Services {
		if !p.matchName(s.Name) && !p.matchName(s.Display) {
			continue
		}
		what := "service"
		if s.Driver {
			what = "driver"
		}
		label := s.Name
		if s.Display != "" && !strings.EqualFold(s.Display, s.Name) {
			label = s.Display + " (" + s.Name + ")"
		}
		if s.Running {
			raise(p.ServiceRunning, "its "+what+" "+label+" is running")
		} else {
			raise(LevelInfo, "its "+what+" "+label+" is installed")
		}
	}
	for _, prog := range inv.Programs {
		if p.matchName(prog) {
			raise(LevelInfo, "installed ("+prog+")")
		}
	}
	if c.Level == "" {
		return Conflict{}, false
	}
	switch {
	case p.Kind == KindPacketFilter:
	case c.Level == LevelInfo:
		c.Action = "Nothing to do while it stays closed; don't run it at the same time as Lowpath."
	case !running && p.Kind == KindBooster:
		// Only its background service runs (ExitLag keeps one that starts
		// with Windows): the app itself is closed.
		c.Action = "Keep " + p.Name + " itself closed while you boost. If the game still connects badly, stop its service or uninstall " + p.Name + "."
	}
	return c, true
}

// foreignDivert finds a WinDivert driver another program loaded. WinDivert
// registers its driver as a service named "WinDivert" (1.x versions as
// "WinDivert1.x") pointing at the .sys file it loaded; the service goes
// away at the next restart. why is set when a running WinDivert service's
// file couldn't be read, so it can't be told apart from Lowpath's own.
func (d *Detector) foreignDivert(svcs []Service) (c Conflict, ok bool, why string) {
	for _, s := range svcs {
		if !s.Running || !strings.HasPrefix(strings.ToLower(s.Name), "windivert") {
			continue
		}
		path := cleanDriverPath(s.Path)
		if path == "" {
			why = "the WinDivert driver's folder: it couldn't be read"
			continue
		}
		if d.ours(path) {
			continue
		}
		return Conflict{
			ID: "windivert", Name: "Another WinDivert program", Kind: KindPacketFilter, Level: LevelWarn,
			Found:  []string{"the WinDivert driver was loaded from " + filepath.Dir(path)},
			Action: "If a program such as GoodbyeDPI, zapret or clumsy still runs, close it. If none does, this clears when you restart the PC.",
			Why:    "WinDivert is the packet driver Lowpath uses too. Two programs can use it at once, but whichever takes the game's packets first decides where they go, and if the other program loaded a different WinDivert version, route mode can't start until the PC restarts.",
		}, true, ""
	}
	return Conflict{}, false, why
}

// ours reports whether a driver file is in one of Lowpath's driver folders.
func (d *Detector) ours(path string) bool {
	dir := filepath.Clean(filepath.Dir(path))
	for _, o := range d.ownDirs {
		if strings.EqualFold(dir, filepath.Clean(o)) {
			return true
		}
	}
	return false
}

// cleanDriverPath turns a service ImagePath into a file path: drivers are
// often registered as \??\C:\..., \SystemRoot\... or quoted.
func cleanDriverPath(p string) string {
	p = strings.TrimSpace(strings.Trim(strings.TrimSpace(p), `"`))
	for _, pre := range []string{`\??\`, `\\?\`} {
		p = strings.TrimPrefix(p, pre)
	}
	return p
}

// vpnConflict reports a VPN that carries the internet route. A VPN that
// only reaches an office or a game LAN (split tunnel, Tailscale without an
// exit node) doesn't touch the game's traffic and isn't one.
func vpnConflict(inv Inventory) (Conflict, bool) {
	if inv.DefaultRoute == 0 {
		return Conflict{}, false
	}
	for _, a := range inv.Adapters {
		if a.Index != inv.DefaultRoute || !a.Up {
			continue
		}
		name := vpnName(a)
		if name == "" {
			return Conflict{}, false
		}
		found := "all internet traffic goes through " + adapterLabel(a)
		label := name
		if name == "a VPN" {
			label = "Your VPN"
		}
		return Conflict{
			ID: "vpn", Name: upperFirst(label), Kind: KindVPN, Level: LevelWarn,
			Found:  []string{found},
			Action: "Turn the VPN off while you play, or leave Lowpath and the game out of it (split tunnelling).",
			Why:    "A full VPN carries all of this PC's internet traffic. Lowpath's relay traffic then goes through the VPN's server first, which adds its delay to every packet, and the before and after numbers measure the VPN rather than your connection. Some VPNs also block traffic that tries to leave outside the tunnel.",
		}, true
	}
	return Conflict{}, false
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func sortedCopy(s []string) []string {
	c := slices.Clone(s)
	slices.Sort(c)
	return c
}
