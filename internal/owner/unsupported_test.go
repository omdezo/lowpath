package owner

import (
	"errors"
	"strings"
	"testing"
)

func TestUnsupportedSaysSo(t *testing.T) {
	u := NewUnsupported("plan9")
	if _, err := u.UDPSockets(); !errors.Is(err, errors.ErrUnsupported) || !strings.Contains(err.Error(), "plan9") {
		t.Errorf("UDPSockets: %v", err)
	}
	if _, err := u.Programs(); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("Programs: %v", err)
	}
	if _, err := ListProcesses(u, 1); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("ListProcesses: %v", err)
	}
}
