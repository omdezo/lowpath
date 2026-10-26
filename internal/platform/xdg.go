package platform

import (
	_ "embed"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// webkitGTKScript shows the UI in a WebKitGTK window through PyGObject,
// which GNOME desktops (Ubuntu, Fedora) ship: a native web view without
// linking GTK into the app. It reads the URL from the environment, which
// other users can't see (unlike the command line), and exits with status 3
// when no WebKitGTK version is installed.
//
//go:embed webkitgtk.py
var webkitGTKScript string

// chromiumNames are the Chromium-family browsers tried for an app-mode
// window, best known first.
func chromiumNames() []string {
	return []string{
		"google-chrome-stable", "google-chrome", "chromium", "chromium-browser",
		"microsoft-edge-stable", "microsoft-edge", "brave-browser", "vivaldi-stable", "vivaldi",
	}
}

func newXDGDesktop(cfg DesktopConfig, x execer, getenv func(string) string, home func() (string, error)) *AppWindows {
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	w := &AppWindows{
		x: x, grace: startGrace, stop: stopWait, log: log,
		browser: func(url string) command { return command{path: "xdg-open", args: []string{url}} },
		dialog: func(title, msg string) []command {
			return []command{
				{path: "zenity", args: []string{"--error", "--no-markup", "--title", title, "--text", msg}},
				{path: "kdialog", args: []string{"--title", title, "--error", msg}},
			}
		},
		menu: func(exe string) error {
			m, err := NewMenuEntry(cfg.Icon, getenv, home)
			if err != nil {
				return err
			}
			return m.Install(exe)
		},
	}
	w.kinds = []windowKind{
		onDisplay(webkitGTKKind(cfg, x, getenv), getenv),
		onDisplay(xdgChromiumKind(cfg, x, home), getenv),
	}
	return w
}

// onDisplay makes k unavailable outside a graphical session (a TTY, ssh,
// a service started before the desktop): a window program there may fail
// slowly, or keep running with nothing on screen, and the app would wait
// for a window nobody can see instead of printing the address.
func onDisplay(k windowKind, getenv func(string) string) windowKind {
	inner := k.command
	k.command = func(url string) (command, error) {
		if getenv("WAYLAND_DISPLAY") == "" && getenv("DISPLAY") == "" {
			return command{}, errors.New("no graphical session (WAYLAND_DISPLAY and DISPLAY are not set)")
		}
		return inner(url)
	}
	return k
}

func webkitGTKKind(cfg DesktopConfig, x execer, getenv func(string) string) windowKind {
	return windowKind{name: "webkitgtk", command: func(url string) (command, error) {
		// The system's Python has the GObject bindings; one from a
		// virtualenv or conda first on PATH usually doesn't.
		py, ok := firstFound(x, "/usr/bin/python3", "python3")
		if !ok {
			return command{}, errors.New("no python3")
		}
		env := []string{"LOWPATH_URL=" + url}
		if icon := writeIcon(cfg.ProfileDir, cfg.Icon); icon != "" {
			env = append(env, "LOWPATH_ICON="+icon)
		}
		// WebKitGTK 2.42+ draws a blank window with NVIDIA's driver unless
		// its DMA-BUF renderer is off; the UI doesn't need it.
		if getenv("WEBKIT_DISABLE_DMABUF_RENDERER") == "" && fileExists("/proc/driver/nvidia/version") {
			env = append(env, "WEBKIT_DISABLE_DMABUF_RENDERER=1")
		}
		// -I: no user site packages, PYTHON* variables or working folder
		// on the module path.
		return command{path: py, args: []string{"-I", "-c", webkitGTKScript}, env: env}, nil
	}}
}

func xdgChromiumKind(cfg DesktopConfig, x execer, home func() (string, error)) windowKind {
	return chromiumKind(func() (string, string, error) {
		path, ok := firstFound(x, chromiumNames()...)
		if !ok {
			return "", "", errors.New("no Chromium-family browser")
		}
		if snap := snapName(path); snap != "" {
			// A snap browser may only write in its own folder under the home
			// folder, not in hidden folders such as ~/.cache.
			h, err := home()
			if err != nil {
				return "", "", err
			}
			return path, filepath.Join(h, "snap", snap, "common", "lowpath-window"), nil
		}
		return path, filepath.Join(cfg.ProfileDir, "chromium"), nil
	})
}

// snapName is the snap a browser program runs from ("" for none):
// /snap/bin/chromium, or Ubuntu's chromium-browser, a script that starts it.
func snapName(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	if rest, ok := strings.CutPrefix(path, "/snap/bin/"); ok {
		return strings.SplitN(rest, ".", 2)[0]
	}
	if strings.HasPrefix(path, "/snap/") {
		return strings.SplitN(strings.TrimPrefix(path, "/snap/"), "/", 2)[0]
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	head := make([]byte, 4096)
	n, _ := f.Read(head)
	if strings.HasPrefix(string(head[:n]), "#!") {
		if i := strings.Index(string(head[:n]), "/snap/bin/"); i >= 0 {
			name := string(head[i+len("/snap/bin/") : n])
			if end := strings.IndexAny(name, " \t\n\"'"); end >= 0 {
				name = name[:end]
			}
			return strings.SplitN(name, ".", 2)[0]
		}
	}
	return ""
}

func fileExists(path string) bool { _, err := os.Stat(path); return err == nil }
