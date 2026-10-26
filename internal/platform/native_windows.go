package platform

import (
	"lowpath/internal/capture"
	"lowpath/internal/compat"
	"lowpath/internal/detect"
	"lowpath/internal/diag"
	"lowpath/internal/owner"
)

// Native is the Windows platform: WinDivert capture, the IP Helper socket
// table, UAC, Steam detection, icons read from the program files and flows
// learned from ETW (Microsoft-Windows-Kernel-Network); other network software
// is read from the service manager, the registry and the IP Helper API.
func Native() *Platform {
	owners := owner.New()
	priv := NewUAC()
	paths := NewWindowsPaths()
	games := detect.New()
	return &Platform{
		OS:         "windows",
		Capture:    capture.NewWinDivert(owners, priv, paths),
		Owners:     owners,
		Privileges: priv,
		Games:      games,
		Icons:      games,
		Paths:      paths,
		Flows:      detect.NewKernelNetwork(),
		Inventory:  compat.NewSystem(games, diag.NewNetStats()),
	}
}
