package app

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"strings"

	"lowpath/internal/demo"
	"lowpath/internal/engine"
)

//go:embed games.json
var gamesJSON []byte

type Game struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Genre     string   `json:"genre"`
	Color     string   `json:"color"`
	Smart     bool     `json:"smart"`
	Processes []string `json:"processes,omitempty"`
	Paths     []string `json:"paths,omitempty"`
	Steam     string   `json:"steam,omitempty"`     // folder name in a Steam library
	SteamApp  int      `json:"steam_app,omitempty"` // Steam app id: library artwork
	IconExe   []string `json:"icon_exe,omitempty"`  // programs whose icon to show, before Processes
	Exe       string   `json:"exe,omitempty"`       // custom games: the program's path when it was added
	Custom    bool     `json:"custom"`              // added by the user from a running process
}

func LoadCatalog() ([]Game, error) {
	var g []Game
	err := json.Unmarshal(gamesJSON, &g)
	return g, err
}

// Network answers "which relay paths exist for this game, and where is its
// server". Route mode only needs the paths (the server is whatever the game
// talks to); proxy mode and the Analyze baseline also need the endpoint.
type Network interface {
	Demo() bool
	Paths(gameID string) []engine.PathSpec
	Endpoint(gameID string) (target, direct netip.AddrPort, ok bool)
}

// DemoNetwork routes every game to the simulated server on localhost.
type DemoNetwork struct{ N *demo.Network }

func (d DemoNetwork) Demo() bool                     { return true }
func (d DemoNetwork) Paths(string) []engine.PathSpec { return d.N.Paths }
func (d DemoNetwork) Endpoint(string) (netip.AddrPort, netip.AddrPort, bool) {
	return d.N.GameAddr(), d.N.DirectAddr(), true
}

// RelayImport is a read-only relay list loaded with -relays, for relays
// managed outside the app (a team's shared fleet, say):
//
//	{"relays": [{"id":"fra-1","label":"Frankfurt","region":"EU-Central",
//	             "continent":"Europe","addr":"203.0.113.10:40000",
//	             "key":"...","egress":""}],
//	 "games":  {"cs2": {"server": "198.51.100.20:27015"}}}
//
// "egress" names the id of the egress relay a forwarding hop sends to; ""
// means the relay is an egress itself. "games" gives proxy mode a server; a
// file may hold only games, to use proxy mode with the app's own relays.
type RelayImport struct {
	Relays []ImportedRelay `json:"relays"`
	Games  map[string]struct {
		Server netip.AddrPort `json:"server"`
	} `json:"games"`
}

type ImportedRelay struct {
	ID        string         `json:"id"`
	Label     string         `json:"label"`
	Region    string         `json:"region"`
	Continent string         `json:"continent"`
	Addr      netip.AddrPort `json:"addr"`
	Key       string         `json:"key"`
	Egress    string         `json:"egress"`
}

func LoadRelayImport(path string) (*RelayImport, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c RelayImport
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(c.Relays) == 0 && len(c.Games) == 0 {
		return nil, fmt.Errorf("%s: no relays or games defined", path)
	}
	seen := map[string]bool{loopbackID: true}
	for i, r := range c.Relays {
		if r.ID == "" || !r.Addr.IsValid() {
			return nil, fmt.Errorf("%s: relay %d needs id and addr", path, i)
		}
		if seen[r.ID] {
			return nil, fmt.Errorf("%s: relay id %q is used twice (or reserved)", path, r.ID)
		}
		seen[r.ID] = true
		if r.Label == "" {
			c.Relays[i].Label = r.ID
		}
	}
	for _, r := range c.Relays {
		if r.Egress != "" && !seen[r.Egress] {
			return nil, fmt.Errorf("%s: relay %q forwards to unknown egress %q", path, r.ID, r.Egress)
		}
	}
	c.Games = lowerKeys(c.Games) // game ids are lower case; the file may not be
	return &c, nil
}

func lowerKeys[V any](m map[string]V) map[string]V {
	out := make(map[string]V, len(m))
	for k, v := range m {
		out[strings.ToLower(k)] = v
	}
	return out
}
