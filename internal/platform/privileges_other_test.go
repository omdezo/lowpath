//go:build !windows

package platform

import "testing"

func TestRootElevation(t *testing.T) {
	if (&Root{euid: func() int { return 0 }}).Elevated() != true {
		t.Error("euid 0 not elevated")
	}
	if (&Root{euid: func() int { return 1000 }}).Elevated() {
		t.Error("euid 1000 elevated")
	}
}
