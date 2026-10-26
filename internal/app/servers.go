package app

import (
	"errors"
	"net/http"
	"slices"

	"lowpath/internal/catalog"
)

// handleServers answers GET /api/games/{id}/servers (the game's known
// servers with a direct ping each) and POST .../servers/ping (measure them
// again now). Both return at once; the UI polls while "state" is
// "loading" or "pinging" is true.
func (s *Server) handleServers(w http.ResponseWriter, r *http.Request) {
	g, ok := s.game(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "not_found", errors.New("unknown game"))
		return
	}
	writeJSON(w, 200, s.serverList(g, r.Method == http.MethodPost))
}

func (s *Server) serverList(g Game, ping bool) catalog.ServerList {
	if s.servers == nil || g.SteamApp <= 0 || !slices.Contains(g.Discovery, DiscoverySDR) {
		return catalog.Unsupported("Lowpath has no public server list for " + g.Name + " yet.")
	}
	if ping {
		return s.servers.Ping(g.SteamApp)
	}
	return s.servers.List(g.SteamApp)
}
