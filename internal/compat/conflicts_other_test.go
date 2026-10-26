//go:build !windows

package compat

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"lowpath/internal/diag"
)

type noProcs struct{}

func (noProcs) Running() map[string]bool { return map[string]bool{"lowpath.test": true} }

func TestUnixSystem(t *testing.T) {
	sys := NewSystem(noProcs{}, diag.NewNetStats())
	if _, err := sys.Services(); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("Services: %v, want errors.ErrUnsupported", err)
	}
	if _, err := sys.Programs(); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("Programs: %v, want errors.ErrUnsupported", err)
	}
	ads, err := sys.Adapters()
	if err != nil {
		t.Fatalf("Adapters: %v", err)
	}
	if !slices.ContainsFunc(ads, func(a Adapter) bool { return strings.HasPrefix(a.Name, "lo") && a.Up }) {
		t.Errorf("no loopback adapter among %d", len(ads))
	}
}
