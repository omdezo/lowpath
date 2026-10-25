//go:build windows && amd64

package divert

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The tests load the DLL straight from the source tree; the helpers they use
// run in user mode and need neither admin nor the driver.
func loadForTest(t testing.TB) {
	t.Helper()
	if err := Load("bin"); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

// udpPacket is a minimal IPv4+UDP packet with zero checksums.
func udpPacket(src, dst [4]byte, sport, dport uint16, payload string) []byte {
	p := make([]byte, 28+len(payload))
	p[0] = 0x45
	binary.BigEndian.PutUint16(p[2:], uint16(len(p)))
	p[8] = 64
	p[9] = 17
	copy(p[12:], src[:])
	copy(p[16:], dst[:])
	binary.BigEndian.PutUint16(p[20:], sport)
	binary.BigEndian.PutUint16(p[22:], dport)
	binary.BigEndian.PutUint16(p[24:], uint16(8+len(payload)))
	copy(p[28:], payload)
	return p
}

func TestLoadTwice(t *testing.T) {
	loadForTest(t)
	abs, _ := filepath.Abs("bin")
	if err := Load(strings.ToUpper(abs)); err != nil {
		t.Fatalf("reload same dir: %v", err)
	}
	if err := Load(t.TempDir()); err == nil {
		t.Fatal("loading from a second directory should fail")
	}
}

func TestCompileFilter(t *testing.T) {
	loadForTest(t)
	if err := CompileFilter("outbound and ip and udp and ip.DstAddr < 224.0.0.0", LayerNetwork); err != nil {
		t.Fatalf("valid filter rejected: %v", err)
	}
	err := CompileFilter("outbound and udp.NoSuchField == 1", LayerNetwork)
	if err == nil || !strings.Contains(err.Error(), "position") {
		t.Fatalf("invalid filter: err = %v", err)
	}
	t.Log(err)
}

// EvalFilter reads direction, loopback and interface from the Address, so it
// checks this package's bit layout against the real DLL.
func TestAddressLayoutAgainstDLL(t *testing.T) {
	loadForTest(t)
	pkt := udpPacket([4]byte{192, 168, 1, 2}, [4]byte{8, 8, 8, 8}, 5000, 3478, "x")

	var a Address
	a.SetOutbound(true)
	a.SetNetwork(7, 3)
	for _, c := range []struct {
		filter string
		want   bool
	}{
		{"outbound", true},
		{"inbound", false},
		{"loopback", false},
		{"impostor", false},
		{"ifIdx == 7 and subIfIdx == 3", true},
		{"ifIdx == 3", false},
		{"ip and udp and udp.DstPort == 3478 and ip.DstAddr == 8.8.8.8", true},
	} {
		got, err := EvalFilter(c.filter, pkt, &a)
		if err != nil || got != c.want {
			t.Errorf("EvalFilter(%q) = %v, %v; want %v", c.filter, got, err, c.want)
		}
	}

	var b Address
	b.SetLoopback(true)
	b.SetImpostor(true)
	for _, f := range []string{"inbound", "loopback", "impostor"} {
		if got, err := EvalFilter(f, pkt, &b); err != nil || !got {
			t.Errorf("EvalFilter(%q) = %v, %v; want true", f, got, err)
		}
	}
}

func TestCalcChecksums(t *testing.T) {
	loadForTest(t)
	pkt := udpPacket([4]byte{10, 0, 0, 1}, [4]byte{1, 1, 1, 1}, 1234, 4321, "hello")
	var a Address
	if err := CalcChecksums(pkt, &a); err != nil {
		t.Fatal(err)
	}
	if binary.BigEndian.Uint16(pkt[10:]) == 0 || binary.BigEndian.Uint16(pkt[26:]) == 0 {
		t.Fatalf("checksums not filled: % x", pkt)
	}
	if !a.IPChecksum() || !a.UDPChecksum() {
		t.Fatalf("checksum flags not set at bits 21/23: %#x", a.bits)
	}
	if err := CalcChecksums([]byte{0x99, 1, 2}, nil); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestOpenNeedsAdmin(t *testing.T) {
	if IsAdmin() {
		t.Skip("elevated: Open would really load the driver")
	}
	loadForTest(t)
	h, err := Open("false", LayerNetwork, 0, FlagSniff|FlagRecvOnly)
	if err == nil {
		h.Close()
		t.Fatal("Open succeeded without admin")
	}
	if !errors.Is(err, ErrNeedAdmin) {
		t.Fatalf("Open without admin: %v, want ErrNeedAdmin", err)
	}
}

func TestInstall(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "driver")
	if err := Install(dir); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][]byte{dllName: dllFile, sysName: sysFile} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s: %v (len %d, want %d)", name, err, len(got), len(want))
		}
	}

	// Identical files are left alone; a damaged one is replaced.
	sys := filepath.Join(dir, sysName)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(sys, old, old)
	dll := filepath.Join(dir, dllName)
	os.WriteFile(dll, []byte("not a dll"), 0o644)
	if err := Install(dir); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(sys); !fi.ModTime().Equal(old) {
		t.Error("identical driver file was rewritten")
	}
	if got, _ := os.ReadFile(dll); !bytes.Equal(got, dllFile) {
		t.Error("damaged DLL was not replaced")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Errorf("install left %d files, want 2", len(entries))
	}
}

// Recv and Send can't run without the driver, but CalcChecksums makes the same
// kind of call (Go buffer, out-pointers): none of them may allocate.
func TestCallsDoNotAllocate(t *testing.T) {
	loadForTest(t)
	pkt := udpPacket([4]byte{10, 0, 0, 1}, [4]byte{1, 1, 1, 1}, 1234, 4321, "hello")
	var a Address
	if n := testing.AllocsPerRun(1000, func() { CalcChecksums(pkt, &a) }); n != 0 {
		t.Fatalf("CalcChecksums: %.1f allocations per call", n)
	}
}
