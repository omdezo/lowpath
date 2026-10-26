package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"

	"lowpath/internal/app"
)

// maxCrashLog is when crash.log starts over (checked at start only).
const maxCrashLog = 1 << 20

// keepCrashes sends the Go runtime's report of a fatal crash (an unrecovered
// panic, a fatal error) to <data>/crash.log as well as stderr: the window
// build has no console, so without it a crash leaves no trace. Each start
// adds a header line, so a report can be matched to the run (and the PID in
// a session marker) that wrote it. The returned func stops the redirect.
func keepCrashes(dir string, log *slog.Logger) func() {
	path := filepath.Join(dir, "crash.log")
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if fi, err := os.Stat(path); err == nil && fi.Size() > maxCrashLog {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		log.Warn("crash log", "err", err)
		return func() {}
	}
	fmt.Fprintf(f, "--- Lowpath %s, pid %d, started %s\n", app.Version, os.Getpid(), time.Now().UTC().Format(time.RFC3339))
	if err := debug.SetCrashOutput(f, debug.CrashOptions{}); err != nil {
		log.Warn("crash log", "err", err)
		f.Close()
		return func() {}
	}
	// SetCrashOutput keeps its own duplicate of the file.
	f.Close()
	return func() { debug.SetCrashOutput(nil, debug.CrashOptions{}) }
}
