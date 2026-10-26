package app

import (
	"net/http"

	"lowpath/internal/diag"
)

// The router pinger behind Settings → Diagnostics: diag.GatewayPinger runs
// for the app's whole life but only pings while the Diagnostics tab polls
// GET /api/diag/gateway (and for 15 s after) or a boost runs, and never
// while the bufferbloat test loads the line. Its latest series is the
// verdict's gateway evidence.

const gatewayPausedForBloat = "The bufferbloat test is running: it pings your router itself while it fills the line, so the live pings wait until it ends."

func newGatewayPinger(cfg Config, s *Server) *diag.GatewayPinger {
	return diag.NewGatewayPinger(diag.GatewayConfig{
		Net: cfg.NetStats, Ping: cfg.Ping, PingTTL: cfg.PingTTL,
		Interval: cfg.GatewayInterval,
		Hold:     s.boosting,
		Pause: func() string {
			if s.bloat.isRunning() {
				return gatewayPausedForBloat
			}
			return ""
		},
	})
}

// boosting reports whether a session runs: the pinger keeps going during a
// boost, so the verdict knows whether Wi-Fi lags the game right now.
func (s *Server) boosting() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sess != nil
}

// handleGateway is GET /api/diag/gateway. Asking keeps the pinger going:
// the Diagnostics tab polls it while it is on screen.
func (s *Server) handleGateway(w http.ResponseWriter, r *http.Request) {
	s.gateway.Touch()
	writeJSON(w, http.StatusOK, s.gateway.View())
}
