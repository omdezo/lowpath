package app

import (
	"errors"
	"hash/fnv"
	"net/http"
	"strings"

	"lowpath/internal/detect"
)

// customColors are the library tile colors handed out to custom games, picked
// by name so a game keeps its color.
var customColors = []string{"#19E3B4", "#5B8DEF", "#E8A33A", "#8A5CF6", "#E2525C", "#3FB8D9", "#C8A456", "#7BC86C"}

func (s *Server) handleAddCustomGame(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name    string `json:"name"`
		Process string `json:"process"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, "bad_request", err)
		return
	}
	g, err := newCustomGame(req.Name, req.Process)
	if err != nil {
		writeErr(w, 400, "bad_game", err)
		return
	}
	// Remember where the program lives, so its icon can be shown after it exits.
	g.Exe = detect.RunningExe(g.Processes)
	if err := s.store.AddCustomGame(g); err != nil {
		if errors.Is(err, ErrGameExists) {
			writeErr(w, 409, "game_exists", err)
		} else {
			writeErr(w, 500, "failed", err)
		}
		return
	}
	s.log.Info("custom game added", "id", g.ID, "process", g.Processes[0])
	s.scanRunning() // so the reply already says whether it is running
	writeJSON(w, 200, s.gameViews(g)[0])
}

// newCustomGame validates the request. The process may be given as a full
// path (pasted from Explorer); only the image name is matched.
func newCustomGame(name, process string) (Game, error) {
	name = strings.TrimSpace(name)
	process = strings.TrimSpace(process)
	if i := strings.LastIndexAny(process, `\/`); i >= 0 {
		process = process[i+1:]
	}
	switch {
	case name == "":
		return Game{}, errors.New("enter a name for the game")
	case len(name) > 64:
		return Game{}, errors.New("the name is too long (64 characters at most)")
	case process == "":
		return Game{}, errors.New("pick the game's process")
	case len(process) > 260 || strings.ContainsAny(process, `:*?"<>|`):
		return Game{}, errors.New("that is not a process name")
	}
	id := slug(name, 40)
	if id == "" {
		id = slug(strings.TrimSuffix(strings.ToLower(process), ".exe"), 40)
	}
	if id == "" {
		return Game{}, errors.New("the name needs at least one letter or digit")
	}
	h := fnv.New32a()
	h.Write([]byte(id))
	return Game{
		ID:        "custom-" + id,
		Name:      name,
		Genre:     "Custom",
		Color:     customColors[h.Sum32()%uint32(len(customColors))],
		Smart:     true,
		Processes: []string{process},
		Custom:    true,
	}, nil
}

func (s *Server) handleDeleteCustomGame(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	active := s.sess != nil && s.sess.GameID == id
	s.mu.Unlock()
	if active {
		s.stopSession()
	}
	if err := s.store.DeleteCustomGame(id); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeErr(w, 404, "not_found", errors.New("unknown custom game"))
		} else {
			writeErr(w, 500, "failed", err)
		}
		return
	}
	s.mu.Lock()
	delete(s.analysis, id)
	delete(s.running, id)
	s.mu.Unlock()
	s.log.Info("custom game removed", "id", id)
	writeJSON(w, 200, map[string]bool{"ok": true})
}
