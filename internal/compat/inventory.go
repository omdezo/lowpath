package compat

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"lowpath/internal/diag"
)

// Service is one installed Windows service or driver.
type Service struct {
	Name    string `json:"name"`    // key name, e.g. "ndisrd"
	Display string `json:"display"` // display name, may be ""
	// Path is the program or driver file (ImagePath); "" when it couldn't
	// be read.
	Path    string `json:"path"`
	Driver  bool   `json:"driver"`
	Running bool   `json:"running"`
}

// Adapter is one network interface.
type Adapter struct {
	Index       int    `json:"index"`
	Name        string `json:"name"`        // "Ethernet", "wg0", ...
	Description string `json:"description"` // driver description on Windows ("WireGuard Tunnel"), "" elsewhere
	// Type is the IANA ifType (6 Ethernet, 23 PPP, 53 virtual, 131
	// tunnel); 0 when the OS doesn't say.
	Type uint32 `json:"type"`
	Up   bool   `json:"up"`
}

// Inventory is a snapshot of what else on the PC handles network traffic.
// It is also the format of the file NewFileSystem reads.
type Inventory struct {
	Processes []string  `json:"processes"` // image names, e.g. "ExitLag.exe"
	Services  []Service `json:"services"`
	Programs  []string  `json:"programs"` // installed programs' display names
	Adapters  []Adapter `json:"adapters"`
	// DefaultRoute is the index of the interface that carries internet
	// traffic; 0 when offline or unknown.
	DefaultRoute int `json:"default_route"`
}

// System reads the parts of an Inventory from the OS. Each part may fail on
// its own (a refused registry key doesn't hide the adapters); a part an OS
// doesn't have returns an error matching errors.ErrUnsupported and is left
// out without a note.
type System interface {
	Processes() ([]string, error)
	Services() ([]Service, error)
	Programs() ([]string, error)
	Adapters() ([]Adapter, error)
	// DefaultRoute returns the interface index carrying internet traffic,
	// or an error matching diag.ErrNoRoute when offline.
	DefaultRoute() (int, error)
}

// ProcessLister is the running process list (platform.GameDetector).
type ProcessLister interface {
	// Running returns the lower-cased image names of running processes.
	Running() map[string]bool
}

// RouteFinder finds the interface that carries internet traffic
// (diag.NetStats).
type RouteFinder interface {
	DefaultRoute() (diag.Route, error)
}

// processNames is the running process list as a slice.
func processNames(p ProcessLister) []string {
	run := p.Running()
	out := make([]string, 0, len(run))
	for n := range run {
		out = append(out, n)
	}
	return out
}

// routeIndex asks r which interface carries internet traffic.
func routeIndex(r RouteFinder) (int, error) {
	rt, err := r.DefaultRoute()
	if err != nil {
		return 0, err
	}
	return rt.Index, nil
}

// FileSystem is a System read from a JSON Inventory file: the end-to-end
// test (lowpath -compat-inventory) plays "ExitLag is running" with it on
// any PC, without touching the real system. The file is read again on every
// call, so a test can change it while the app runs; a missing file is an
// empty, offline system.
type FileSystem struct{ path string }

// NewFileSystem reads the inventory at path.
func NewFileSystem(path string) *FileSystem { return &FileSystem{path: path} }

func (f *FileSystem) read() (Inventory, error) {
	var inv Inventory
	b, err := os.ReadFile(f.path)
	if errors.Is(err, fs.ErrNotExist) {
		return inv, nil
	}
	if err != nil {
		return inv, err
	}
	if err := json.Unmarshal(b, &inv); err != nil {
		return inv, fmt.Errorf("compat: %s: %w", f.path, err)
	}
	return inv, nil
}

func (f *FileSystem) Processes() ([]string, error) {
	inv, err := f.read()
	return inv.Processes, err
}

func (f *FileSystem) Services() ([]Service, error) {
	inv, err := f.read()
	return inv.Services, err
}

func (f *FileSystem) Programs() ([]string, error) {
	inv, err := f.read()
	return inv.Programs, err
}

func (f *FileSystem) Adapters() ([]Adapter, error) {
	inv, err := f.read()
	return inv.Adapters, err
}

func (f *FileSystem) DefaultRoute() (int, error) {
	inv, err := f.read()
	if err == nil && inv.DefaultRoute == 0 {
		err = diag.ErrNoRoute
	}
	return inv.DefaultRoute, err
}
