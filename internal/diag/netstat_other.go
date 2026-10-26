//go:build !windows && !linux && !darwin

package diag

// unsupportedNetStats is for platforms without an implementation: callers
// show "—" for throughput and the gateway.
type unsupportedNetStats struct{}

func newPlatformNetStats() NetStats { return unsupportedNetStats{} }

func (unsupportedNetStats) DefaultRoute() (Route, error)   { return Route{}, ErrUnsupported }
func (unsupportedNetStats) Counters(int) (Counters, error) { return Counters{}, ErrUnsupported }
