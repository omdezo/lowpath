//go:build !windows

package capture

// WinDivert is Windows-only: elsewhere it is never available.
func available(elevated bool) error { return ErrUnsupported }

func Start(cfg Config) (*Capture, error) {
	if err := cfg.check(); err != nil {
		return nil, err
	}
	return nil, ErrUnsupported
}
