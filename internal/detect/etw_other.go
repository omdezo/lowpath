//go:build !windows

package detect

import (
	"errors"
	"fmt"
	"runtime"
)

// KernelNetwork is the flow watcher of systems without one yet: Linux
// (eBPF or /proc/net/udp would do, issue #43) and macOS. Watch reports
// errors.ErrUnsupported so the UI can say so.
type KernelNetwork struct{}

func NewKernelNetwork() *KernelNetwork { return &KernelNetwork{} }

func (*KernelNetwork) Watch(func(FlowEvent)) (FlowWatch, error) {
	return nil, fmt.Errorf("watching game flows without a boost is not available on %s yet: %w", runtime.GOOS, errors.ErrUnsupported)
}

// Reset has nothing to stop: no watch here outlives its process.
func (*KernelNetwork) Reset() (bool, error) { return false, nil }
