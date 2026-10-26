package detect

import (
	"errors"
	"net"
	"net/netip"
	"os"
	"runtime"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The structs passed to ETW must match the Windows SDK layouts byte for
// byte (sizes from evntrace.h and evntcons.h on 64-bit Windows).
func TestETWStructLayouts(t *testing.T) {
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skip("layouts checked on 64-bit Windows")
	}
	for _, c := range []struct {
		name      string
		got, want uintptr
	}{
		{"WNODE_HEADER", unsafe.Sizeof(wnodeHeader{}), 48},
		{"EVENT_TRACE_PROPERTIES", unsafe.Sizeof(eventTraceProperties{}), 120},
		{"EVENT_TRACE_HEADER", unsafe.Sizeof(eventTraceHeader{}), 48},
		{"EVENT_TRACE", unsafe.Sizeof(eventTrace{}), 88},
		{"TIME_ZONE_INFORMATION", unsafe.Sizeof(timeZoneInformation{}), 172},
		{"TRACE_LOGFILE_HEADER", unsafe.Sizeof(traceLogfileHeader{}), 280},
		{"EVENT_TRACE_LOGFILEW", unsafe.Sizeof(eventTraceLogfile{}), 448},
		{"EVENT_RECORD", unsafe.Sizeof(eventRecord{}), 112},
		{"EVENT_RECORD.UserData", unsafe.Offsetof(eventRecord{}.UserData), 96},
		{"EVENT_RECORD.Id", unsafe.Offsetof(eventRecord{}.ID), 40},
		{"EVENT_TRACE_LOGFILEW.EventRecordCallback", unsafe.Offsetof(eventTraceLogfile{}.EventRecordCallback), 424},
	} {
		if c.got != c.want {
			t.Errorf("%s: %d bytes, want %d", c.name, c.got, c.want)
		}
	}
}

// Without administrator rights the session can't start, and Watch must say
// so plainly. As administrator, the watch must see this test's own UDP
// packet to a documentation address, which checks the field order of the
// live provider (the "lab check" for the manifest's daddr/dport names).
func TestKernelNetworkWatch(t *testing.T) {
	k := NewKernelNetwork()
	k.name = "Lowpath Flows Test"
	events := make(chan FlowEvent, 4096)
	w, err := k.Watch(func(ev FlowEvent) {
		select {
		case events <- ev:
		default:
		}
	})
	if !windows.GetCurrentProcessToken().IsElevated() {
		if !errors.Is(err, ErrNeedAdmin) {
			t.Fatalf("unelevated Watch: %v, want ErrNeedAdmin", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := k.Watch(func(FlowEvent) {}); err == nil {
		t.Fatal("a second watch must be refused")
	}

	dst := netip.MustParseAddrPort("192.0.2.1:9") // TEST-NET-1, discard port
	c, err := net.DialUDP("udp4", nil, net.UDPAddrFromAddrPort(dst))
	if err != nil {
		t.Skip("no IPv4 route:", err)
	}
	defer c.Close()
	pid := uint32(os.Getpid())
	deadline := time.After(10 * time.Second)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case ev := <-events:
			if ev.PID == pid && ev.Proto == "udp" && ev.Outbound && ev.Remote == dst {
				if ev.Local.Port() != uint16(c.LocalAddr().(*net.UDPAddr).Port) {
					t.Errorf("local port %d, want %d", ev.Local.Port(), c.LocalAddr().(*net.UDPAddr).Port)
				}
				return
			}
		case <-tick.C:
			c.Write([]byte("lowpath etw test"))
		case <-deadline:
			t.Fatal("no Kernel-Network event for our own UDP packet within 10 s")
		}
	}
}

// Reset stops a session left behind by name, once, and never the session
// of an open watch. Unelevated, the session can't be controlled: Reset says
// ErrNeedAdmin, or nothing when Windows reports no such session first.
func TestKernelNetworkReset(t *testing.T) {
	k := NewKernelNetwork()
	k.name = "Lowpath Flows Reset Test"
	if !windows.GetCurrentProcessToken().IsElevated() {
		stopped, err := k.Reset()
		if stopped || (err != nil && !errors.Is(err, ErrNeedAdmin)) {
			t.Fatalf("unelevated Reset = %v, %v; want false and nil or ErrNeedAdmin", stopped, err)
		}
		t.Logf("unelevated Reset: %v", err)
		return
	}

	// No session: nothing to stop.
	if stopped, err := k.Reset(); stopped || err != nil {
		t.Fatalf("Reset without a session = %v, %v", stopped, err)
	}

	// A session left behind: started and never stopped, like a crashed
	// Lowpath's.
	props, err := k.properties()
	if err != nil {
		t.Fatal(err)
	}
	name, _ := windows.UTF16PtrFromString(k.name)
	var session uint64
	if r, _, _ := k.startTrace.Call(uintptr(unsafe.Pointer(&session)), uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(props))); r != 0 {
		t.Fatalf("start a leftover session: %v", windows.Errno(r))
	}
	defer k.stop(0)
	if stopped, err := k.Reset(); !stopped || err != nil {
		t.Fatalf("Reset with a leftover = %v, %v; want true", stopped, err)
	}
	if stopped, err := k.Reset(); stopped || err != nil {
		t.Fatalf("second Reset = %v, %v; want false", stopped, err)
	}

	// Our own open watch stays open.
	w, err := k.Watch(func(FlowEvent) {})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if stopped, err := k.Reset(); stopped || err != nil {
		t.Fatalf("Reset during a watch = %v, %v; want false", stopped, err)
	}
	select {
	case <-w.Done():
		t.Fatal("Reset stopped the open watch")
	case <-time.After(2 * etwFlushSeconds * time.Second):
	}
}
