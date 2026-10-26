package capture

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Marker records a running capture session on disk, so that a run that
// ended without Close (taskkill /F, a crash, a power cut) can be cleaned up
// at the next start: Backend.Recover gets it back.
type Marker struct {
	Backend string    `json:"backend"` // Backend.Name of the session
	PID     uint32    `json:"pid"`     // the process that captured
	Exe     string    `json:"exe"`     // its image name, to tell a reused PID apart
	Game    string    `json:"game"`    // game id, for the notice
	Started time.Time `json:"started"`
}

// maxMarkerSize caps what Load reads: the file is a few hundred bytes, and
// anything bigger isn't ours.
const maxMarkerSize = 64 << 10

// MarkerFile keeps one Marker in a file. Save replaces the file atomically
// (write a temporary file, then rename), so a power cut leaves either the
// old marker or the new one, never half of one.
type MarkerFile struct{ path string }

// NewMarkerFile keeps the marker at path; its folder is created on Save.
func NewMarkerFile(path string) *MarkerFile { return &MarkerFile{path: path} }

// Path is where the marker lives.
func (f *MarkerFile) Path() string { return f.path }

// Save writes m, replacing any marker already there.
func (f *MarkerFile) Save(m Marker) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
		return fmt.Errorf("capture: session marker: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(f.path), ".capture-session-*")
	if err != nil {
		return fmt.Errorf("capture: session marker: %w", err)
	}
	_, werr := tmp.Write(b)
	if serr := tmp.Sync(); werr == nil {
		werr = serr
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), f.path)
	}
	if werr != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("capture: session marker: %w", werr)
	}
	return nil
}

// Load reads the marker. ok is false when there is none. A file that isn't
// a marker (truncated, edited, not ours) is reported as an error; Clear
// still removes it.
func (f *MarkerFile) Load() (m Marker, ok bool, err error) {
	fh, err := os.Open(f.path)
	if errors.Is(err, fs.ErrNotExist) {
		return Marker{}, false, nil
	}
	if err != nil {
		return Marker{}, false, fmt.Errorf("capture: session marker: %w", err)
	}
	defer fh.Close()
	b, err := io.ReadAll(io.LimitReader(fh, maxMarkerSize+1))
	if err != nil {
		return Marker{}, false, fmt.Errorf("capture: session marker: %w", err)
	}
	if len(b) > maxMarkerSize {
		return Marker{}, false, fmt.Errorf("capture: session marker %s is too big to be one", f.path)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return Marker{}, false, fmt.Errorf("capture: session marker %s is unreadable: %w", f.path, err)
	}
	if m.Backend == "" || m.PID == 0 {
		return Marker{}, false, fmt.Errorf("capture: session marker %s is incomplete", f.path)
	}
	return m, true, nil
}

// Clear removes the marker; no marker is not an error.
func (f *MarkerFile) Clear() error {
	if err := os.Remove(f.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("capture: session marker: %w", err)
	}
	return nil
}
