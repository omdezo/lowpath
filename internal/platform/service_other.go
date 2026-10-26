//go:build !windows

package platform

import (
	"context"
	"errors"
	"fmt"
	"runtime"
)

// NoServiceManager is the ServiceManager on Linux and macOS until the
// systemd unit and launchd daemon exist: nothing is installed, and
// installing says it isn't available. `lowpathsvc run` still serves the API
// on DefaultSocketPath when started by hand or by an init system.
type NoServiceManager struct{}

func NewServiceManager() NoServiceManager { return NoServiceManager{} }

func (NoServiceManager) unsupported(what string) error {
	return fmt.Errorf("platform: %s the background service is not available on %s yet: %w", what, runtime.GOOS, errors.ErrUnsupported)
}

func (m NoServiceManager) State() (ServiceState, error) {
	return ServiceState{}, m.unsupported("managing")
}
func (m NoServiceManager) Start() error                { return m.unsupported("starting") }
func (m NoServiceManager) Install(ServiceConfig) error { return m.unsupported("installing") }
func (m NoServiceManager) Uninstall() error            { return m.unsupported("removing") }

// ForegroundRunner is the ServiceRunner where init systems run services as
// plain processes (systemd and launchd stop them with SIGTERM): it never
// takes over.
type ForegroundRunner struct{}

func NewServiceRunner() ForegroundRunner { return ForegroundRunner{} }

func (ForegroundRunner) Run(string, func(context.Context) error) (bool, error) { return false, nil }

// ServiceAPIAt is the API socket at path, for a service run by hand on a
// socket of its own (development and tests).
func ServiceAPIAt(path string) ServiceAPI { return NewSocketAPI(path, 0o600) }
