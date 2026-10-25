package capture

import (
	"cmp"
	"encoding/binary"
	"fmt"
	"net/netip"
	"os"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"lowpath/internal/divert"
)

// Available reports whether Start can work on this machine: nil, ErrNeedAdmin
// or ErrNoDriver. Driver load problems (blocked, missing BFE service) only
// show up when Start opens the handle.
func Available() error {
	if runtime.GOARCH != "amd64" {
		return ErrNoDriver
	}
	if !divert.IsAdmin() {
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
	o, err := newOwners(cfg.Processes, cfg.PIDs, uint32(os.Getpid()), udpSockets, processNames)
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

var (
	iphlpapi                = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetExtendedUdpTable = iphlpapi.NewProc("GetExtendedUdpTable")
)

const (
	afInet           = 2
	afInet6          = 23
	udpTableOwnerPID = 1 // UDP_TABLE_OWNER_PID

	udpRowLen  = 12 // MIB_UDPROW_OWNER_PID: addr, port, pid
	udp6RowLen = 28 // MIB_UDP6ROW_OWNER_PID: addr[16], scope, port, pid
)

// udpSockets lists every IPv4 and IPv6 UDP socket with its owning PID. It
// needs no admin rights.
func udpSockets() ([]socket, error) {
	v4, err := udpTable(afInet)
	if err != nil {
		return nil, err
	}
	v6, err := udpTable(afInet6)
	if err != nil {
		return nil, err
	}
	out := make([]socket, 0, 64)
	out = parseUDPRows(out, v4, udpRowLen)
	return parseUDPRows(out, v6, udp6RowLen), nil
}

// udpTable returns the raw MIB_UDP(6)TABLE_OWNER_PID for one family.
func udpTable(family uintptr) ([]byte, error) {
	size := uint32(16 << 10)
	for range 8 {
		buf := make([]byte, size)
		r, _, _ := procGetExtendedUdpTable.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)),
			0, family, udpTableOwnerPID, 0)
		switch syscall.Errno(r) {
		case 0:
			return buf, nil
		case windows.ERROR_INSUFFICIENT_BUFFER:
			size += 4 << 10 // sockets may be opened before the next call
		default:
			return nil, fmt.Errorf("GetExtendedUdpTable: %w", syscall.Errno(r))
		}
	}
	return nil, fmt.Errorf("GetExtendedUdpTable: table keeps growing")
}

// parseUDPRows decodes a MIB_UDP(6)TABLE_OWNER_PID: a DWORD count, then rows
// whose last three DWORDs are the port (network order in the low 16 bits)
// and the PID, after a 4-byte IPv4 or a 16-byte IPv6 address plus scope id.
func parseUDPRows(out []socket, b []byte, rowLen int) []socket {
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
		out = append(out, socket{netip.AddrPortFrom(a, port), pid})
	}
	return out
}

// processNames maps every running PID to its image name ("cs2.exe").
func processNames() (map[uint32]string, error) {
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

// Processes lists the programs that have UDP sockets open, sorted by name, to
// pick a game from. System processes and Lowpath itself are left out.
func Processes() ([]Process, error) {
	socks, err := udpSockets()
	if err != nil {
		return nil, err
	}
	names, err := processNames()
	if err != nil {
		return nil, err
	}
	self := uint32(os.Getpid())
	byPID := map[uint32]*Process{}
	for _, s := range socks {
		if s.pid <= 4 || s.pid == self { // 0 idle, 4 System
			continue
		}
		name, ok := names[s.pid]
		if !ok {
			continue // exited since the socket table was read
		}
		p := byPID[s.pid]
		if p == nil {
			p = &Process{PID: s.pid, Name: name}
			byPID[s.pid] = p
		}
		if !slices.Contains(p.UDPPorts, s.local.Port()) {
			p.UDPPorts = append(p.UDPPorts, s.local.Port())
		}
	}
	out := make([]Process, 0, len(byPID))
	for _, p := range byPID {
		slices.Sort(p.UDPPorts)
		out = append(out, *p)
	}
	slices.SortFunc(out, func(a, b Process) int {
		if c := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
			return c
		}
		return cmp.Compare(a.PID, b.PID)
	})
	return out, nil
}
