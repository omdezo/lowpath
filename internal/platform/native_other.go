//go:build !windows

package platform

import (
	"errors"
	"fmt"
	"runtime"

	"lowpath/internal/capture"
	"lowpath/internal/detect"
	"lowpath/internal/owner"
)

// Native is the Linux and macOS platform. Proxy mode, relays, measurements
// and the UI work as on Windows; route mode reports that capture isn't
// available here until the TUN backends land (issues #41 and #42), and the
// owner lookup and Steam detection report nothing until issue #43.
func Native() *Platform {
	games := detect.New()
	return &Platform{
		OS:         runtime.GOOS,
		Capture:    capture.NewUnavailable(fmt.Errorf("capture: packet capture is not available on %s yet: %w", runtime.GOOS, errors.ErrUnsupported)),
		Owners:     owner.New(),
		Privileges: NewRoot(),
		Games:      games,
		Icons:      games,
		Paths:      NewUnixPaths(),
		Flows:      detect.NewKernelNetwork(),
	}
}
