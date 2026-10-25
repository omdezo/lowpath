// Package divert is a pure-Go (no cgo) binding to WinDivert 2.2, a signed
// Windows kernel driver that lets a user-mode program take packets off the
// network stack, drop or modify them, and inject new ones. Lowpath uses it to
// capture a game's outbound UDP packets and to put the relays' replies back
// into the game's socket without the game knowing.
//
// The driver (WinDivert64.sys) and its user-mode library (WinDivert.dll) are
// embedded in the binary: Install writes them to a directory and Load loads
// the DLL from there. Opening a handle needs administrator rights. The helper
// functions (CompileFilter, EvalFilter, CalcChecksums) run entirely in user
// mode, need neither admin nor the driver, and are what the tests use.
//
// Only 64-bit x86 Windows is supported (WinDivert 2.2 ships no ARM64 driver);
// elsewhere every call fails with ErrUnsupported or ErrNoDriver.
package divert

import (
	"encoding/binary"
	"errors"
)

// Layer selects where in the network stack a handle sits (WINDIVERT_LAYER).
type Layer uint32

const (
	LayerNetwork        Layer = 0 // packets to and from this machine
	LayerNetworkForward Layer = 1 // packets routed through this machine
	LayerFlow           Layer = 2
	LayerSocket         Layer = 3
	LayerReflect        Layer = 4
)

// Event is what an Address describes (WINDIVERT_EVENT). Network-layer
// handles only ever see EventNetworkPacket.
type Event uint8

const EventNetworkPacket Event = 0

// Flags for Open (WINDIVERT_FLAG_*).
const (
	FlagSniff     uint64 = 0x0001 // copy packets instead of taking them
	FlagDrop      uint64 = 0x0002 // drop matching packets without reading them
	FlagRecvOnly  uint64 = 0x0004
	FlagSendOnly  uint64 = 0x0008
	FlagNoInstall uint64 = 0x0010 // fail instead of installing the driver
	FlagFragments uint64 = 0x0020
)

// Param is a handle parameter for SetParam / GetParam (WINDIVERT_PARAM).
type Param uint32

const (
	ParamQueueLength  Param = 0 // packets queued before new ones are dropped
	ParamQueueTime    Param = 1 // ms a packet may wait in the queue
	ParamQueueSize    Param = 2 // bytes queued
	ParamVersionMajor Param = 3
	ParamVersionMinor Param = 4
)

const (
	PriorityHighest = 30000
	PriorityLowest  = -PriorityHighest

	// MTUMax is the largest packet Recv can return (WINDIVERT_MTU_MAX); a
	// receive buffer this big never truncates.
	MTUMax = 40 + 0xFFFF
)

var (
	// ErrNeedAdmin: the driver can only be loaded (and a handle opened) by an
	// elevated process.
	ErrNeedAdmin = errors.New("divert: administrator rights are required to capture packets")
	// ErrNoDriver: the WinDivert files are missing or Windows refused to load
	// the driver. The wrapped error says which.
	ErrNoDriver = errors.New("divert: the WinDivert driver is not available")
	// ErrUnsupported: not 64-bit Windows.
	ErrUnsupported = errors.New("divert: packet capture needs 64-bit Windows")
	// ErrNotLoaded: Load has not been called (or failed).
	ErrNotLoaded = errors.New("divert: WinDivert.dll is not loaded")
	// ErrClosed: the handle was shut down or closed. Recv returns it once a
	// shut-down handle's queue is empty.
	ErrClosed = errors.New("divert: handle closed")
)

// Address mirrors WINDIVERT_ADDRESS (80 bytes). Recv fills it in and Send
// reads it: for a network-layer packet it says the direction and the
// interface. Only the network-layer member of the union is exposed.
type Address struct {
	Timestamp int64 // QueryPerformanceCounter value when the packet was seen

	// Layer:8 Event:8 Sniffed:1 Outbound:1 Loopback:1 Impostor:1 IPv6:1
	// IPChecksum:1 TCPChecksum:1 UDPChecksum:1 Reserved1:8, LSB first.
	bits uint32
	_    uint32   // Reserved2
	data [64]byte // union; NETWORK = {UINT32 IfIdx; UINT32 SubIfIdx}
}

const (
	bitSniffed     = 1 << 16
	bitOutbound    = 1 << 17
	bitLoopback    = 1 << 18
	bitImpostor    = 1 << 19
	bitIPv6        = 1 << 20
	bitIPChecksum  = 1 << 21
	bitTCPChecksum = 1 << 22
	bitUDPChecksum = 1 << 23
)

func (a *Address) Layer() Layer   { return Layer(a.bits & 0xff) }
func (a *Address) Event() Event   { return Event(a.bits >> 8 & 0xff) }
func (a *Address) Sniffed() bool  { return a.bits&bitSniffed != 0 }
func (a *Address) Outbound() bool { return a.bits&bitOutbound != 0 }
func (a *Address) Loopback() bool { return a.bits&bitLoopback != 0 }
func (a *Address) Impostor() bool { return a.bits&bitImpostor != 0 }
func (a *Address) IPv6() bool     { return a.bits&bitIPv6 != 0 }

// IPChecksum, TCPChecksum and UDPChecksum report whether the packet's
// checksums are known to be valid (CalcChecksums sets them).
func (a *Address) IPChecksum() bool  { return a.bits&bitIPChecksum != 0 }
func (a *Address) TCPChecksum() bool { return a.bits&bitTCPChecksum != 0 }
func (a *Address) UDPChecksum() bool { return a.bits&bitUDPChecksum != 0 }

// SetLayer sets the layer and clears the event, which is all a packet built
// from scratch needs (the zero Address is already LayerNetwork).
func (a *Address) SetLayer(l Layer) { a.bits = a.bits&^0xffff | uint32(l)&0xff }

// SetOutbound picks the direction Send injects in: outbound packets go out
// through the interface, inbound packets are delivered up the local stack as
// if they had just arrived on it.
func (a *Address) SetOutbound(v bool) { a.setBit(bitOutbound, v) }
func (a *Address) SetLoopback(v bool) { a.setBit(bitLoopback, v) }
func (a *Address) SetImpostor(v bool) { a.setBit(bitImpostor, v) }
func (a *Address) SetIPv6(v bool)     { a.setBit(bitIPv6, v) }

func (a *Address) setBit(bit uint32, v bool) {
	if v {
		a.bits |= bit
	} else {
		a.bits &^= bit
	}
}

// IfIdx and SubIfIdx identify the interface a network-layer packet used.
func (a *Address) IfIdx() uint32    { return binary.LittleEndian.Uint32(a.data[0:4]) }
func (a *Address) SubIfIdx() uint32 { return binary.LittleEndian.Uint32(a.data[4:8]) }

// SetNetwork sets the interface a packet is injected on.
func (a *Address) SetNetwork(ifIdx, subIfIdx uint32) {
	binary.LittleEndian.PutUint32(a.data[0:4], ifIdx)
	binary.LittleEndian.PutUint32(a.data[4:8], subIfIdx)
}
