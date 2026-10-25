//go:build windows && amd64

package divert

import (
	"bytes"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The official, Authenticode-signed WinDivert 2.2.2 x64 release files.
var (
	//go:embed bin/WinDivert.dll
	dllFile []byte
	//go:embed bin/WinDivert64.sys
	sysFile []byte
)

const (
	dllName = "WinDivert.dll"
	sysName = "WinDivert64.sys" // WinDivert.dll looks for it in its own directory
)

// Install writes WinDivert.dll and WinDivert64.sys into dir unless identical
// copies are already there.
func Install(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("divert: install: %w", err)
	}
	if err := writeIfDifferent(filepath.Join(dir, dllName), dllFile); err != nil {
		return err
	}
	return writeIfDifferent(filepath.Join(dir, sysName), sysFile)
}

// writeIfDifferent leaves a file alone when it already has the right bytes:
// a loaded DLL or a running driver can't be replaced, and an identical copy is
// exactly what a restart or a second instance finds.
func writeIfDifferent(path string, data []byte) error {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("divert: install: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("divert: install %s (is another WinDivert program running?): %w", filepath.Base(path), err)
	}
	return nil
}

var (
	loadMu    sync.Mutex
	loadedDir string
	ready     atomic.Bool // set once every proc below is resolved

	procOpen, procRecv, procSend, procShutdown, procClose uintptr
	procSetParam, procGetParam                            uintptr
	procCalcChecksums, procCompileFilter, procEvalFilter  uintptr
)

var procNames = [...]struct {
	name string
	addr *uintptr
}{
	{"WinDivertOpen", &procOpen},
	{"WinDivertRecv", &procRecv},
	{"WinDivertSend", &procSend},
	{"WinDivertShutdown", &procShutdown},
	{"WinDivertClose", &procClose},
	{"WinDivertSetParam", &procSetParam},
	{"WinDivertGetParam", &procGetParam},
	{"WinDivertHelperCalcChecksums", &procCalcChecksums},
	{"WinDivertHelperCompileFilter", &procCompileFilter},
	{"WinDivertHelperEvalFilter", &procEvalFilter},
}

// Load loads dir\WinDivert.dll. Later calls with the same dir are no-ops; a
// process can only use one copy.
func Load(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("divert: load: %w", err)
	}
	loadMu.Lock()
	defer loadMu.Unlock()
	if ready.Load() {
		if strings.EqualFold(loadedDir, abs) {
			return nil
		}
		return fmt.Errorf("divert: already loaded from %s", loadedDir)
	}
	// An absolute path makes Windows load exactly this file rather than
	// searching the DLL path for a WinDivert.dll of some other version.
	dll, err := syscall.LoadDLL(filepath.Join(abs, dllName))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNoDriver, err)
	}
	for _, p := range procNames {
		proc, err := dll.FindProc(p.name)
		if err != nil {
			dll.Release()
			return fmt.Errorf("%w: %v", ErrNoDriver, err)
		}
		*p.addr = proc.Addr()
	}
	loadedDir = abs
	ready.Store(true)
	return nil
}

// The calls below go through syscall.SyscallN rather than Proc.Call: Call is
// marked uintptrescapes, which would move every out-parameter (and the
// Address) to the heap on each packet.

// Handle is an open WinDivert handle. Recv, Send and the other methods may be
// called from several goroutines at once.
type Handle struct {
	h      uintptr
	closed atomic.Bool
}

// Open opens a handle that diverts every packet matching filter at layer.
// Without admin rights it fails with ErrNeedAdmin.
func Open(filter string, layer Layer, priority int16, flags uint64) (*Handle, error) {
	if !ready.Load() {
		return nil, ErrNotLoaded
	}
	f, err := syscall.BytePtrFromString(filter)
	if err != nil {
		return nil, fmt.Errorf("divert: filter: %w", err)
	}
	r, _, errno := syscall.SyscallN(procOpen, uintptr(unsafe.Pointer(f)), uintptr(layer),
		uintptr(int64(priority)), uintptr(flags))
	if syscall.Handle(r) == syscall.InvalidHandle {
		return nil, openError(errno)
	}
	return &Handle{h: r}, nil
}

