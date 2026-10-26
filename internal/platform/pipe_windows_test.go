package platform

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func testPipeName(t *testing.T) string {
	var b [8]byte
	rand.Read(b[:])
	return `\\.\pipe\lowpath-test-` + hex.EncodeToString(b[:])
}

func echo(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() { io.Copy(c, c); c.Close() }()
	}
}

func roundTrip(t *testing.T, c net.Conn) {
	t.Helper()
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo: %q, %v", buf, err)
	}
}

// The pipe works for its creator without administrator rights (the owner
// may add instances, so many windows and requests can connect at once), and
// the window checks who serves it.
func TestPipe(t *testing.T) {
	name := testPipeName(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	self := func() (uint32, error) { return uint32(os.Getpid()), nil }
	api := NewPipe(name, self)
	if _, err := api.Dial(ctx); !errors.Is(err, ErrServiceNotRunning) {
		t.Fatalf("dial with no service: %v, want ErrServiceNotRunning", err)
	}

	ln, err := api.Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go echo(ln)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := api.Dial(ctx)
			if err != nil {
				t.Error(err)
				return
			}
			roundTrip(t, c)
		}()
	}
	wg.Wait()

	// Anyone else serving the name is refused, before a byte is sent.
	other := NewPipe(name, func() (uint32, error) { return uint32(os.Getpid()) + 4, nil })
	if _, err := other.Dial(ctx); !errors.Is(err, ErrNotService) {
		t.Errorf("dial a pipe served by another process: %v, want ErrNotService", err)
	}
	// And so is everything while the service manager says it isn't running.
	down := NewPipe(name, func() (uint32, error) { return 0, ErrServiceNotRunning })
	if _, err := down.Dial(ctx); !errors.Is(err, ErrServiceNotRunning) {
		t.Errorf("dial while the service is stopped: %v, want ErrServiceNotRunning", err)
	}
	// Unchecked (a service run by hand on a pipe of its own).
	c, err := ServiceAPIAt(name).Dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, c)

	// A second server can't take the name while it is served.
	if ln2, err := NewPipe(name, nil).Listen(); err == nil {
		ln2.Close()
		t.Fatal("a second listener took a pipe that is in use")
	}
}

// The access list on the real pipe is the one documented: interactive users
// may read and write but not create instances (which would let them answer
// in the service's place).
func TestPipeSecurity(t *testing.T) {
	name := testPipeName(t)
	ln, err := NewPipe(name, nil).Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go echo(ln) // an instance to open waits once Accept runs
	var sd *windows.SECURITY_DESCRIPTOR
	for deadline := time.Now().Add(5 * time.Second); ; {
		sd, err = windows.GetNamedSecurityInfo(name, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if !errors.Is(err, windows.ERROR_PIPE_BUSY) || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	got := aceMasks(t, sd)
	iu, _ := windows.CreateWellKnownSid(windows.WinInteractiveSid)
	mask, ok := got[iu.String()]
	if !ok || mask != 0x120083 {
		t.Fatalf("Interactive Users mask %#x (present %v), want 0x120083; DACL %v", mask, ok, got)
	}
	if mask&windows.FILE_APPEND_DATA != 0 || mask&windows.GENERIC_WRITE != 0 {
		t.Fatal("Interactive Users may create pipe instances")
	}
	everyone, _ := windows.CreateWellKnownSid(windows.WinWorldSid)
	anon, _ := windows.CreateWellKnownSid(windows.WinAnonymousSid)
	network, _ := windows.CreateWellKnownSid(windows.WinNetworkSid)
	for _, s := range []*windows.SID{everyone, anon, network} {
		if _, ok := got[s.String()]; ok {
			t.Errorf("DACL grants %s access", s)
		}
	}
}

// aceMasks maps each allow entry's SID to its access mask.
func aceMasks(t *testing.T, sd *windows.SECURITY_DESCRIPTOR) map[string]uint32 {
	t.Helper()
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]uint32{}
	for i := range uint32(dacl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatal(err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		out[sid.String()] |= uint32(ace.Mask)
	}
	return out
}
