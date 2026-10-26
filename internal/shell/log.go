package shell

import (
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"

	"lowpath/internal/app"
)

// MaxLogSize is when the log starts over (checked at start only).
const MaxLogSize = 5 << 20

// OpenLog logs to <dir>/lowpath.log and stderr, and points the standard
// library's log (used by the WebView2 binding) at the same place.
func OpenLog(dir string) (*slog.Logger, func(), error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, err
	}
	path := filepath.Join(dir, "lowpath.log")
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if fi, err := os.Stat(path); err == nil && fi.Size() > MaxLogSize {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return nil, nil, err
	}
	w := tee{f, os.Stderr}
	logger := slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	log.SetOutput(w)
	return logger, func() { f.Close() }, nil
}

// tee writes to every writer and never fails: a GUI build or a service has
// no stderr, and logging must not break the app.
type tee []io.Writer

func (t tee) Write(p []byte) (int, error) {
	for _, w := range t {
		w.Write(p)
	}
	return len(p), nil
}

// MaxCrashLog is when crash.log starts over (checked at start only).
const MaxCrashLog = 1 << 20

// KeepCrashes sends the Go runtime's report of a fatal crash (an unrecovered
// panic, a fatal error) to <dir>/crash.log as well as stderr: the window
// build and the service have no console, so without it a crash leaves no
// trace. Each start adds a header line, so a report can be matched to the
// run (and the PID in a session marker) that wrote it. The returned func
// stops the redirect.
func KeepCrashes(dir string, log *slog.Logger) func() {
	path := filepath.Join(dir, "crash.log")
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if fi, err := os.Stat(path); err == nil && fi.Size() > MaxCrashLog {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		log.Warn("crash log", "err", err)
		return func() {}
	}
	exe, _ := os.Executable()
	fmt.Fprintf(f, "--- Lowpath %s (%s), pid %d, started %s\n", app.Version, filepath.Base(exe), os.Getpid(), time.Now().UTC().Format(time.RFC3339))
	if err := debug.SetCrashOutput(f, debug.CrashOptions{}); err != nil {
		log.Warn("crash log", "err", err)
		f.Close()
		return func() {}
	}
	// SetCrashOutput keeps its own duplicate of the file.
	f.Close()
	return func() { debug.SetCrashOutput(nil, debug.CrashOptions{}) }
}
