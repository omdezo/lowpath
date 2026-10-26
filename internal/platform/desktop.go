package platform

import (
	"context"
	"errors"
	"io"
	"log/slog"
)

// Desktop is the shell around the UI on the user's desktop: the app window,
// the default browser as the fallback, and a dialog for errors at start.
// Native builds use WebView2 on Windows, a WebKit or Chromium app window on
// Linux and a WKWebView window on macOS (NewDesktop).
type Desktop interface {
	// RunWindow shows url in an app window and returns when the user has
	// closed every window it opened, or ctx has ended (it then closes them).
	// It fails at once, with an error matching ErrNoWindow, when this system
	// can't show one, so the caller can open the browser instead.
	RunWindow(ctx context.Context, url string) error
	// ShowWindow brings the window forward, opening another one at url (a
	// fresh launch link) where the window can't be raised from outside. It
	// is for a second start of the app, while RunWindow runs; without an
	// open window it fails with ErrNoWindow.
	ShowWindow(url string) error
	// OpenBrowser opens url in the user's default browser.
	OpenBrowser(url string) error
	// ShowError tells the user why the app could not start: a build started
	// from the desktop has no terminal to print to.
	ShowError(title, msg string)
	// AddToAppMenu makes exe startable from the desktop's app menu with one
	// click, where the OS keeps such entries per user (Linux); elsewhere the
	// installer or the app bundle does it, and this does nothing.
	AddToAppMenu(exe string) error
}

// ErrNoWindow is what Desktop.RunWindow and ShowWindow return when there is
// no app window to show.
var ErrNoWindow = errors.New("no app window")

// DesktopConfig is what NewDesktop needs.
type DesktopConfig struct {
	// ProfileDir holds the window's browser profile (it keeps no state the
	// UI needs, so it may be thrown away).
	ProfileDir string
	// Icon is the app icon as a 256×256 PNG, for windows and the app menu
	// on systems that take it from a file.
	Icon []byte
	Log  *slog.Logger
}

// Instance keeps Lowpath to one running copy per user and UI address: a
// later start hands over to the copy that runs (which shows its window)
// instead of starting a second one.
type Instance interface {
	// Serve takes the calls of later starts until the returned Closer is
	// closed; onShow runs for each, on its own goroutine.
	Serve(onShow func()) (io.Closer, error)
	// Signal asks the running copy to show its window, and reports whether
	// it answered.
	Signal() bool
}
