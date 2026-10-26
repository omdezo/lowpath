package platform

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// PipeName is the service's API pipe.
const PipeName = `\\.\pipe\lowpath`

// pipeSDDL is the pipe's security descriptor (protected, so nothing is
// inherited):
//
//   - SYSTEM, Administrators and the pipe's owner (OW: the account that
//     created it, SYSTEM for the service) get full control, which includes
//     creating more instances of the pipe: the listener makes one per
//     waiting client.
//   - Interactive Users (IU, everyone signed in at the PC or over Remote
//     Desktop; not services, not network logons) may read, write and read
//     attributes: 0x120083 = FILE_READ_DATA | FILE_WRITE_DATA |
//     FILE_READ_ATTRIBUTES | READ_CONTROL | SYNCHRONIZE. Not GENERIC_WRITE:
//     it includes FILE_APPEND_DATA, which on a pipe is
//     FILE_CREATE_PIPE_INSTANCE, and would let any user add a server end
//     and answer the window in the service's place.
//
// The listener also rejects remote clients (FILE_PIPE_REJECT_REMOTE_CLIENTS)
// and creates the first instance with FILE_CREATE, so it fails instead of
// joining a pipe someone else made first.
const pipeSDDL = "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;OW)(A;;0x120083;;;IU)"

// pipeClientAccess is what the window opens the pipe with: read and write
// data, and read attributes (GetNamedPipeInfo and
// GetNamedPipeServerProcessId need it).
const pipeClientAccess = windows.FILE_READ_DATA | windows.FILE_WRITE_DATA | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE

// Pipe is the Windows ServiceAPI: a named pipe.
type Pipe struct {
	name string
	// serverPID is the installed service's process id; Dial refuses any
	// other server process. Nil: no check (a test or a service run by hand
	// on a pipe of its own).
	serverPID func() (uint32, error)
}

// NewPipe makes the pipe called name. serverPID, when set, names the only
// process the window may talk to (WindowsService.PID).
func NewPipe(name string, serverPID func() (uint32, error)) *Pipe {
	return &Pipe{name: name, serverPID: serverPID}
}

func (p *Pipe) String() string { return p.name }

func (p *Pipe) Listen() (net.Listener, error) {
	ln, err := winio.ListenPipe(p.name, &winio.PipeConfig{
		SecurityDescriptor: pipeSDDL,
		InputBufferSize:    64 << 10,
		OutputBufferSize:   64 << 10,
	})
	if err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_PIPE_BUSY) {
			return nil, fmt.Errorf("service pipe %s: another program holds it: %w", p.name, err)
		}
		return nil, fmt.Errorf("service pipe %s: %w", p.name, err)
	}
	return ln, nil
}

// Dial opens the pipe and, when the pipe is the installed service's, checks
// that the process at the other end is that service.
func (p *Pipe) Dial(ctx context.Context) (net.Conn, error) {
	c, err := winio.DialPipeAccess(ctx, p.name, pipeClientAccess)
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			return nil, fmt.Errorf("%w (%s)", ErrServiceNotRunning, p.name)
		}
		return nil, fmt.Errorf("service pipe %s: %w", p.name, err)
	}
	if p.serverPID == nil {
		return c, nil
	}
	if err := p.checkServer(c); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func (p *Pipe) checkServer(c net.Conn) error {
	f, ok := c.(interface{ Fd() uintptr })
	if !ok {
		return fmt.Errorf("service pipe %s: no handle to check the server on", p.name)
	}
	var got uint32
	if err := windows.GetNamedPipeServerProcessId(windows.Handle(f.Fd()), &got); err != nil {
		return fmt.Errorf("service pipe %s: server process: %w", p.name, err)
	}
	want, err := p.serverPID()
	if err != nil {
		return fmt.Errorf("service pipe %s: %w", p.name, err)
	}
	if got != want {
		return fmt.Errorf("%w: %s is served by process %d, the service is process %d", ErrNotService, p.name, got, want)
	}
	return nil
}

// ServiceAPIAt is the API pipe called name, with no check of the process
// serving it: for a service run by hand on a pipe of its own (development
// and the end-to-end test), never for the installed service's PipeName.
func ServiceAPIAt(name string) ServiceAPI { return NewPipe(name, nil) }
