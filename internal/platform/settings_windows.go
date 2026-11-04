package platform

// settingsURLs are ms-settings: URIs, which the shell opens in the Settings
// app (Desktop.OpenBrowser hands them to url.dll's FileProtocolHandler).
// Microsoft names privacy-location as the page to send users to when the
// Wi-Fi scan is refused for location: "Changes to API behavior for Wi-Fi
// access and location".
var settingsURLs = map[SettingsPage]string{
	SettingsLocation: "ms-settings:privacy-location",
}
