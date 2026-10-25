//go:build !(windows && amd64)

package divert

import "runtime"

// errUnavailable: WinDivert 2.2 ships an x64 driver only.
var errUnavailable = func() error {
	if runtime.GOOS == "windows" {
		return ErrNoDriver
	}
	return ErrUnsupported
}()

func Install(dir string) error { return errUnavailable }
func Load(dir string) error    { return errUnavailable }

type Handle struct{}

func Open(filter string, layer Layer, priority int16, flags uint64) (*Handle, error) {
	return nil, errUnavailable
}

func (h *Handle) Recv(buf []byte, addr *Address) (int, error) { return 0, errUnavailable }
func (h *Handle) Send(pkt []byte, addr *Address) (int, error) { return 0, errUnavailable }
func (h *Handle) Shutdown() error                             { return errUnavailable }
func (h *Handle) ShutdownRecv() error                         { return errUnavailable }
func (h *Handle) Close() error                                { return nil }
func (h *Handle) SetParam(p Param, v uint64) error            { return errUnavailable }
func (h *Handle) GetParam(p Param) (uint64, error)            { return 0, errUnavailable }

func CalcChecksums(pkt []byte, addr *Address) error  { return errUnavailable }
func CompileFilter(filter string, layer Layer) error { return errUnavailable }
func EvalFilter(filter string, pkt []byte, addr *Address) (bool, error) {
	return false, errUnavailable
}
