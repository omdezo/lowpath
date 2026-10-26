package diag

import (
	"errors"
)

// Wi-Fi link readings and tuning. A Wi-Fi link loses and delays packets in
// ways a cable doesn't: a weak or crowded radio channel, a slow PHY rate,
// and the adapter's periodic background scans, which leave the channel for
// tens to hundreds of milliseconds to look for other access points. WLAN
// reads the link and can pause those scans while a game is boosted.
//
// The implementation is per OS (the Native Wifi API on Windows,
// /proc/net/wireless on Linux); a part the OS doesn't offer reports an
// error matching errors.ErrUnsupported, and the UI shows "—" with the
// reason instead of a number.

// WLAN errors. Each says why a reading is missing, so the UI can show it
// next to the "—".
var (
	// ErrNoWiFi: the PC has no Wi-Fi adapter, or the system's Wi-Fi
	// service doesn't run.
	ErrNoWiFi = errors.New("diag: no Wi-Fi adapter")
	// ErrWiFiDisconnected: the adapter isn't connected to a network.
	ErrWiFiDisconnected = errors.New("diag: Wi-Fi isn't connected")
	// ErrLocationDenied: the system lets only apps with location access
	// read the access point's details (Windows 11 24H2 and later).
	ErrLocationDenied = errors.New("diag: Wi-Fi details need location access")
)
