package main

import (
	"os"
	"testing"
)

// TestMain keeps the app tests off the internet: run starts the whole app,
// whose background work (game artwork, server lists) would otherwise fetch
// from the internet. Loopback requests never go through a proxy, so the
// app's own API still answers; everything else fails at once, as offline.
func TestMain(m *testing.M) {
	if os.Getenv("LOWPATH_INTERNET") != "1" {
		for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
			os.Setenv(k, "http://127.0.0.1:9")
		}
		os.Unsetenv("NO_PROXY")
		os.Unsetenv("no_proxy")
	}
	os.Exit(m.Run())
}
