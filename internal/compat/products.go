package compat

import (
	"strings"
	"unicode"
)

// Product is a program Lowpath knows competes with it, and how to find it.
// Matching works on normalized names (lower case, letters and digits only),
// so "ExitLag.exe", "ExitLag Service" and "ExitLag" all match "exitlag".
type Product struct {
	ID   string
	Name string
	Kind Kind
	// Keys match a process, service or installed program whose normalized
	// name contains one of them. Only long, distinctive words belong here.
	Keys []string
	// Exact match a normalized process name (without ".exe"), service name
	// or program name exactly, for short names such as "haste".
	Exact []string
	// Running is the level when the program's process runs; ServiceRunning
	// when only its service or driver does.
	Running, ServiceRunning Level
	// Why explains what the program does to game traffic; action is built
	// from Name unless Action is set.
	Why    string
	Action string
}

const boosterWhy = "%s reroutes game traffic through its own servers, like Lowpath does. Two programs rerouting the same packets fight over them: each packet may go through %s, through Lowpath, or get lost, and neither program's ping is right any more. Lowpath doesn't start route mode while %s runs, and never fights another driver for the game's packets."

// boosters are the game boosters and game VPNs players most often have
// installed next to Lowpath. ExitLag and NoPing capture with the Windows
// Packet Filter (WinpkFilter) driver.
func boosters() []Product {
	b := func(id, name string, keys, exact []string) Product {
		return Product{ID: id, Name: name, Kind: KindBooster, Keys: keys, Exact: exact, Running: LevelBlock, ServiceRunning: LevelWarn, Why: boosterWhy}
	}
	return []Product{
		b("exitlag", "ExitLag", []string{"exitlag"}, nil),
		b("noping", "NoPing", []string{"noping"}, nil),
		b("wtfast", "WTFast", []string{"wtfast"}, nil),
		b("mudfish", "Mudfish", []string{"mudfish"}, nil),
		b("lagofast", "LagoFast", []string{"lagofast"}, nil),
		b("gearup", "GearUP Booster", []string{"gearupbooster", "gearupgame"}, nil),
		b("haste", "Haste", nil, []string{"haste", "hastesvc"}),
		b("pingzapper", "Pingzapper", []string{"pingzapper"}, nil),
		b("battleping", "Battleping", []string{"battleping"}, nil),
		b("killping", "Kill Ping", []string{"killping"}, nil),
		b("outfox", "Outfox", nil, []string{"outfox"}),
	}
}

// packetTools rewrite packets with WinDivert, the driver Lowpath uses.
// GoodbyeDPI and zapret change web traffic (TCP 80/443, QUIC) and usually
// leave game UDP alone, so they only warn; clumsy delays and drops
// everything it matches on purpose.
func packetTools() []Product {
	const dpiWhy = "%s rewrites packets with WinDivert, the same packet driver Lowpath uses. It usually only changes web traffic, but whichever program takes a packet first decides where it goes, so %s can still interfere with the game's packets. If the boost misbehaves while %s runs, close it and try again."
	return []Product{
		{ID: "goodbyedpi", Name: "GoodbyeDPI", Kind: KindPacketTool, Keys: []string{"goodbyedpi"}, Running: LevelWarn, ServiceRunning: LevelWarn, Why: dpiWhy},
		{ID: "zapret", Name: "zapret", Kind: KindPacketTool, Keys: []string{"zapret"}, Exact: []string{"winws"}, Running: LevelWarn, ServiceRunning: LevelWarn, Why: dpiWhy},
		{ID: "clumsy", Name: "clumsy", Kind: KindPacketTool, Exact: []string{"clumsy"}, Running: LevelBlock, ServiceRunning: LevelBlock,
			Why:    "%s delays, drops and reorders packets on purpose with WinDivert, the same packet driver Lowpath uses. While %s runs, the game's packets are degraded before Lowpath can route them, and Lowpath's numbers would measure %s rather than your connection.",
			Action: "Close clumsy first (or press Stop in it)."},
	}
}

// winpkFilter is the Windows Packet Filter driver (ndisrd), an NDIS
// lightweight filter that ExitLag and NoPing install and often leave behind.
// It does nothing on its own; a program using it sees every packet first.
func winpkFilter() Product {
	return Product{
		ID: "winpkfilter", Name: "Windows Packet Filter", Kind: KindPacketFilter,
		Keys: []string{"winpkfilter", "windowspacketfilter"}, Exact: []string{"ndisrd"},
		Running: LevelWarn, ServiceRunning: LevelWarn,
		Why: "%s (WinpkFilter, driver ndisrd) is a packet filter driver that ExitLag and NoPing install. It does nothing on its own, but a program that uses it sees every packet before Lowpath does, and its uninstaller sometimes leaves it behind. If a game connects badly only while it is loaded, remove it.",
		Action: "If you no longer use ExitLag or NoPing, uninstall \"Windows Packet Filter\" in Settings → Apps → Installed apps, then restart the PC.",
	}
}

// matchName reports whether a process, service or program name is this
// product's.
func (p Product) matchName(name string) bool {
	n := normalize(name)
	if n == "" {
		return false
	}
	for _, e := range p.Exact {
		if n == e || n == e+"exe" {
			return true
		}
	}
	for _, k := range p.Keys {
		if strings.Contains(n, k) {
			return true
		}
	}
	return false
}

func (p Product) action() string {
	if p.Action != "" {
		return p.Action
	}
	if p.Kind == KindBooster {
		return "Close " + p.Name + " first: right-click its icon next to the clock and choose Exit, or end it in Task Manager."
	}
	return "Close " + p.Name + " first."
}

func (p Product) why() string {
	return strings.ReplaceAll(p.Why, "%s", p.Name)
}

// normalize lower-cases s and keeps letters and digits only.
func normalize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}
