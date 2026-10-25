//go:build !windows

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"syscall"
	"time"
)

// Route mode (and so elevation) exists only on Windows; elsewhere the app
// runs in proxy mode and is viewed in a browser.
const elevationSupported = false

func relaunchElevated([]string) error { return errors.New("not supported on this platform") }

func waitForExit(pid int, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		p, err := os.FindProcess(pid)
		if err != nil || p.Signal(syscall.Signal(0)) != nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func showError(title, msg string) { fmt.Fprintf(os.Stderr, "%s: %s\n", title, msg) }

func runWindow(ctx context.Context, url string, log *slog.Logger) error {
	return errors.New("the app window is only available on Windows")
}

func focusExisting() bool { return false }
