package compat

import (
	"slices"
	"strings"
	"time"
)

// Kind says what sort of software a conflict is.
type Kind string

const (
	// KindBooster is another game booster or game VPN (ExitLag, NoPing,
	// WTFast, ...): it reroutes the same game packets Lowpath does.
	KindBooster Kind = "booster"
	// KindPacketTool is a program that rewrites packets with WinDivert
	// (GoodbyeDPI, zapret, clumsy).
	KindPacketTool Kind = "packet_tool"
	// KindPacketFilter is a packet filter driver on its own: WinpkFilter
	// (ndisrd), which ExitLag and NoPing install, or a WinDivert driver
	// another program loaded.
	KindPacketFilter Kind = "packet_filter"
	// KindVPN is a VPN that carries all internet traffic (full tunnel).
	KindVPN Kind = "vpn"
)

// Level is how much a conflict matters to a boost.
type Level string

const (
	// LevelInfo: installed but idle. Harmless while it stays closed; named
	// so the player knows it is there.
	LevelInfo Level = "info"
	// LevelWarn: active, and it can make the boost worse or the numbers
	// wrong, but it doesn't take the game's packets away from Lowpath.
	LevelWarn Level = "warn"
	// LevelBlock: running and capturing the same packets. Route mode won't
	// start next to it unless the player says to; two programs fighting
	// over one packet stream break the game's connection.
	LevelBlock Level = "block"
)

func (l Level) rank() int {
	switch l {
	case LevelBlock:
		return 3
	case LevelWarn:
		return 2
	case LevelInfo:
		return 1
	}
	return 0
}

// Conflict is one program, driver or VPN that competes with Lowpath, in
// words the UI shows as they are.
type Conflict struct {
	ID    string `json:"id"`   // stable: "exitlag", "winpkfilter", "windivert", "vpn"
	Name  string `json:"name"` // "ExitLag", "NordVPN", ...
	Kind  Kind   `json:"kind"`
	Level Level  `json:"level"`
	// Found is the evidence, as phrases: "ExitLag.exe is running",
	// "installed".
	Found []string `json:"found"`
	// Action is what to do, one sentence ("Close ExitLag first: ...").
	Action string `json:"action"`
	// Why is the explanation behind the action, for the "Why?" link.
	Why string `json:"why"`
}

// Report is the result of one scan.
type Report struct {
	At        time.Time  `json:"at"`
	Conflicts []Conflict `json:"conflicts"`
	// Unchecked lists the checks that couldn't run, with the reason
	// ("installed programs: access denied"), so "nothing found" is never
	// claimed for something that wasn't looked at.
	Unchecked []string `json:"unchecked"`
}

// Blocking returns the conflicts that keep route mode from starting.
func (r Report) Blocking() []Conflict {
	var out []Conflict
	for _, c := range r.Conflicts {
		if c.Level == LevelBlock {
			out = append(out, c)
		}
	}
	return out
}

// Active returns the conflicts that are running now (warn or block), the
// ones that can explain a capture failure.
func (r Report) Active() []Conflict {
	var out []Conflict
	for _, c := range r.Conflicts {
		if c.Level.rank() >= LevelWarn.rank() {
			out = append(out, c)
		}
	}
	return out
}

// Redacted is the report with user folder names cut out of the evidence,
// for the diagnostics export a player may post publicly.
func (r Report) Redacted() Report {
	out := r
	out.Conflicts = make([]Conflict, len(r.Conflicts))
	for i, c := range r.Conflicts {
		c.Found = slices.Clone(c.Found)
		for j, f := range c.Found {
			c.Found[j] = redactUser(f)
		}
		out.Conflicts[i] = c
	}
	out.Unchecked = slices.Clone(r.Unchecked)
	return out
}

// redactUser replaces the folder name after \Users\ or /home/ (the account
// name, often a real name) with "…".
func redactUser(s string) string {
	for _, marker := range []string{`\users\`, "/users/", "/home/"} {
		for from := 0; ; {
			i := strings.Index(strings.ToLower(s[from:]), marker)
			if i < 0 {
				break
			}
			start := from + i + len(marker)
			end := strings.IndexAny(s[start:], `\/`)
			if end < 0 {
				end = len(s) - start
			}
			s = s[:start] + "…" + s[start+end:]
			from = start + len("…")
		}
	}
	return s
}

// Names lists the conflicts' names for a sentence: "ExitLag", "ExitLag and
// NoPing", "ExitLag, NoPing and WTFast".
func Names(cs []Conflict) string {
	names := make([]string, 0, len(cs))
	for _, c := range cs {
		names = append(names, c.Name)
	}
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}
