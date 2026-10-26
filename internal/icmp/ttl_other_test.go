//go:build !windows

package icmp

import (
	"errors"
	"net"
	"net/netip"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSocketError(t *testing.T) {
	open := func(errno syscall.Errno) error {
		return &net.OpError{Op: "listen", Net: "ip4:icmp", Err: os.NewSyscallError("socket", errno)}
	}
	if err := socketError(open(syscall.EPERM)); !errors.Is(err, os.ErrPermission) {
		t.Errorf("EPERM: %v, want a permission error", err)
	}
	if err := socketError(open(syscall.EACCES)); !errors.Is(err, os.ErrPermission) {
		t.Errorf("EACCES: %v, want a permission error", err)
	}
	if err := socketError(open(syscall.EAFNOSUPPORT)); err != ErrNoRoute {
		t.Errorf("EAFNOSUPPORT: %v, want ErrNoRoute", err)
	}
	for _, errno := range []syscall.Errno{syscall.EPROTONOSUPPORT, syscall.ESOCKTNOSUPPORT} {
		if err := socketError(open(errno)); !errors.Is(err, errors.ErrUnsupported) || !errors.Is(err, errno) {
			t.Errorf("%v: %v, want errors.ErrUnsupported wrapping it", errno, err)
		}
	}
}

func TestSendError(t *testing.T) {
	send := func(errno syscall.Errno) error {
		return &net.OpError{Op: "write", Net: "ip4:icmp", Err: os.NewSyscallError("sendto", errno)}
	}
	for _, errno := range []syscall.Errno{syscall.ENETUNREACH, syscall.EHOSTUNREACH, syscall.EADDRNOTAVAIL} {
		if err := sendError(send(errno)); err != ErrNoRoute {
			t.Errorf("%v: %v, want ErrNoRoute", errno, err)
		}
	}
	// A firewall rule (EPERM) or a prohibit route (EACCES) refuses this
	// send, not the socket: no permission error, and the errno is named.
	for _, errno := range []syscall.Errno{syscall.EPERM, syscall.EACCES} {
		err := sendError(send(errno))
		if err == nil || errors.Is(err, os.ErrPermission) || !strings.Contains(err.Error(), "refused to send") || !strings.Contains(err.Error(), errno.Error()) {
			t.Errorf("%v: %v, want a refused send that isn't a permission error", errno, err)
		}
	}
	if err := sendError(send(syscall.EMSGSIZE)); !errors.Is(err, syscall.EMSGSIZE) {
		t.Errorf("EMSGSIZE: %v, want it unchanged", err)
	}
}

// A send the system refuses is the probe's answer: the chain keeps the
// raw socket for the next probe instead of falling back for good.
func TestTTLChainKeepsSocketAfterRefusedSend(t *testing.T) {
	raw := &fakeSender{err: sendError(os.NewSyscallError("sendto", syscall.EPERM))}
	ping := &fakeSender{from: "198.51.100.1"}
	chain := newTTLChain("root", raw, ping)
	for range 2 {
		if _, err := chain.sendTTL(netip.MustParseAddr("203.0.113.9"), 3, time.Second); err == nil || errors.Is(err, os.ErrPermission) {
			t.Fatalf("got %v, want the raw socket's refused send", err)
		}
	}
	if raw.calls != 2 || ping.calls != 0 {
		t.Fatalf("calls: raw %d, ping %d; want 2, 0", raw.calls, ping.calls)
	}
}
