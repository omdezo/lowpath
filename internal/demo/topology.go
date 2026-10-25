// Package demo wires a complete simulated network on localhost: one game
// server, one egress relay next to it, several impaired "carrier" paths into
// that relay, and an impaired direct "ISP" route. It lets the real client,
// relay and dedup code run end-to-end on a single machine.
package demo

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"time"

	"lowpath/internal/engine"
	"lowpath/internal/gamesim"
	"lowpath/internal/netsim"
	"lowpath/internal/relay"
)

type PathDef struct {
	Spec    engine.PathSpec
	Profile netsim.Profile
}

const ms = time.Millisecond

// DefaultPaths models a player in Europe whose ISP has poor peering to the
// game's datacenter. Profiles are one-way.
func DefaultPaths() []PathDef {
	return []PathDef{
		{engine.PathSpec{ID: "eu-west-a", Label: "EU-West via Carrier A", Region: "EU-West", Continent: "Europe"},
			netsim.Profile{Delay: 27 * ms, Jitter: 1500 * time.Microsecond, Loss: 0.004, SpikeEvery: 37 * time.Second, SpikeLen: 2 * time.Second, SpikeExtra: 35 * ms, SpikeLoss: 0.25}},
		{engine.PathSpec{ID: "eu-west-b", Label: "EU-West via Carrier B", Region: "EU-West", Continent: "Europe"},
			netsim.Profile{Delay: 29 * ms, Jitter: 2 * ms, Loss: 0.006}},
		{engine.PathSpec{ID: "eu-central", Label: "EU-Central (Frankfurt)", Region: "EU-Central", Continent: "Europe"},
			netsim.Profile{Delay: 32 * ms, Jitter: 1 * ms, Loss: 0.002}},
		{engine.PathSpec{ID: "eu-north", Label: "EU-North (Stockholm)", Region: "EU-North", Continent: "Europe"},
			netsim.Profile{Delay: 37 * ms, Jitter: 2500 * time.Microsecond, Loss: 0.004}},
		{engine.PathSpec{ID: "na-east", Label: "NA-East (Virginia)", Region: "NA-East", Continent: "North America"},
			netsim.Profile{Delay: 68 * ms, Jitter: 2 * ms, Loss: 0.003}},
	}
}

// DirectProfile is the player's normal ISP route: slower, jittery, lossy and
// with periodic congestion spikes.
func DirectProfile() netsim.Profile {
	return netsim.Profile{Delay: 44 * ms, Jitter: 7 * ms, Loss: 0.012, SpikeEvery: 23 * time.Second, SpikeLen: 2 * time.Second, SpikeExtra: 70 * ms, SpikeLoss: 0.12}
}

type Network struct {
	Game   *gamesim.Server
	Egress *relay.Egress
	Direct *netsim.Link
	Links  map[string]*netsim.Link
	Paths  []engine.PathSpec
}

// Start brings the network up on 127.0.0.1 with random ports.
func Start(ctx context.Context, paths []PathDef, direct netsim.Profile, log *slog.Logger) (*Network, error) {
	n := &Network{Links: map[string]*netsim.Link{}}
	var err error
	if n.Game, err = gamesim.Listen("127.0.0.1:0"); err != nil {
		return nil, err
	}
	go n.Game.Serve(ctx)

	if n.Egress, err = relay.ListenEgress("127.0.0.1:0", log); err != nil {
		return nil, err
	}
	go n.Egress.Serve(ctx)

	if n.Direct, err = netsim.Listen("127.0.0.1:0", n.Game.Addr(), direct); err != nil {
		return nil, err
	}
	go n.Direct.Serve(ctx)

	for _, d := range paths {
		l, err := netsim.Listen("127.0.0.1:0", n.Egress.Addr(), d.Profile)
		if err != nil {
			return nil, fmt.Errorf("path %s: %w", d.Spec.ID, err)
		}
		go l.Serve(ctx)
		s := d.Spec
		s.Addr = l.Addr()
		n.Links[s.ID] = l
		n.Paths = append(n.Paths, s)
	}
	return n, nil
}

func (n *Network) GameAddr() netip.AddrPort   { return n.Game.Addr() }
func (n *Network) DirectAddr() netip.AddrPort { return n.Direct.Addr() }
