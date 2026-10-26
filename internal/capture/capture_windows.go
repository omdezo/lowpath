package capture

import (
	"fmt"
	"os"
	"runtime"

	"lowpath/internal/divert"
)

// available reports whether Start can work on this machine: nil, ErrNeedAdmin
// or ErrNoDriver. Driver load problems (blocked, missing BFE service) only
// show up when Start opens the handle.
func available(elevated bool) error {
	if runtime.GOARCH != "amd64" {
		return ErrNoDriver
	}
	if !elevated {
		return ErrNeedAdmin
	}
	return nil
}

// Start loads the driver from cfg.DriverDir and begins capturing the
// configured processes' UDP traffic.
func Start(cfg Config) (*Capture, error) {
	if err := cfg.check(); err != nil {
		return nil, err
	}
	if err := divert.Load(cfg.DriverDir); err != nil {
		return nil, err
	}
	// Map sockets to processes before the first packet is diverted, so game
	// packets hit the table from the start.
	o, err := newOwners(cfg.Processes, cfg.PIDs, uint32(os.Getpid()), cfg.Owners)
	if err != nil {
		return nil, fmt.Errorf("capture: list UDP sockets: %w", err)
	}
	h, err := divert.Open(buildFilter(cfg.Exclude), divert.LayerNetwork, 0, 0)
	if err != nil {
		return nil, err
	}
	c := newCapture(cfg, divertDevice{h}, o)
	c.run(min(4, max(2, runtime.NumCPU()/2)))
	c.log.Info("capture started", "processes", cfg.Processes, "pids", cfg.PIDs,
		"game_pids", o.gamePIDs(), "excluded", len(cfg.Exclude))
	return c, nil
}

type divertDevice struct{ *divert.Handle }

func (divertDevice) CalcChecksums(pkt []byte, addr *divert.Address) error {
	return divert.CalcChecksums(pkt, addr)
}
