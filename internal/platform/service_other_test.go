//go:build !windows

package platform

import (
	"context"
	"errors"
	"testing"
)

// Without a service manager integration yet, every call says so, and the
// runner always leaves the process to run in the foreground.
func TestNoServiceManager(t *testing.T) {
	m := NewServiceManager()
	if _, err := m.State(); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("State: %v", err)
	}
	for name, err := range map[string]error{"Start": m.Start(), "Install": m.Install(ServiceConfig{}), "Uninstall": m.Uninstall()} {
		if !errors.Is(err, errors.ErrUnsupported) {
			t.Errorf("%s: %v, want errors.ErrUnsupported", name, err)
		}
	}
	ok, err := NewServiceRunner().Run(ServiceName, func(context.Context) error { return nil })
	if ok || err != nil {
		t.Errorf("Run = %v, %v", ok, err)
	}
}
