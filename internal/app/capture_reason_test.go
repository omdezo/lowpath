package app

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"lowpath/internal/capture"
)

func TestCaptureReasons(t *testing.T) {
	rights := &capture.RightsError{Reason: "it needs root rights", Fix: "start Lowpath with sudo"}
	for err, want := range map[error]string{
		capture.ErrNeedAdmin:                       "administrator rights are needed",
		fmt.Errorf("open: %w", rights):             "it needs root rights",
		fmt.Errorf("x: %w", capture.ErrNoTUN):      "the kernel has no TUN device (/dev/net/tun)",
		capture.ErrNoDriver:                        "the WinDivert driver can't run on this PC",
		fmt.Errorf("x: %w", errors.ErrUnsupported): "packet capture isn't available on this system yet",
	} {
		if got := captureReason(err); got != want {
			t.Errorf("captureReason(%v) = %q, want %q", err, got, want)
		}
	}
	e := captureErr(fmt.Errorf("start: %w", rights))
	if e.code != 409 || e.kind != "needs_admin" || !strings.Contains(e.err.Error(), "sudo") || strings.Contains(e.err.Error(), "administrator") {
		t.Errorf("captureErr(rights) = %d %s %v", e.code, e.kind, e.err)
	}
	if e := captureErr(capture.ErrNeedAdmin); e.kind != "needs_admin" || !strings.Contains(e.err.Error(), "administrator") {
		t.Errorf("captureErr(ErrNeedAdmin) = %s %v", e.kind, e.err)
	}
}

// The restart button says what it does on each OS: on Linux it asks for a
// password once and keeps running as the same user, so "administrator"
// would be wrong.
func TestElevateWording(t *testing.T) {
	for os, want := range map[string]string{"windows": "Restart as administrator", "linux": "Allow route mode", "darwin": "Restart with administrator rights"} {
		action, hint := elevateWording(os)
		if action != want || hint == "" {
			t.Errorf("%s: %q / %q", os, action, hint)
		}
	}
	if _, hint := elevateWording("linux"); strings.Contains(hint, "administrator") || !strings.Contains(hint, "password") {
		t.Errorf("linux hint %q", hint)
	}
}
