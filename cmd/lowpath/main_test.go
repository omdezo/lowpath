package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"lowpath/internal/app"
)

func TestChildArgsReplaceWaitPID(t *testing.T) {
	saved := os.Args
	defer func() { os.Args = saved }()
	os.Args = []string{"Lowpath.exe", "-headless", "-wait-pid", "5", "--wait-pid=6", "-addr", "127.0.0.1:7879"}

	got := childArgs(options{data: `C:\Users\p\AppData\Roaming\Lowpath`})
	want := []string{"-headless", "-addr", "127.0.0.1:7879", "-data", `C:\Users\p\AppData\Roaming\Lowpath`, "-wait-pid", strconv.Itoa(os.Getpid())}
	if !slices.Equal(got, want) {
		t.Errorf("childArgs = %q, want %q", got, want)
	}

	// An explicit -data is passed through as given, not added twice.
	os.Args = []string{"Lowpath.exe", "-data", `D:\x`}
	got = childArgs(options{data: `D:\x`, dataFromArg: true})
	if strings.Count(strings.Join(got, " "), "-data") != 1 {
		t.Errorf("childArgs = %q, want -data once", got)
	}
}

func TestOpenLogStartsOverWhenLarge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lowpath.log")
	if err := os.WriteFile(path, make([]byte, maxLogSize+1), 0o600); err != nil {
		t.Fatal(err)
	}
	logger, closeLog, err := openLog(dir)
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
	logger, closeLog, _ = openLog(dir)
	logger.Info("again")
	closeLog()
	b, _ = os.ReadFile(path)
	if !strings.Contains(string(b), "hello") || !strings.Contains(string(b), "again") {
		t.Errorf("small log not appended to: %q", b)
	}
}

func TestCheckLoopback(t *testing.T) {
	for addr, ok := range map[string]bool{"127.0.0.1:7878": true, "localhost:1": true, "[::1]:7878": true, "0.0.0.0:7878": false, "192.168.1.2:80": false} {
		if err := checkLoopback(addr); (err == nil) != ok {
			t.Errorf("checkLoopback(%s) = %v", addr, err)
		}
	}
}

// A second launch finds the running copy through the one API call that needs
// no session, and nothing else passes for it.
func TestExistingInstance(t *testing.T) {
	store, err := app.OpenStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv, err := app.NewServer(app.Config{Store: store, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	defer srv.Close()
	if _, ok := existingInstance(ts.Listener.Addr().String()); !ok {
		t.Error("running Lowpath not recognized")
	}

	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"name":"something else"}`))
	}))
	defer other.Close()
	if _, ok := existingInstance(other.Listener.Addr().String()); ok {
		t.Error("another program on the port taken for Lowpath")
	}
}
