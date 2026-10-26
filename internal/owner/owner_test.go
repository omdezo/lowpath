package owner

import (
	"errors"
	"net/netip"
	"reflect"
	"testing"
)

type fakeResolver struct {
	socks    []Socket
	names    map[uint32]string
	sockErr  error
	namesErr error
}

func (f fakeResolver) UDPSockets() ([]Socket, error)            { return f.socks, f.sockErr }
func (f fakeResolver) ProcessNames() (map[uint32]string, error) { return f.names, f.namesErr }

func TestListProcesses(t *testing.T) {
	ap := netip.MustParseAddrPort
	r := fakeResolver{
		socks: []Socket{
			{ap("0.0.0.0:500"), 4},             // System: left out
			{ap("127.0.0.1:7000"), 10},         //
			{ap("[::]:7001"), 10},              // IPv6 socket of the same game
			{ap("0.0.0.0:7000"), 10},           // same port twice: listed once
			{ap("192.168.1.2:6000"), 11},       //
			{ap("0.0.0.0:6001"), 99},           // self
			{ap("0.0.0.0:6002"), 12},           // exited before the process list was read
			{ap("[fe80::1%eth0]:6003"), 13},    // link-local with a zone
			{ap("0.0.0.0:6004"), 13},           //
			{ap("[::ffff:10.0.0.1]:6005"), 13}, // v4-mapped
		},
		names: map[uint32]string{4: "System", 10: "cs2.exe", 11: "Discord.exe", 13: "apex", 99: "lowpath.exe"},
	}
	got, err := ListProcesses(r, 99)
	if err != nil {
		t.Fatal(err)
	}
	want := []Process{
		{PID: 13, Name: "apex", UDPPorts: []uint16{6003, 6004, 6005}},
		{PID: 10, Name: "cs2.exe", UDPPorts: []uint16{7000, 7001}},
		{PID: 11, Name: "Discord.exe", UDPPorts: []uint16{6000}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListProcesses =\n%+v\nwant\n%+v", got, want)
	}
}

func TestListProcessesEmptyAndErrors(t *testing.T) {
	got, err := ListProcesses(fakeResolver{}, 1)
	if err != nil || got == nil || len(got) != 0 {
		t.Errorf("no sockets: %v, %v; want an empty, non-nil list", got, err)
	}
	boom := errors.New("boom")
	if _, err := ListProcesses(fakeResolver{sockErr: boom}, 1); !errors.Is(err, boom) {
		t.Errorf("socket table error lost: %v", err)
	}
	r := fakeResolver{socks: []Socket{{netip.MustParseAddrPort("0.0.0.0:1"), 10}}, namesErr: boom}
	if _, err := ListProcesses(r, 1); !errors.Is(err, boom) {
		t.Errorf("process list error lost: %v", err)
	}
}

// Whatever OS this runs on, New works or says it is unsupported; it never
// reports an empty table as if nothing were open.
func TestNewEitherWorksOrIsUnsupported(t *testing.T) {
	r := New()
	if _, err := r.UDPSockets(); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("UDPSockets: %v", err)
	}
	if _, err := r.ProcessNames(); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("ProcessNames: %v", err)
	}
}
