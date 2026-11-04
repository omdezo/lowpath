package app

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"lowpath/internal/platform"
)

// SettingsOpener opens a page of the OS's own settings on the user's
// desktop (platform.OpenSettings through the window's Desktop). Only the
// process that shows the window has one: the background service runs as
// another account, where a settings page would open on nobody's screen.
type SettingsOpener func(page platform.SettingsPage) error

// handleOpenSettings is POST /api/os/settings/{page}: it opens that page
// for a button the user pressed. The page is one of the names the app
// defines (platform.SettingsPage.Known), never a URL from the request;
// whether this system has it is open's answer (errors.ErrUnsupported). Served by the
// process with the window: the app when the engine runs in it, the
// Frontend in front of the service, and never the service itself (open is
// nil there).
func handleOpenSettings(open SettingsOpener, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		page := platform.SettingsPage(r.PathValue("page"))
		if !page.Known() {
			writeErr(w, http.StatusNotFound, "unknown_page", fmt.Errorf("no settings page %q", page))
			return
		}
		if open == nil {
			writeErr(w, http.StatusNotImplemented, "not_supported", errors.New("this part of Lowpath can't open the system's settings"))
			return
		}
		err := open(page)
		switch {
		case err == nil:
			w.WriteHeader(http.StatusNoContent)
		case errors.Is(err, errors.ErrUnsupported):
			writeErr(w, http.StatusNotImplemented, "not_supported", errors.New("this system has no such settings page"))
		default:
			log.Warn("open settings", "page", page, "err", err)
			writeErr(w, http.StatusInternalServerError, "failed", fmt.Errorf("the system's settings couldn't be opened: %w", err))
		}
	}
}
