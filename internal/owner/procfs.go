package owner

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"strconv"
	"strings"
)

// Procfs is the Linux resolver. The socket table comes from /proc/net/udp
// and /proc/net/udp6 (local address and inode of every UDP socket in this
// network namespace), the owners from the socket:[inode] links in
// /proc/<pid>/fd, and the names from /proc/<pid>/comm, cmdline and exe.
// None of it needs root: the tables are world-readable, and a process's fd
// folder is readable by its own user, which is the user who runs the game.
// Sockets of other users' processes have no owner it can see and are left
// out. It reads through an fs.FS rooted at "/" so tests can feed it fixtures
// on any OS.
type Procfs struct{ fsys fs.FS }

// NewProcfs reads from fsys, which must be rooted at "/" (on Linux:
// os.DirFS("/")) and should implement fs.ReadLinkFS, as os.DirFS does.
func NewProcfs(fsys fs.FS) *Procfs { return &Procfs{fsys: fsys} }

func (p *Procfs) UDPSockets() ([]Socket, error) {
	byInode := map[uint64]netip.AddrPort{}
	read := 0
	for _, name := range []string{"proc/net/udp", "proc/net/udp6"} {
		f, err := p.fsys.Open(name)
		if err != nil {
			continue // udp6 is missing when IPv6 is off
		}
		err = parseProcNetUDP(f, byInode)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("owner: read /%s: %w", name, err)
		}
		read++
	}
	if read == 0 {
		return nil, errors.New("owner: /proc/net/udp can't be read")
	}
	var out []Socket
	if len(byInode) == 0 {
		return out, nil
	}
	err := p.eachSocket(func(pid uint32, ino uint64) bool {
		if local, ok := byInode[ino]; ok {
			out = append(out, Socket{Local: local, PID: pid})
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SocketOwners maps the socket inodes in want to the PIDs holding them. A
// socket shared by several processes (inherited across fork) goes to one
// of them. Sockets of other users' processes have no owner it can see and
// are left out.
func (p *Procfs) SocketOwners(want map[uint64]bool) (map[uint64]uint32, error) {
	out := map[uint64]uint32{}
	if len(want) == 0 {
		return out, nil
	}
	err := p.eachSocket(func(pid uint32, ino uint64) bool {
		if _, seen := out[ino]; want[ino] && !seen {
			out[ino] = pid
		}
		return len(out) < len(want)
	})
	return out, err
}

// eachSocket calls f with every socket:[inode] link in /proc/<pid>/fd this
// process may read, until f returns false.
func (p *Procfs) eachSocket(f func(pid uint32, ino uint64) bool) error {
	pids, err := p.pids()
	if err != nil {
		return err
	}
	for _, pid := range pids {
		dir := "proc/" + strconv.FormatUint(uint64(pid), 10) + "/fd"
		fds, err := fs.ReadDir(p.fsys, dir)
		if err != nil {
			continue // another user's process, or it exited
		}
		for _, fd := range fds {
			target, err := fs.ReadLink(p.fsys, dir+"/"+fd.Name())
			if err != nil {
				continue
			}
			if ino, ok := socketInode(target); ok && !f(pid, ino) {
				return nil
			}
		}
	}
	return nil
}

func (p *Procfs) ProcessNames() (map[uint32]string, error) {
	progs, err := p.Programs()
	if err != nil {
		return nil, err
	}
	out := make(map[uint32]string, len(progs))
	for _, pr := range progs {
		out[pr.PID] = pr.Name
	}
	return out, nil
}

func (p *Procfs) Programs() ([]Program, error) {
	pids, err := p.pids()
	if err != nil {
		return nil, err
	}
	out := make([]Program, 0, len(pids))
	for _, pid := range pids {
		if f, ok := p.facts(pid); ok {
			out = append(out, f.program())
		}
	}
	return out, nil
}

// facts reads one process; false when it exited meanwhile.
func (p *Procfs) facts(pid uint32) (procFacts, bool) {
	dir := "proc/" + strconv.FormatUint(uint64(pid), 10) + "/"
	comm, err := fs.ReadFile(p.fsys, dir+"comm")
	if err != nil {
		return procFacts{}, false
	}
	f := procFacts{pid: pid, comm: strings.TrimSuffix(string(comm), "\n")}
	if exe, err := fs.ReadLink(p.fsys, dir+"exe"); err == nil {
		// A replaced or removed program file still shows up, marked.
		f.exe = strings.TrimSuffix(exe, " (deleted)")
	}
	if cmd, err := fs.ReadFile(p.fsys, dir+"cmdline"); err == nil {
		f.argv0, _, _ = strings.Cut(string(cmd), "\x00")
	}
	if winePath(f.argv0) {
		if env, err := fs.ReadFile(p.fsys, dir+"environ"); err == nil {
			f.env = envLookup(strings.Split(string(env), "\x00"))
		}
	}
	return f, true
}

// pids lists the numeric entries of /proc. None at all means /proc isn't
// mounted (a bare container), which must not read as "nothing runs".
func (p *Procfs) pids() ([]uint32, error) {
	ents, err := fs.ReadDir(p.fsys, "proc")
	if err != nil {
		return nil, fmt.Errorf("owner: read /proc: %w", err)
	}
	out := make([]uint32, 0, len(ents))
	for _, e := range ents {
		if n, err := strconv.ParseUint(e.Name(), 10, 32); err == nil && n > 0 {
			out = append(out, uint32(n))
		}
	}
	if len(out) == 0 {
		return nil, errors.New("owner: /proc lists no processes (not mounted?)")
	}
	return out, nil
}

// socketInode parses an fd link target "socket:[12345]".
func socketInode(target string) (uint64, bool) {
	s, ok := strings.CutPrefix(target, "socket:[")
	if !ok {
		return 0, false
	}
	s, ok = strings.CutSuffix(s, "]")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseUint(s, 10, 64)
	return n, err == nil && n != 0
}

// parseProcNetUDP adds the rows of /proc/net/udp or udp6 to byInode:
//
//	sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode ...
//	 0: 0100007F:1B58 00000000:0000 07 00000000:00000000 00:00000000 00000000  1000        0 41234 ...
//
// The address is the kernel's in-memory bytes printed as 32-bit words in
// host byte order (so 127.0.0.1 is 0100007F on little-endian machines), the
// port is plain hex. Rows with inode 0 (sockets being torn down) are
// skipped, and so are rows that don't parse.
func parseProcNetUDP(r io.Reader, byInode map[uint64]netip.AddrPort) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	first := true
	for sc.Scan() {
		if first {
			first = false
			continue
		}
		f := strings.Fields(sc.Text())
		if len(f) < 10 {
			continue
		}
		local, ok := parseProcAddr(f[1])
		if !ok {
			continue
		}
		ino, err := strconv.ParseUint(f[9], 10, 64)
		if err != nil || ino == 0 {
			continue
		}
		byInode[ino] = local
	}
	return sc.Err()
}

// parseProcAddr parses "0100007F:1B58" or a 32-digit IPv6 form.
func parseProcAddr(s string) (netip.AddrPort, bool) {
	h, portHex, ok := strings.Cut(s, ":")
	if !ok || len(portHex) != 4 {
		return netip.AddrPort{}, false
	}
	port, err := strconv.ParseUint(portHex, 16, 16)
	if err != nil {
		return netip.AddrPort{}, false
	}
	raw, err := hex.DecodeString(h)
	if err != nil || (len(raw) != 4 && len(raw) != 16) {
		return netip.AddrPort{}, false
	}
	// Each 32-bit word was printed as a number: turn it back into the
	// bytes it was in memory.
	b := make([]byte, 0, len(raw))
	for w := 0; w < len(raw); w += 4 {
		b = binary.NativeEndian.AppendUint32(b, binary.BigEndian.Uint32(raw[w:]))
	}
	var a netip.Addr
	if len(b) == 4 {
		a = netip.AddrFrom4([4]byte(b))
	} else {
		a = netip.AddrFrom16([16]byte(b))
	}
	return netip.AddrPortFrom(a, uint16(port)), true
}
