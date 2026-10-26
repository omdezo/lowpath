// Package catalog knows where game servers are before the game connects.
//
// Version 1 reads Valve's Steam Datagram Relay (SDR) network config, the
// list of relay points of presence (POPs) that CS2, Dota 2 and Deadlock
// route their matches through, cleans up its coordinates, and measures a
// direct ping from this PC to each POP.
package catalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"regexp"
	"slices"
	"strings"

	"lowpath/internal/relay"
)

// Limits on what a config may contain. The real one has about 50 POPs with
// at most 14 relays each; anything far beyond that is not Valve's data.
const (
	maxPOPs         = 400
	maxRelaysPerPOP = 64
	maxDescLen      = 80
	maxConfigBytes  = 4 << 20
)

// SDRRelay is one relay of a POP: the address the game would send to, and
// the UDP ports it listens on.
type SDRRelay struct {
	Addr      netip.Addr
	PortFirst uint16 // 0 when the config gave no valid range
	PortLast  uint16
}

// POP is one SDR point of presence (a data center Valve relays through).
type POP struct {
	ID       string   // "dxb", "bom2", ...
	Aliases  []string // older ids for the same POP ("bom")
	Desc     string   // as Valve wrote it: "Dubai (United Arab Emirates)"
	City     string   // "Dubai"
	Country  string   // "United Arab Emirates"; "" when the desc has none
	Geo      Geo      // after Sanitize: where the POP really is, or unknown
	GeoFix   GeoFix   // what Sanitize changed
	Tier     int
	Partners int
	Relays   []SDRRelay
}

// SDRConfig is the parsed network config of one Steam app.
type SDRConfig struct {
	AppID    int
	Revision uint64
	POPs     []POP // sorted by id
}

// ErrNotSDR is returned for a reply that is valid JSON but not an SDR config
// (Steam answers {"success":false} for an app without one).
var ErrNotSDR = errors.New("not an SDR network config")

var popIDRe = regexp.MustCompile(`^[a-z0-9]{2,12}$`)

type rawConfig struct {
	Success  *bool             `json:"success"`
	Message  string            `json:"message"`
	Revision uint64            `json:"revision"`
	POPs     map[string]rawPOP `json:"pops"`
}

type rawPOP struct {
	Desc     string     `json:"desc"`
	Geo      []float64  `json:"geo"`
	Partners int        `json:"partners"`
	Tier     int        `json:"tier"`
	Aliases  []string   `json:"aliases"`
	Relays   []rawRelay `json:"relays"`
}

type rawRelay struct {
	IPv4      string `json:"ipv4"`
	IPv6      string `json:"ipv6"`
	PortRange []int  `json:"port_range"`
}

// ParseSDRConfig reads the JSON GetSDRConfig returns. It keeps only what it
// can trust: POPs with a sane id, public relay addresses (a hostile or
// broken reply must not make Lowpath ping the user's own network), valid
// port ranges and coordinates checked by Sanitize.
func ParseSDRConfig(appID int, b []byte) (*SDRConfig, error) {
	if len(b) > maxConfigBytes {
		return nil, fmt.Errorf("SDR config is too large (%d bytes)", len(b))
	}
	var raw rawConfig
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("SDR config: %w", err)
	}
	if raw.Success != nil && !*raw.Success {
		if raw.Message != "" {
			return nil, fmt.Errorf("%w: %s", ErrNotSDR, raw.Message)
		}
		return nil, ErrNotSDR
	}
	if len(raw.POPs) == 0 {
		return nil, fmt.Errorf("%w: no POPs", ErrNotSDR)
	}
	if len(raw.POPs) > maxPOPs {
		return nil, fmt.Errorf("SDR config lists %d POPs, more than %d", len(raw.POPs), maxPOPs)
	}
	cfg := &SDRConfig{AppID: appID, Revision: raw.Revision}
	// Visit the ids in a fixed order: "DXB" and "dxb" are the same POP once
	// lowercased, and which one is kept must not depend on map order.
	ids := slices.Sorted(maps.Keys(raw.POPs))
	seen := make(map[string]bool, len(ids))
	for _, key := range ids {
		rp := raw.POPs[key]
		id := strings.ToLower(strings.TrimSpace(key))
		if !popIDRe.MatchString(id) || seen[id] {
			continue
		}
		seen[id] = true
		p := POP{ID: id, Tier: rp.Tier, Partners: rp.Partners}
		for _, a := range rp.Aliases {
			if a = strings.ToLower(strings.TrimSpace(a)); popIDRe.MatchString(a) && len(p.Aliases) < 8 {
				p.Aliases = append(p.Aliases, a)
			}
		}
		p.Desc = cleanDesc(rp.Desc)
		if p.Desc == "" {
			p.Desc = id
		}
		p.City, p.Country = splitDesc(p.Desc)
		p.Geo = rawGeo(rp.Geo)
		for _, rr := range rp.Relays {
			if len(p.Relays) == maxRelaysPerPOP {
				break
			}
			first, last := portRange(rr.PortRange)
			for _, s := range []string{rr.IPv4, rr.IPv6} {
				a, err := netip.ParseAddr(strings.TrimSpace(s))
				if err != nil || !relay.IsPublic(a) {
					continue
				}
				a = a.Unmap()
				if slices.ContainsFunc(p.Relays, func(r SDRRelay) bool { return r.Addr == a }) {
					continue
				}
				p.Relays = append(p.Relays, SDRRelay{Addr: a, PortFirst: first, PortLast: last})
			}
		}
		cfg.POPs = append(cfg.POPs, p)
	}
	if len(cfg.POPs) == 0 {
		return nil, fmt.Errorf("%w: no usable POPs", ErrNotSDR)
	}
	slices.SortFunc(cfg.POPs, func(a, b POP) int { return strings.Compare(a.ID, b.ID) })
	Sanitize(cfg.POPs)
	return cfg, nil
}

// cleanDesc keeps printable text of a sane length.
func cleanDesc(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if r := []rune(s); len(r) > maxDescLen {
		s = string(r[:maxDescLen])
	}
	return strings.TrimSpace(s)
}

// splitDesc turns "Mumbai (India)" into "Mumbai", "India". Descs without a
// country ("Singapore", "Multiplay Washington DC") are all city.
func splitDesc(d string) (city, country string) {
	if i := strings.LastIndex(d, " ("); i > 0 && strings.HasSuffix(d, ")") {
		return strings.TrimSpace(d[:i]), strings.TrimSpace(d[i+2 : len(d)-1])
	}
	return d, ""
}

func portRange(r []int) (uint16, uint16) {
	if len(r) != 2 || r[0] < 1 || r[1] > 65535 || r[0] > r[1] {
		return 0, 0
	}
	return uint16(r[0]), uint16(r[1])
}
