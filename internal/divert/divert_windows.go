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
	"time"
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

	procOpen                                             uintptr
	procCalcChecksums, procCompileFilter, procEvalFilter uintptr

	// dllProcs are the calls a Handle makes; tests give a Handle a table of
	// fakes instead.
	dllProcs procTable
)

// procTable holds the addresses of the WinDivert functions that take a
// HANDLE.
type procTable struct {
	recv, send, shutdown, close, setParam, getParam uintptr
}

var procNames = [...]struct {
	name string
	addr *uintptr
}{
	{"WinDivertOpen", &procOpen},
	{"WinDivertRecv", &dllProcs.recv},
	{"WinDivertSend", &dllProcs.send},
	{"WinDivertShutdown", &dllProcs.shutdown},
	{"WinDivertClose", &dllProcs.close},
	{"WinDivertSetParam", &dllProcs.setParam},
	{"WinDivertGetParam", &dllProcs.getParam},
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
// called from several goroutines at once, also while Close runs: Close waits
// for the calls already in the driver before it releases the handle, and every
// later call fails with ErrClosed.
type Handle struct {
	h     uintptr
	procs *procTable
	gate  *callGate
}

// releaseWait bounds how long Close waits for calls in flight. Send and the
// parameter calls return in microseconds and Close makes a blocked Recv
// return, so only a call stuck in the driver waits this long.
var releaseWait = 100 * time.Millisecond

func newHandle(h uintptr, procs *procTable) *Handle {
	return &Handle{h: h, procs: procs, gate: newCallGate()}
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
	return newHandle(r, &dllProcs), nil
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
	if len(buf) == 0 {
		return 0, fmt.Errorf("divert: recv: empty buffer")
	}
	if !h.gate.enter() {
		return 0, ErrClosed
	}
	defer h.gate.exit()
	var n uint32
	r, _, errno := syscall.SyscallN(h.procs.recv, h.h, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)),
		uintptr(unsafe.Pointer(&n)), uintptr(unsafe.Pointer(addr)))
	if r == 0 {
		return 0, ioError("recv", errno)
	}
	return int(n), nil
}

// Send injects pkt, outbound or inbound as addr says.
func (h *Handle) Send(pkt []byte, addr *Address) (int, error) {
	if len(pkt) == 0 {
		return 0, fmt.Errorf("divert: send: empty packet")
	}
	if !h.gate.enter() {
		return 0, ErrClosed
	}
	defer h.gate.exit()
	var n uint32
	r, _, errno := syscall.SyscallN(h.procs.send, h.h, uintptr(unsafe.Pointer(&pkt[0])), uintptr(len(pkt)),
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
	if !h.gate.enter() {
		return ErrClosed
	}
	defer h.gate.exit()
	if r, _, errno := syscall.SyscallN(h.procs.shutdown, h.h, how); r == 0 {
		return ioError("shutdown", errno)
	}
	return nil
}

// Close releases the handle. From then on the driver no longer diverts
// anything for it. Call Shutdown first and wait for receivers to return.
//
// Calls that are already running when Close starts finish first: Close
// shuts down receiving so that a blocked Recv returns, and waits up to
// releaseWait for them. A call still running after that is stuck. Close
// returns an error saying so, without waiting any longer, and the handle is
// released when that call returns: releasing it under the call would let
// the call reach whatever object Windows gives the value next, and a
// shut-down handle no longer diverts anything (the driver passes new
// packets straight on), so the capture fails open either way.
func (h *Handle) Close() error {
	inFlight, first := h.gate.shut()
	if !first {
		return nil
	}
	if inFlight > 0 {
		syscall.SyscallN(h.procs.shutdown, h.h, shutdownRecv) // the caller may not have; the driver accepts a repeat
		if n := h.gate.wait(releaseWait); n > 0 {
			go func() {
				<-h.gate.drained
				syscall.SyscallN(h.procs.close, h.h)
			}()
			return fmt.Errorf("divert: close: %d calls still running after %v; the handle is released when they return", n, releaseWait)
		}
	}
	if r, _, errno := syscall.SyscallN(h.procs.close, h.h); r == 0 {
		return fmt.Errorf("divert: close: %w", errno)
	}
	return nil
}

func (h *Handle) SetParam(p Param, v uint64) error {
	if !h.gate.enter() {
		return ErrClosed
	}
	defer h.gate.exit()
	if r, _, errno := syscall.SyscallN(h.procs.setParam, h.h, uintptr(p), uintptr(v)); r == 0 {
		return fmt.Errorf("divert: set param %d: %w", p, errno)
	}
	return nil
}

func (h *Handle) GetParam(p Param) (uint64, error) {
	if !h.gate.enter() {
		return 0, ErrClosed
	}
	defer h.gate.exit()
	var v uint64
	if r, _, errno := syscall.SyscallN(h.procs.getParam, h.h, uintptr(p), uintptr(unsafe.Pointer(&v))); r == 0 {
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
