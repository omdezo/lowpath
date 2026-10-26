package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"lowpath/internal/app"
	"lowpath/internal/compat"
	"lowpath/internal/platform"
	"lowpath/internal/shell"
)

// service is one run of the API behind the service's pipe.
type service struct {
	plat *platform.Platform
	api  platform.ServiceAPI
	o    runOptions
}

func newService(plat *platform.Platform, o runOptions) *service {
	api := plat.ServiceAPI
	if o.pipe != "" && o.pipe != api.String() {
		api = platform.ServiceAPIAt(o.pipe)
	}
	if o.compatInventory != "" {
		// The end-to-end test plays its own boosters and drivers; the PC's
		// real software doesn't change its results.
		p := *plat
		p.Inventory = compat.NewFileSystem(o.compatInventory)
		plat = &p
	}
	return &service{plat: plat, api: api, o: o}
}

// shutdownWait bounds letting in-flight requests (a live stream, an
// analysis) finish after the session has stopped.
const shutdownWait = 3 * time.Second

// serve runs the engine and the API until ctx ends or serving fails. On the
// way out the session stops first, so capture is closed and the game is on
// its normal connection before anything else shuts down.
func (s *service) serve(ctx context.Context) error {
	logger, closeLog, err := shell.OpenLog(s.o.data)
	if err != nil {
		return err
	}
	defer closeLog()
	defer shell.KeepCrashes(s.o.data, logger)()

	// Take the pipe before starting anything: if another process holds it,
	// fail at once (the service manager retries) rather than run a second
	// engine nobody can reach.
	ln, err := s.api.Listen()
	if err != nil {
		logger.Error("can't serve the API", "api", s.api.String(), "err", err)
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	srv, err := shell.Build(ctx, shell.Options{Data: s.o.data, Relays: s.o.relays, InService: true}, s.plat, logger, cancel)
	if err != nil {
		ln.Close()
		logger.Error("can't start", "err", err)
		return err
	}
	srv.Run(ctx)

	hs := &http.Server{Handler: srv.ServiceHandler(), ReadHeaderTimeout: 5 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- hs.Serve(ln) }()
	logger.Info("Lowpath service is running", "api", s.api.String(), "version", app.Version, "pid", os.Getpid(),
		"os", s.plat.OS, "capture", s.plat.Capture.Name(), "admin", s.plat.Privileges.Elevated(), "data", s.o.data)

	select {
	case <-ctx.Done():
		err = nil
	case err = <-serveErr:
		err = fmt.Errorf("serving the API: %w", err)
	}
	logger.Info("Lowpath service is stopping")
	srv.Close()
	shut, done := context.WithTimeout(context.Background(), shutdownWait)
	defer done()
	if serr := hs.Shutdown(shut); serr != nil {
		hs.Close()
	}
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	logger.Info("Lowpath service stopped")
	return err
}
