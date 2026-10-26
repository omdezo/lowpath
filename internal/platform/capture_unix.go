//go:build !windows && !linux

package platform

import (
	"errors"
	"fmt"
	"runtime"

	"lowpath/internal/capture"
	"lowpath/internal/owner"
)

// nativeCapture reports that capture isn't available on this OS yet (the
// macOS utun backend is issue #42).
func nativeCapture(owner.Resolver) CaptureBackend {
	return capture.NewUnavailable(fmt.Errorf("capture: packet capture is not available on %s yet: %w", runtime.GOOS, errors.ErrUnsupported))
}
