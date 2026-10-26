package platform

import (
	_ "embed"
	"errors"
	"log/slog"
	"path"
	"path/filepath"
	"strings"
)

// wkWebViewScript shows the UI in a WKWebView window through osascript's
// JavaScript bridge to Cocoa, which every macOS has: a native web view
// without cgo. The URL comes in the environment, which other users can't
// see (unlike the command line).
//
//go:embed wkwebview.js
var wkWebViewScript string

// macChromiumApps are the Chromium-family browsers tried for an app-mode
// window when the WKWebView one can't open.
func macChromiumApps() []string {
	return []string{"Google Chrome", "Microsoft Edge", "Brave Browser", "Chromium", "Vivaldi"}
}

func newMacDesktop(cfg DesktopConfig, x execer, home func() (string, error)) *AppWindows {
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	w := &AppWindows{
		x: x, grace: startGrace, stop: stopWait, log: log,
		browser: func(url string) command { return command{path: "/usr/bin/open", args: []string{url}} },
		dialog: func(title, msg string) []command {
			return []command{{path: "/usr/bin/osascript", args: []string{
				"-e", "on run argv",
				"-e", "display alert (item 1 of argv) message (item 2 of argv) as critical",
				"-e", "end run", title, msg,
			}}}
		},
	}
	w.kinds = []windowKind{wkWebViewKind(cfg, x), macChromiumKind(cfg, x, home)}
	return w
}

func wkWebViewKind(cfg DesktopConfig, x execer) windowKind {
	return windowKind{name: "wkwebview", command: func(url string) (command, error) {
		osa, ok := firstFound(x, "/usr/bin/osascript")
		if !ok {
			return command{}, errors.New("no osascript")
		}
		// App Transport Security lets a web view load plain HTTP from
		// localhost; an IP address isn't exempt. The API answers both.
		url = strings.Replace(url, "://127.0.0.1:", "://localhost:", 1)
		env := []string{"LOWPATH_URL=" + url}
		if icon := writeIcon(cfg.ProfileDir, cfg.Icon); icon != "" {
			env = append(env, "LOWPATH_ICON="+icon)
		}
		return command{path: osa, args: []string{"-l", "JavaScript", "-e", wkWebViewScript}, env: env}, nil
	}}
}

func macChromiumKind(cfg DesktopConfig, x execer, home func() (string, error)) windowKind {
	return chromiumKind(func() (string, string, error) {
		var dirs []string
		if h, err := home(); err == nil {
			dirs = append(dirs, path.Join(h, "Applications"))
		}
		dirs = append(dirs, "/Applications")
		var paths []string
		for _, name := range macChromiumApps() {
			for _, d := range dirs {
				paths = append(paths, path.Join(d, name+".app", "Contents", "MacOS", name)) // macOS paths, on any OS the tests run on
			}
		}
		found, ok := firstFound(x, paths...)
		if !ok {
			return "", "", errors.New("no Chromium-family browser")
		}
		return found, filepath.Join(cfg.ProfileDir, "chromium"), nil
	})
}