// openError turns WinDivertOpen's GetLastError into something a user can act on.
func openError(errno syscall.Errno) error {
	switch errno {
	case 2: // ERROR_FILE_NOT_FOUND
		return fmt.Errorf("%w: %s is missing next to %s (%w)", ErrNoDriver, sysName, dllName, errno)
	case 5: // ERROR_ACCESS_DENIED
		return fmt.Errorf("%w (%w)", ErrNeedAdmin, errno)
	case 87: // ERROR_INVALID_PARAMETER
		return fmt.Errorf("divert: invalid filter, layer, priority or flags (%w)", errno)
	case 577: // ERROR_INVALID_IMAGE_HASH
		return fmt.Errorf("%w: Windows rejected the driver signature; a driver policy or outdated Windows may be blocking it (%w)", ErrNoDriver, errno)
	case 654: // ERROR_DRIVER_FAILED_PRIOR_UNLOAD
		return fmt.Errorf("%w: another program has a different WinDivert version loaded; close it or reboot (%w)", ErrNoDriver, errno)
	case 1275: // ERROR_DRIVER_BLOCKED
		return fmt.Errorf("%w: the driver is blocked by Windows or by security software (%w)", ErrNoDriver, errno)
	case 1753: // EPT_S_NOT_REGISTERED
		return fmt.Errorf("%w: the Base Filtering Engine service is not running (%w)", ErrNoDriver, errno)
	}
	return fmt.Errorf("divert: WinDivertOpen: %w", errno)
}

// ioError maps the errors a shut-down or closed handle produces to ErrClosed.
func ioError(op string, errno syscall.Errno) error {
	switch errno {
	case 6, 232, 995: // ERROR_INVALID_HANDLE, ERROR_NO_DATA, ERROR_OPERATION_ABORTED
		return ErrClosed
	}
	return fmt.Errorf("divert: %s: %w", op, errno)
}

// Recv blocks until a packet matching the filter arrives, copies it into buf
// and describes it in addr. Use a buffer of MTUMax bytes.
func (h *Handle) Recv(buf []byte, addr *Address) (int, error) {
	if h.closed.Load() {
		return 0, ErrClosed
	}
	if len(buf) == 0 {
		return 0, fmt.Errorf("divert: recv: empty buffer")
	}
	var n uint32
	r, _, errno := syscall.SyscallN(procRecv, h.h, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)),
		uintptr(unsafe.Pointer(&n)), uintptr(unsafe.Pointer(addr)))
	if r == 0 {
		return 0, ioError("recv", errno)
	}
	return int(n), nil
}

// Send injects pkt, outbound or inbound as addr says.
func (h *Handle) Send(pkt []byte, addr *Address) (int, error) {
	if h.closed.Load() {
		return 0, ErrClosed
	}
	if len(pkt) == 0 {
		return 0, fmt.Errorf("divert: send: empty packet")
	}
	var n uint32
	r, _, errno := syscall.SyscallN(procSend, h.h, uintptr(unsafe.Pointer(&pkt[0])), uintptr(len(pkt)),
		uintptr(unsafe.Pointer(&n)), uintptr(unsafe.Pointer(addr)))
	if r == 0 {
		return 0, ioError("send", errno)
	}
	return int(n), nil
}

const (
	shutdownRecv = 0x1
	shutdownBoth = 0x3
)

// Shutdown stops both directions: blocked and later Recv calls return
// ErrClosed once the queue is empty, and Send fails.
func (h *Handle) Shutdown() error { return h.shutdown(shutdownBoth) }

