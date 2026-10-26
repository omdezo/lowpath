package app

import (
	"context"
	"errors"
	"net/http"
	"time"

	"lowpath/internal/diag"
)

// Locator finds the user's public address and ISP (*diag.Locator). Tests
// substitute a fake so nothing goes to the internet.
type Locator interface {
	Locate(ctx context.Context, refresh bool) (diag.Location, error)
	Last() (diag.Location, bool)
}

// handleLocation answers GET /api/location[?refresh=1]: the public IPv4
// and IPv6 address with AS, ISP and city. A lookup takes up to a few
// seconds the first time (STUN, then RIPEstat), so the UI asks for it in
// the background; repeat calls reuse a recent result. Lowpath's own lookups
// are never captured, so this is the user's real connection even while a
// game is being routed.
func (s *Server) handleLocation(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	loc, err := s.locator.Locate(ctx, r.URL.Query().Get("refresh") == "1")
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			writeErr(w, http.StatusGatewayTimeout, "location_timeout", errors.New("looking up your connection took too long; try again"))
			return
		}
		writeErr(w, http.StatusServiceUnavailable, "location_failed", err)
		return
	}
	writeJSON(w, http.StatusOK, loc)
}

// lastLocation is the latest location for the diagnostics report, with the
// host part of each address removed; nil when none was looked up.
func (s *Server) lastLocation() *diag.Location {
	loc, ok := s.locator.Last()
	if !ok {
		return nil
	}
	red := loc.Redacted()
	return &red
}
