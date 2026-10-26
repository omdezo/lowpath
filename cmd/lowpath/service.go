package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"time"

	"lowpath/internal/app"
	"lowpath/internal/platform"
	"lowpath/ui"
)

const (
	// serviceStartWait is how long a service this process just started
	// gets to answer. The engine starts in well under a second; the
	// driver loads at the first Boost, not here.
	serviceStartWait = 20 * time.Second
	// handPipeWait is how long a service run by hand (-service-pipe) gets.
	handPipeWait = 5 * time.Second
	// checkTimeout bounds one version check.
	checkTimeout = 2 * time.Second
)

// serviceAPI is the channel to the service: the installed one's, or the
// one -service-pipe names. Naming the installed one's pipe keeps its
// server check.
func serviceAPI(o options, plat *platform.Platform) platform.ServiceAPI {
	if o.servicePipe != "" && o.servicePipe != plat.ServiceAPI.String() {
		return platform.ServiceAPIAt(o.servicePipe)
	}
	return plat.ServiceAPI
}

// connectService returns the window's frontend for the background service,
// or nil when this process should run the engine itself: with -demo or
// -no-service, when no service is installed, or when it doesn't answer.
// With nil, note says why an installed service isn't used, for the
// Settings row and the log ("" for -demo, whose row says so itself, and
// when there is no service). A stopped service is started first; the
// install lets signed-in users do that without a prompt. It fails only
// when -service-pipe names a service that doesn't answer.
func connectService(ctx context.Context, o options, plat *platform.Platform, logger *slog.Logger) (fe *app.Frontend, note string, err error) {
	if o.demo {
		return nil, "", nil
	}
	if o.noService {
		return nil, "it was started with -no-service", nil
	}
	api := serviceAPI(o, plat)
	var wait time.Duration // 0: one check
	expected := true       // false: a service may answer, but none is known to be there
	if api != plat.ServiceAPI {
		wait = handPipeWait
	} else {
		st, err := plat.Service.State()
		switch {
		case errors.Is(err, errors.ErrUnsupported):
			// No service manager support here, but a service started by
			// hand (or by an init system) may still listen.
			expected = false
		case err != nil:
			logger.Warn("can't ask the service manager about the Lowpath service", "err", err)
		case !st.Installed:
			return nil, "", nil
		case !st.Running:
			logger.Info("starting the Lowpath service")
			if err := plat.Service.Start(); err != nil {
				logger.Warn("can't start the Lowpath service; running the engine in this window", "err", err)
				return nil, fmt.Sprintf("Lowpath couldn't start it (%v)", err), nil
			}
			wait = serviceStartWait
		}
	}

	dist, err := fs.Sub(ui.Dist, "dist")
	if err != nil {
		logger.Warn("no UI to serve in front of the service", "err", err)
		return nil, fmt.Sprintf("this build has no UI to show in front of it (%v)", err), nil
	}
	fe, err = app.NewFrontend(app.FrontendConfig{Dial: api.Dial, UI: dist, Log: logger})
	if err != nil {
		logger.Warn("can't use the Lowpath service", "err", err)
		return nil, fmt.Sprintf("Lowpath couldn't connect to it (%v)", err), nil
	}
	ver, err := waitForService(ctx, fe, wait)
	if err != nil {
		fe.Close()
		if api != plat.ServiceAPI {
			// Asked for by name: running an engine here instead would
			// hide that the service the caller wanted isn't there.
			return nil, "", fmt.Errorf("no Lowpath service answers on %s: %w", api.String(), err)
		}
		if !expected && errors.Is(err, platform.ErrServiceNotRunning) {
			return nil, "", nil // nothing listens: the usual case without a service
		}
		logger.Warn("the Lowpath service isn't answering; running the engine in this window", "api", api.String(), "err", err)
		if errors.Is(err, platform.ErrNotService) {
			return nil, "the program answering on its pipe is not the installed service, so Lowpath won't talk to it", nil
		}
		if errors.Is(err, platform.ErrPeerRefused) {
			return nil, "it only serves accounts signed in at this computer, and this one isn't", nil
		}
		return nil, fmt.Sprintf("it isn't answering (%v)", err), nil
	}
	if ver != app.Version {
		logger.Warn("the Lowpath service is another version; reinstall it from this build (lowpathsvc install)", "service", ver, "app", app.Version)
	}
	logger.Info("using the Lowpath service", "api", api.String(), "version", ver)
	return fe, "", nil
}

// waitForService checks the service's version until it answers or wait
// has passed (wait 0: one check).
func waitForService(ctx context.Context, fe *app.Frontend, wait time.Duration) (string, error) {
	deadline := time.Now().Add(wait)
	for {
		c, cancel := context.WithTimeout(ctx, checkTimeout)
		v, err := fe.Check(c)
		cancel()
		// A refusal is an answer: asking again won't change it.
		if err == nil || ctx.Err() != nil || errors.Is(err, platform.ErrPeerRefused) || !time.Now().Before(deadline) {
			return v, err
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}
