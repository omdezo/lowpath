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

// The desktop shell (window, focusing a running copy) exists only on
// Windows so far; elsewhere the UI opens in the browser (issue #44).

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

func runWindow(ctx context.Context, url, dataDir string, log *slog.Logger) error {
	return errors.New("the app window is only available on Windows")
}

func focusExisting() bool { return false }
