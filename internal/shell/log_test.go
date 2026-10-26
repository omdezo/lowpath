package shell

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestOpenLogStartsOverWhenLarge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lowpath.log")
	if err := os.WriteFile(path, make([]byte, MaxLogSize+1), 0o600); err != nil {
		t.Fatal(err)
	}
	logger, closeLog, err := OpenLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("hello")
	closeLog()
	b, _ := os.ReadFile(path)
	if len(b) > 1000 || !strings.Contains(string(b), "hello") {
		t.Errorf("log is %d bytes after start; want it truncated and holding the new line", len(b))
	}

	// A small log is appended to.
	logger, closeLog, _ = OpenLog(dir)
	logger.Info("again")
	closeLog()
	b, _ = os.ReadFile(path)
	if !strings.Contains(string(b), "hello") || !strings.Contains(string(b), "again") {
		t.Errorf("small log not appended to: %q", b)
	}
}

func TestKeepCrashes(t *testing.T) {
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(dir, "crash.log")
	// An oversized old log starts over.
	if err := os.WriteFile(path, []byte(strings.Repeat("x", MaxCrashLog+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	stop := KeepCrashes(dir, log)
	stop()
	stop = KeepCrashes(dir, log)
	stop()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 || strings.Contains(string(b), "xxx") {
		t.Fatalf("crash log after two starts:\n%.200s", b)
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, "--- Lowpath ") || !strings.Contains(l, "pid "+strconv.Itoa(os.Getpid())) {
			t.Errorf("header %q", l)
		}
	}
	// A folder that can't take the file only costs the crash log.
	KeepCrashes(filepath.Join(dir, "missing", "deeper"), log)()
}
