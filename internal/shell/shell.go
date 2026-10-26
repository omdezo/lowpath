// Package shell wires the app for the programs that run it: the desktop app
// (cmd/lowpath), which runs the engine itself or puts its window in front of
// the background service, and the service (cmd/lowpathsvc). Both build the
// same app.Server from the same options and keep the same log and crash
// files.
package shell

import (
	"context"
	"io/fs"
	"log/slog"
	"path/filepath"
	"time"

	"lowpath/internal/app"
	"lowpath/internal/capture"
	"lowpath/internal/catalog"
	"lowpath/internal/demo"
	"lowpath/internal/diag"
	"lowpath/internal/platform"
)

// Options are what the command line decides about the app.
type Options struct {
	Data        string // settings, relays, log and caches
	Relays      string // optional read-only relay list (JSON)
	BloatServer string // load server for the bufferbloat test; "" = diag.DefaultLoadURL
	Demo        bool   // the simulated network instead of real relays
	// TraceCount and TraceInterval shorten route traces (tests).
	TraceCount    int
	TraceInterval time.Duration
	UI            fs.FS // the built UI; nil: API only (the service)
	// Elevate restarts the app with administrator rights; nil: no restart
	// button.
	Elevate func() error
	// InService: the server runs inside the background service.
	InService bool
	// ServiceNote: see app.Config.ServiceNote.
	ServiceNote string
}

// Build wires the API server to the store, catalog, relays and the
// platform layer without starting background work (app.Server.Run), so
// tests can run it on a fake platform.
func Build(ctx context.Context, o Options, plat *platform.Platform, logger *slog.Logger, quit func()) (*app.Server, error) {
	store, err := app.OpenStore(filepath.Join(o.Data, "state.json"))
	if err != nil {
		return nil, err
	}
	games, err := app.LoadCatalog()
	if err != nil {
		return nil, err
	}
	var imp *app.RelayImport
	if o.Relays != "" {
		if imp, err = app.LoadRelayImport(o.Relays); err != nil {
			return nil, err
		}
		logger.Info("imported", "file", o.Relays, "relays", len(imp.Relays), "games", len(imp.Games))
	}
	relays, err := app.NewRelayNetwork(store, imp, logger)
	if err != nil {
		return nil, err
	}
	// Valve's SDR POP lists for CS2, Dota 2 and Deadlock: downloaded from
	// Steam when a game page opens, kept on disk for offline starts, pinged
	// from this PC.
	servers := catalog.NewValveServers(catalog.ValveOptions{
		Source: catalog.NewSDRSource(nil, catalog.SteamAPI),
		Prober: catalog.NewPOPProber(catalog.ICMPPinger{}, nil),
		Cache:  catalog.NewDirCache(filepath.Join(o.Data, "catalog")),
		Log:    logger.With("component", "catalog"),
	})
	cfg := app.Config{Store: store, Games: games, Relays: relays, Servers: servers, Platform: plat, UI: o.UI, Log: logger, Quit: quit, ArtDir: filepath.Join(o.Data, "art"),
		TraceCount: o.TraceCount, TraceInterval: o.TraceInterval, Elevate: o.Elevate, InService: o.InService, ServiceNote: o.ServiceNote,
		// While route mode captures, this file says so; finding it at the
		// next start means that run was killed or crashed.
		Marker: capture.NewMarkerFile(filepath.Join(o.Data, "capture-session.json")),
	}
	if o.BloatServer != "" {
		l, err := diag.NewHTTPLoader(diag.HTTPLoaderConfig{BaseURL: o.BloatServer, UserAgent: "Lowpath/" + app.Version + " (bufferbloat test)"})
		if err != nil {
			servers.Close()
			relays.Close()
			return nil, err
		}
		cfg.BloatLoader = l
	}
	if o.Demo {
		sim, err := demo.Start(ctx, demo.DefaultPaths(), demo.DirectProfile(), logger.With("component", "relay"))
		if err != nil {
			servers.Close()
			relays.Close()
			return nil, err
		}
		cfg.Network, cfg.Sim = app.DemoNetwork{N: sim}, sim
		logger.Info("demo network up", "game_server", sim.GameAddr(), "egress", sim.Egress.Addr(), "paths", len(sim.Paths))
	}
	srv, err := app.NewServer(cfg)
	if err != nil {
		servers.Close()
		relays.Close()
		return nil, err
	}
	return srv, nil
}
