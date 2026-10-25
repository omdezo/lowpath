//go:build !windows

package capture

// Available reports why capture can't work here: it is Windows-only.
func Available() error { return ErrUnsupported }

func Start(cfg Config) (*Capture, error) { return nil, ErrUnsupported }

func Processes() ([]Process, error) { return nil, ErrUnsupported }
