package platform

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// routeCaps are the capabilities route mode's TUN backend needs: creating
// the device, its routes and nftables rules (CAP_NET_ADMIN), and the
// SO_MARK of its own relay socket (CAP_NET_RAW).
const routeCaps = "cap_net_admin,cap_net_raw+ep"

// Polkit is the Linux privilege helper. Rather than running the whole app
// (and its window, and the files it writes in the user's folders) as root,
// it asks once, through pkexec and the desktop's polkit password dialog, to
// give this program file the two capabilities route mode needs, then starts
// it again as the same user. The capabilities stay with the file until it
// is replaced (an update asks again).
type Polkit struct {
	Root
	x      execer
	exe    func() (string, error)
	mount  func(path string) (mountFlags, error)
	getcap func(path string) bool
	// noNewPrivs reports whether this process runs with no_new_privs, under
	// which execve grants no file capabilities (capabilities(7)).
	noNewPrivs func() bool
	// ownerOnly takes the group's and others' rights off the program file
	// when this user owns it, before it gets the capabilities: otherwise
	// every account on the PC could start a Lowpath that can change the
	// system's routes and firewall.
	ownerOnly func(path string) error
}

// mountFlags are the options of the filesystem a file is on that matter
// for file capabilities.
type mountFlags struct{ readOnly, noSUID bool }

// tools finds pkexec and setcap, which run as root: absolute paths only,
// never a program found through the user's PATH.
func (p *Polkit) tools() (pkexec, setcap string, err error) {
	pkexec, ok := firstFound(p.x, "/usr/bin/pkexec", "/bin/pkexec")
	if !ok {
		return "", "", errors.New("pkexec (polkit) is not installed")
	}
	setcap, ok = firstFound(p.x, "/usr/sbin/setcap", "/sbin/setcap", "/usr/bin/setcap", "/bin/setcap")
	if !ok {
		return "", "", errors.New("setcap is not installed (package libcap2-bin or libcap)")
	}
	return pkexec, setcap, nil
}

// grantable says why capabilities can't be given to exe: the kernel
// ignores them on a filesystem mounted nosuid, and can't store them on a
// read-only one (an AppImage, a mounted disk image).
func (p *Polkit) grantable(exe string) error {
	m, err := p.mount(exe)
	if err != nil {
		return err
	}
	switch {
	case m.readOnly:
		return fmt.Errorf("%s is on a read-only filesystem", exe)
	case m.noSUID:
		return fmt.Errorf("%s is on a filesystem mounted nosuid, where capabilities don't apply", exe)
	}
	return nil
}

// CanElevate reports whether the prompt can be shown and its answer would
// take effect.
func (p *Polkit) CanElevate() bool { return p.why() == nil }

func (p *Polkit) why() error {
	if _, _, err := p.tools(); err != nil {
		return err
	}
	// Inherited from a sandbox, a launcher or a NoNewPrivileges= unit: the
	// restarted copy would get no capabilities whatever the file carries,
	// and the button would restart Lowpath into the same problem forever.
	if p.noNewPrivs() {
		return errors.New("Lowpath was started with no_new_privs (a sandbox or service setting), which keeps programs from getting file capabilities")
	}
	exe, err := p.exe()
	if err != nil {
		return err
	}
	return p.grantable(exe)
}

// Relaunch asks for the capabilities and, once they are on the program
// file, starts it again with args (as this user: the new process gets them
// from the file).
func (p *Polkit) Relaunch(args []string) error {
	pkexec, setcap, err := p.tools()
	if err != nil {
		return fmt.Errorf("platform: can't ask for network rights: %w", err)
	}
	if err := p.why(); err != nil {
		return fmt.Errorf("platform: can't give Lowpath network rights: %w", err)
	}
	exe, err := p.exe()
	if err != nil {
		return err
	}
	if err := p.ownerOnly(exe); err != nil {
		return fmt.Errorf("platform: keep %s to its owner before giving it network rights: %w", exe, err)
	}
	if !p.getcap(exe) {
		code, stderr, err := p.x.Run(context.Background(), command{path: pkexec, args: []string{setcap, routeCaps, exe}})
		switch {
		case err != nil:
			return fmt.Errorf("platform: run pkexec: %w", err)
		case code == 126: // the user closed the password dialog
			return fmt.Errorf("ask for network rights: %w", ErrElevateCancelled)
		case code == 127:
			return fmt.Errorf("platform: polkit said no (wrong password, an account that isn't an administrator, or no polkit agent running): %s", strings.TrimSpace(lastLine(stderr)))
		case code != 0:
			return fmt.Errorf("platform: setcap failed (exit %d): %s", code, strings.TrimSpace(lastLine(stderr)))
		}
		if !p.getcap(exe) {
			return fmt.Errorf("platform: setcap reported success but %s has no capabilities", exe)
		}
	}
	if err := p.x.Spawn(command{path: exe, args: args}); err != nil {
		return fmt.Errorf("platform: start Lowpath again: %w", err)
	}
	return nil
}
