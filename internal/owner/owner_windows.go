package owner

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows reads the IP Helper UDP table (GetExtendedUdpTable with
// UDP_TABLE_OWNER_PID) and a Toolhelp32 process snapshot. Neither needs
// administrator rights.
type Windows struct {
	getExtendedUdpTable *windows.LazyProc
}

// New returns the resolver for this operating system.
func New() Resolver { return NewWindows() }

// NewWindows binds GetExtendedUdpTable from the system iphlpapi.dll.
func NewWindows() *Windows {
	return &Windows{getExtendedUdpTable: windows.NewLazySystemDLL("iphlpapi.dll").NewProc("GetExtendedUdpTable")}
}

const (
	afInet           = 2
	afInet6          = 23
	udpTableOwnerPID = 1 // UDP_TABLE_OWNER_PID

	udpRowLen  = 12 // MIB_UDPROW_OWNER_PID: addr, port, pid
	udp6RowLen = 28 // MIB_UDP6ROW_OWNER_PID: addr[16], scope, port, pid
)

func (w *Windows) UDPSockets() ([]Socket, error) {
	v4, err := w.udpTable(afInet)
	if err != nil {
		return nil, err
	}
	v6, err := w.udpTable(afInet6)
	if err != nil {
		return nil, err
	}
	out := make([]Socket, 0, 64)
	out = parseUDPRows(out, v4, udpRowLen)
	return parseUDPRows(out, v6, udp6RowLen), nil
}

// udpTable returns the raw MIB_UDP(6)TABLE_OWNER_PID for one family. On
// ERROR_INSUFFICIENT_BUFFER the call stores the size it needs in size; a
// little slack covers sockets opened before the next call.
func (w *Windows) udpTable(family uintptr) ([]byte, error) {
	size := uint32(16 << 10)
	for range 8 {
		buf := make([]byte, size)
		r, _, _ := w.getExtendedUdpTable.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)),
			0, family, udpTableOwnerPID, 0)
		switch syscall.Errno(r) {
		case 0:
			return buf, nil
		case windows.ERROR_INSUFFICIENT_BUFFER:
			size += 4 << 10
		default:
			return nil, fmt.Errorf("GetExtendedUdpTable: %w", syscall.Errno(r))
		}
	}
	return nil, fmt.Errorf("GetExtendedUdpTable: table keeps growing")
}

// parseUDPRows decodes a MIB_UDP(6)TABLE_OWNER_PID: a DWORD count, then rows
// whose last three DWORDs are the port (network order in the low 16 bits)
// and the PID, after a 4-byte IPv4 or a 16-byte IPv6 address plus scope id.
func parseUDPRows(out []Socket, b []byte, rowLen int) []Socket {
	if len(b) < 4 {
		return out
	}
	n := int(binary.LittleEndian.Uint32(b))
	rows := b[4:]
	for i := 0; i < n && len(rows) >= rowLen; i++ {
		row := rows[:rowLen]
		rows = rows[rowLen:]
		var a netip.Addr
		if rowLen == udpRowLen {
			a = netip.AddrFrom4([4]byte(row[0:4]))
		} else {
			a = netip.AddrFrom16([16]byte(row[0:16]))
		}
		port := binary.BigEndian.Uint16(row[rowLen-8:])
		pid := binary.LittleEndian.Uint32(row[rowLen-4:])
		out = append(out, Socket{netip.AddrPortFrom(a, port), pid})
	}
	return out
}

func (w *Windows) ProcessNames() (map[uint32]string, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("process snapshot: %w", err)
	}
	defer windows.CloseHandle(snap)
	out := make(map[uint32]string, 256)
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		out[e.ProcessID] = windows.UTF16ToString(e.ExeFile[:])
	}
	return out, nil
}
