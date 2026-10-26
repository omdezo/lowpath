package capture

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMarkerFile(t *testing.T) {
	dir := t.TempDir()
	f := NewMarkerFile(filepath.Join(dir, "sub", "capture-session.json"))
	if _, ok, err := f.Load(); ok || err != nil {
		t.Fatalf("no file: ok=%v err=%v", ok, err)
	}
	if err := f.Clear(); err != nil {
		t.Fatalf("Clear without a marker: %v", err)
	}
	m := Marker{Backend: "divert", PID: 4242, Exe: "Lowpath.exe", Game: "cs2", Started: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)}
	if err := f.Save(m); err != nil {
		t.Fatal(err)
	}
	got, ok, err := f.Load()
	if !ok || err != nil || got != m {
		t.Fatalf("Load = %+v %v %v, want %+v", got, ok, err, m)
	}
	m.Game = "dota2"
	if err := f.Save(m); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := f.Load(); got.Game != "dota2" {
		t.Fatalf("Save did not replace: %+v", got)
	}
	// Saving goes through a temporary file; none may be left behind.
	ents, _ := os.ReadDir(filepath.Dir(f.Path()))
	if len(ents) != 1 {
		t.Fatalf("folder holds %d files, want just the marker", len(ents))
	}
	if err := f.Clear(); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := f.Load(); ok {
		t.Fatal("marker still there after Clear")
	}
}

func TestMarkerFileRejectsJunk(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"empty":      "",
		"truncated":  `{"backend":"divert","pid":42`,
		"incomplete": `{"backend":"","pid":42}`,
		"no pid":     `{"backend":"divert"}`,
		"too big":    `{"backend":"divert","pid":42,"game":"` + strings.Repeat("x", maxMarkerSize) + `"}`,
	} {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		f := NewMarkerFile(path)
		if _, ok, err := f.Load(); ok || err == nil {
			t.Errorf("%s: ok=%v err=%v, want an error", name, ok, err)
		}
		if err := f.Clear(); err != nil {
			t.Errorf("%s: Clear: %v", name, err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s: still on disk after Clear", name)
		}
	}
}

func TestRecoverIsSafe(t *testing.T) {
	m := Marker{Backend: "divert", PID: 4242, Exe: "Lowpath.exe"}
	w := NewWinDivert(nil, nil, nil)
	if err := w.Recover(m); err != nil {
		t.Fatalf("WinDivert.Recover: %v", err)
	}
	if err := NewUnavailable(ErrUnsupported).Recover(m); err != nil {
		t.Fatalf("Unavailable.Recover: %v", err)
	}
}