// ShutdownRecv stops new packets from being queued but keeps Send working,
// so packets already taken off the wire can still be put back.
func (h *Handle) ShutdownRecv() error { return h.shutdown(shutdownRecv) }

func (h *Handle) shutdown(how uintptr) error {
	if h.closed.Load() {
		return ErrClosed
	}
	if r, _, errno := syscall.SyscallN(procShutdown, h.h, how); r == 0 {
		return ioError("shutdown", errno)
	}
	return nil
}

// Close releases the handle. From then on the driver no longer diverts
// anything for it. Call Shutdown first and wait for receivers to return.
func (h *Handle) Close() error {
	if !h.closed.CompareAndSwap(false, true) {
		return nil
	}
	if r, _, errno := syscall.SyscallN(procClose, h.h); r == 0 {
		return fmt.Errorf("divert: close: %w", errno)
	}
	return nil
}

func (h *Handle) SetParam(p Param, v uint64) error {
	if h.closed.Load() {
		return ErrClosed
	}
	if r, _, errno := syscall.SyscallN(procSetParam, h.h, uintptr(p), uintptr(v)); r == 0 {
		return fmt.Errorf("divert: set param %d: %w", p, errno)
	}
	return nil
}

func (h *Handle) GetParam(p Param) (uint64, error) {
	if h.closed.Load() {
		return 0, ErrClosed
	}
	var v uint64
	if r, _, errno := syscall.SyscallN(procGetParam, h.h, uintptr(p), uintptr(unsafe.Pointer(&v))); r == 0 {
		return 0, fmt.Errorf("divert: get param %d: %w", p, errno)
	}
	return v, nil
}

// CalcChecksums recomputes every IPv4/UDP/TCP/ICMP checksum in pkt and, when
// addr is not nil, marks them valid in addr.
func CalcChecksums(pkt []byte, addr *Address) error {
	if !ready.Load() {
		return ErrNotLoaded
	}
	if len(pkt) == 0 {
		return fmt.Errorf("divert: checksums: empty packet")
	}
	r, _, _ := syscall.SyscallN(procCalcChecksums, uintptr(unsafe.Pointer(&pkt[0])), uintptr(len(pkt)),
		uintptr(unsafe.Pointer(addr)), 0)
	if r == 0 {
		return fmt.Errorf("divert: checksums: not an IP packet")
	}
	return nil
}

// CompileFilter checks a filter string for layer the way Open would, without
// admin rights or the driver.
func CompileFilter(filter string, layer Layer) error {
	if !ready.Load() {
		return ErrNotLoaded
	}
	f, err := syscall.BytePtrFromString(filter)
	if err != nil {
		return fmt.Errorf("divert: filter: %w", err)
	}
	var msg *byte // points into the DLL's static strings
	var pos uint32
	r, _, _ := syscall.SyscallN(procCompileFilter, uintptr(unsafe.Pointer(f)), uintptr(layer), 0, 0,
		uintptr(unsafe.Pointer(&msg)), uintptr(unsafe.Pointer(&pos)))
	if r == 0 {
		return fmt.Errorf("divert: filter: %s at position %d", windows.BytePtrToString(msg), pos)
	}
	return nil
}

// EvalFilter reports whether a packet with addr would match filter. The layer
// comes from addr. Like CompileFilter it needs neither admin nor the driver.
func EvalFilter(filter string, pkt []byte, addr *Address) (bool, error) {
	if err := CompileFilter(filter, addr.Layer()); err != nil {
		return false, err
	}
	if len(pkt) == 0 {
		return false, fmt.Errorf("divert: eval: empty packet")
	}
	f, _ := syscall.BytePtrFromString(filter) // CompileFilter already accepted it
	r, _, _ := syscall.SyscallN(procEvalFilter, uintptr(unsafe.Pointer(f)), uintptr(unsafe.Pointer(&pkt[0])),
		uintptr(len(pkt)), uintptr(unsafe.Pointer(addr)))
	return r != 0, nil
}
