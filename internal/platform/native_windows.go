package platform

import (
	"lowpath/internal/capture"
	"lowpath/internal/detect"
	"lowpath/internal/owner"
)

// Native is the Windows platform: WinDivert capture, the IP Helper socket
// table, UAC, Steam detection and icons read from the program files.
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
	}
}
