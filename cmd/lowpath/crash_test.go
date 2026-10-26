package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestKeepCrashes(t *testing.T) {
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(dir, "crash.log")
	// An oversized old log starts over.
	if err := os.WriteFile(path, []byte(strings.Repeat("x", maxCrashLog+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	stop := keepCrashes(dir, log)
	stop()
	stop = keepCrashes(dir, log)
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
	keepCrashes(filepath.Join(dir, "missing", "deeper"), log)()
}
