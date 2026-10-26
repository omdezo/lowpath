package platform

import (
	"lowpath/internal/capture"
	"lowpath/internal/owner"
)

// nativeCapture is route mode's capture on Linux: a TUN device, an nftables
// mark and a policy-routing rule (capture.TUN).
func nativeCapture(owners owner.Resolver) CaptureBackend { return capture.NewTUN(owners) }
