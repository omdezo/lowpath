package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"lowpath/internal/app"
	"lowpath/internal/platform"
	"lowpath/internal/platform/fakeplatform"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// answer serves the version check on the fake service API, as a running
// service does, until the test ends.
func answer(t *testing.T, fp *fakeplatform.Fake) {
	t.Helper()
	ln, err := fp.ServiceAPI.Listen()
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"version":%q}`, app.Version)
	})}
	go hs.Serve(ln)
	t.Cleanup(func() { hs.Close() })
}

func TestConnectService(t *testing.T) {
	type setup func(t *testing.T, fp *fakeplatform.Fake)
	for _, tc := range []struct {
		name   string
		o      options
		setup  setup
		use    bool // the window goes through the service
		starts int
		// note is part of the reason given for not using an installed
		// service; "" wants none.
		note string
	}{
		{"not installed", options{}, func(t *testing.T, fp *fakeplatform.Fake) {}, false, 0, ""},
		{"running", options{}, func(t *testing.T, fp *fakeplatform.Fake) {
			fp.Service.Set(platform.ServiceState{Installed: true, Running: true, PID: 7}, nil)
			answer(t, fp)
		}, true, 0, ""},
		{"stopped: started, no prompt", options{}, func(t *testing.T, fp *fakeplatform.Fake) {
			fp.Service.Set(platform.ServiceState{Installed: true}, nil)
			fp.Service.OnStart(func() { answer(t, fp) })
		}, true, 1, ""},
		{"won't start", options{}, func(t *testing.T, fp *fakeplatform.Fake) {
			fp.Service.Set(platform.ServiceState{Installed: true}, nil)
			fp.Service.SetStartErr(errors.New("access denied"))
		}, false, 0, "couldn't start it (access denied)"},
		{"running but not answering", options{}, func(t *testing.T, fp *fakeplatform.Fake) {
			fp.Service.Set(platform.ServiceState{Installed: true, Running: true, PID: 7}, nil)
		}, false, 0, "isn't answering"},
		{"impostor on the pipe", options{}, func(t *testing.T, fp *fakeplatform.Fake) {
			fp.Service.Set(platform.ServiceState{Installed: true, Running: true, PID: 7}, nil)
			answer(t, fp)
			fp.ServiceAPI.SetDialErr(platform.ErrNotService)
		}, false, 0, "not the installed service"},
		{"no service manager, started by hand", options{}, func(t *testing.T, fp *fakeplatform.Fake) {
			fp.Service.Set(platform.ServiceState{}, errors.ErrUnsupported)
			answer(t, fp)
		}, true, 0, ""},
		{"no service manager, nothing listens", options{}, func(t *testing.T, fp *fakeplatform.Fake) {
			fp.Service.Set(platform.ServiceState{}, errors.ErrUnsupported)
		}, false, 0, ""},
		{"demo never uses it", options{demo: true}, func(t *testing.T, fp *fakeplatform.Fake) {
			fp.Service.Set(platform.ServiceState{Installed: true, Running: true}, nil)
			answer(t, fp)
		}, false, 0, ""},
		{"-no-service", options{noService: true}, func(t *testing.T, fp *fakeplatform.Fake) {
			fp.Service.Set(platform.ServiceState{Installed: true, Running: true}, nil)
			answer(t, fp)
		}, false, 0, "-no-service"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fp := fakeplatform.New(t.TempDir())
			tc.setup(t, fp)
			fe, note, err := connectService(t.Context(), tc.o, fp.Platform(), quiet)
			if err != nil {
				t.Fatal(err)
			}
			if (tc.note == "") != (note == "") || !strings.Contains(note, tc.note) {
				t.Errorf("note %q, want one with %q", note, tc.note)
			}
			if (fe != nil) != tc.use {
				t.Errorf("frontend %v, want use=%v", fe != nil, tc.use)
			}
			if fe != nil {
				fe.Close()
			}
			if got := fp.Service.Starts(); got != tc.starts {
				t.Errorf("service started %d times, want %d", got, tc.starts)
			}
			if (tc.o.demo || tc.o.noService) && fp.ServiceAPI.Dials() != 0 {
				t.Error("dialed the service with -demo or -no-service")
			}
		})
	}
}

// A service asked for by name (-service-pipe) that doesn't answer is an
// error, not a silent fallback to an engine in the window.
func TestConnectServiceNamedPipeMissing(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the named service")
	}
	var b [6]byte
	rand.Read(b[:])
	name := `\\.\pipe\lowpath-test-` + hex.EncodeToString(b[:])
	if runtime.GOOS != "windows" {
		name = filepath.Join(t.TempDir(), "api.sock")
	}
	fp := fakeplatform.New(t.TempDir())
	fe, _, err := connectService(t.Context(), options{servicePipe: name}, fp.Platform(), quiet)
	if err == nil || fe != nil {
		t.Fatalf("connectService = %v, %v; want an error", fe, err)
	}
	if fp.Service.Starts() != 0 {
		t.Error("started the installed service for a named one")
	}
}

func TestServiceAPIChoice(t *testing.T) {
	plat := fakeplatform.New(t.TempDir()).Platform()
	if serviceAPI(options{}, plat) != plat.ServiceAPI {
		t.Error("default is not the platform's service API")
	}
	if serviceAPI(options{servicePipe: plat.ServiceAPI.String()}, plat) != plat.ServiceAPI {
		t.Error("naming the installed service's pipe dropped its server check")
	}
	if got := serviceAPI(options{servicePipe: "other"}, plat); got == plat.ServiceAPI || got.String() != "other" {
		t.Errorf("named pipe: %v", got)
	}
}
