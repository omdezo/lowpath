package owner

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Darwin is the macOS resolver: the process list from the kern.proc.all
// sysctl, each process's sockets from libproc's proc_pidinfo
// (PROC_PIDLISTFDS) and proc_pidfdinfo (PROC_PIDFDSOCKETINFO), the program
// path from proc_pidpath and argv[0] from kern.procargs2. libproc is a thin
// wrapper over the proc_info system call, which this calls directly, so the
// binary needs no cgo. macOS answers these for the user's own processes
// without root; other users' processes are skipped, the way lsof skips
// them.
type Darwin struct{}

// New returns the resolver for this operating system.
func New() Resolver { return NewDarwin() }

// NewTable returns the resolver with the program list game detection uses.
func NewTable() Table { return NewDarwin() }

func NewDarwin() *Darwin { return &Darwin{} }

// procInfo is __proc_info(callnum, pid, flavor, arg, buffer, buffersize),
// the call behind proc_pidinfo, proc_pidfdinfo and proc_pidpath. It returns
// the bytes written (or, with an empty buffer, the size needed).
func procInfo(call int, pid int32, flavor int, arg uint64, buf []byte) (int, error) {
	var p unsafe.Pointer
	if len(buf) > 0 {
		p = unsafe.Pointer(&buf[0])
	}
	r, _, e := unix.Syscall6(unix.SYS_PROC_INFO, uintptr(call), uintptr(pid), uintptr(flavor),
		uintptr(arg), uintptr(p), uintptr(len(buf)))
	if e != 0 {
		return 0, e
	}
	return int(r), nil
}

func (d *Darwin) procs() ([]unix.KinfoProc, error) {
	ps, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, fmt.Errorf("owner: list processes: %w", err)
	}
	if len(ps) == 0 {
		return nil, errors.New("owner: the process list is empty")
	}
	return ps, nil
}

func (d *Darwin) UDPSockets() ([]Socket, error) {
	ps, err := d.procs()
	if err != nil {
		return nil, err
	}
	var out []Socket
	info := make([]byte, socketFDInfoLen+256) // room in case the struct grows
	for _, kp := range ps {
		pid := kp.Proc.P_pid
		if pid <= 0 {
			continue
		}
		for _, fd := range d.socketFDs(pid) {
			n, err := procInfo(procInfoCallPIDFDInfo, pid, procPIDFDSocketInfo, uint64(fd), info)
			if err != nil {
				continue // closed meanwhile
			}
			if local, ok := parseSocketFDInfo(info[:n]); ok {
				out = append(out, Socket{Local: local, PID: uint32(pid)})
			}
		}
	}
	return out, nil
}

// socketFDs lists a process's socket descriptors; none when it exited or
// belongs to another user (EPERM).
func (d *Darwin) socketFDs(pid int32) []int32 {
	need, err := procInfo(procInfoCallPIDInfo, pid, procPIDListFDs, 0, nil)
	if err != nil || need <= 0 {
		return nil
	}
	buf := make([]byte, need+32*procFDInfoLen) // descriptors opened since
	n, err := procInfo(procInfoCallPIDInfo, pid, procPIDListFDs, 0, buf)
	if err != nil {
		return nil
	}
	return parseProcFDs(buf[:min(n, len(buf))])
}

func (d *Darwin) ProcessNames() (map[uint32]string, error) {
	progs, err := d.Programs()
	if err != nil {
		return nil, err
	}
	out := make(map[uint32]string, len(progs))
	for _, p := range progs {
		out[p.PID] = p.Name
	}
	return out, nil
}

func (d *Darwin) Programs() ([]Program, error) {
	ps, err := d.procs()
	if err != nil {
		return nil, err
	}
	out := make([]Program, 0, len(ps))
	path := make([]byte, procPIDPathInfoMax)
	for _, kp := range ps {
		pid := kp.Proc.P_pid
		if pid <= 0 {
			continue
		}
		f := procFacts{pid: uint32(pid), comm: cString(kp.Proc.P_comm[:])}
		// libproc's proc_pidpath reads the path up to its NUL rather than
		// trusting the return value, and so does this.
		path[0] = 0
		if _, err := procInfo(procInfoCallPIDInfo, pid, procPIDPathInfo, 0, path); err == nil {
			f.exe = cString(path)
		}
		if args, err := unix.SysctlRaw("kern.procargs2", int(pid)); err == nil {
			var env []string
			f.argv0, env = parseProcArgs2(args)
			if winePath(f.argv0) {
				f.env = envLookup(env)
			}
		}
		out = append(out, f.program())
	}
	return out, nil
}
