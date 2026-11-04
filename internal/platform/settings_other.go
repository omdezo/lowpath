//go:build !windows

package platform

// settingsURLs is empty: no page the app sends users to exists here yet
// (only Windows gates the Wi-Fi scan behind the location setting).
var settingsURLs = map[SettingsPage]string{}
