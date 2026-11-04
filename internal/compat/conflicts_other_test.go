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

func (noProcs) Running() (map[string]bool, error) {
	return map[string]bool{"lowpath.test": true}, nil
}

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

type failingProcs struct{ err error }

func (f failingProcs) Running() (map[string]bool, error) { return nil, f.err }

// A process list that couldn't be read is an error, so the report lists
// running programs as unchecked instead of saying no booster runs.
func TestUnixSystemUnreadableProcesses(t *testing.T) {
	unreadable := errors.New("no /proc")
	sys := NewSystem(failingProcs{unreadable}, diag.NewNetStats())
	if p, err := sys.Processes(); p != nil || !errors.Is(err, unreadable) {
		t.Errorf("Processes = %v, %v; want %v", p, err, unreadable)
	}
}
