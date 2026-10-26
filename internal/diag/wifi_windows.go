package diag

import (
	"errors"
	"fmt"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// winWLAN reads the Wi-Fi link through the Native Wifi API (wlanapi.dll),
// which needs no administrator rights. Since Windows 11 24H2 the calls that
// can reveal where the PC is (the connected BSSID, scan results) need
// location access, so Link uses only the location-free opcodes:
// wlan_intf_opcode_realtime_connection_quality (RSSI, band, width, PHY and
// rates in one call) and wlan_intf_opcode_rssi, falling back to
// current_connection only on Windows versions without the realtime opcode,
// which have no location rule either. Survey reads the scan cache with
// WlanGetNetworkBssList and is only called when the user asks.
type winWLAN struct{ api *wlanAPI }

func newPlatformWLAN() WLAN { return &winWLAN{api: newWLANAPI()} }

// WLAN_INTF_OPCODE values.
const (
	opBackgroundScan    = 2
	opMediaStreaming    = 3
	opCurrentConnection = 7
	opChannelNumber     = 8
	opRealtimeQuality   = 19
	opRSSI              = 0x10000102
)

// Win32 errors the Wi-Fi service returns.
const (
	errAccessDenied       = 5
	errNotSupported       = 50
	errInvalidParameter   = 87
	errServiceNotActive   = 1062
	errNotFound           = 1168
	errInvalidState       = 5023
	errDot11PowerStateBad = 0x80342002 // ERROR_NDIS_DOT11_POWER_STATE_INVALID: the radio is off
)

// errOpUnsupported: this Windows or this driver doesn't know the opcode.
var errOpUnsupported = errors.New("diag: not supported by this Windows version or Wi-Fi driver")

// wlanAPI is wlanapi.dll, loaded on first use: Windows Server has it only
// with the Wireless LAN Service feature.
type wlanAPI struct {
	dll                                 *windows.LazyDLL
	open, closeHandle, enum, query, set *windows.LazyProc
	bssList, free                       *windows.LazyProc
	loadOnce                            sync.Once
	loadErr                             error
}

func newWLANAPI() *wlanAPI {
	d := windows.NewLazySystemDLL("wlanapi.dll")
	return &wlanAPI{dll: d,
		open: d.NewProc("WlanOpenHandle"), closeHandle: d.NewProc("WlanCloseHandle"), enum: d.NewProc("WlanEnumInterfaces"),
		query: d.NewProc("WlanQueryInterface"), set: d.NewProc("WlanSetInterface"),
		bssList: d.NewProc("WlanGetNetworkBssList"), free: d.NewProc("WlanFreeMemory")}
}

func (a *wlanAPI) load() error {
	a.loadOnce.Do(func() {
		if err := a.dll.Load(); err != nil {
			a.loadErr = fmt.Errorf("%w: Windows' Wi-Fi API (wlanapi.dll) isn't installed", ErrNoWiFi)
		}
	})
	return a.loadErr
}

// wlanErr names a Native Wifi error code.
func wlanErr(op string, code uintptr) error {
	switch code {
	case 0:
		return nil
	case errServiceNotActive:
		return fmt.Errorf("%w: the WLAN AutoConfig service isn't running", ErrNoWiFi)
	case errInvalidState:
		return ErrWiFiDisconnected
	case errDot11PowerStateBad:
		return fmt.Errorf("%w: the Wi-Fi radio is off", ErrWiFiDisconnected)
	case errNotSupported, errInvalidParameter, errNotFound:
		return fmt.Errorf("%s: %w", op, errOpUnsupported)
	case errAccessDenied:
		return fmt.Errorf("%s: %w", op, windows.ERROR_ACCESS_DENIED)
	}
	return fmt.Errorf("%s: %w", op, syscall.Errno(code))
}

// wlanClient is one open Native Wifi handle.
type wlanClient struct {
	api *wlanAPI
	h   windows.Handle
}

func (a *wlanAPI) client() (*wlanClient, error) {
	if err := a.load(); err != nil {
		return nil, err
	}
	var negotiated uint32
	var h windows.Handle
	r, _, _ := a.open.Call(2, 0, uintptr(unsafe.Pointer(&negotiated)), uintptr(unsafe.Pointer(&h)))
	if err := wlanErr("WlanOpenHandle", r); err != nil {
		return nil, err
	}
	return &wlanClient{api: a, h: h}, nil
}

func (c *wlanClient) close() { c.api.closeHandle.Call(uintptr(c.h), 0) }

// take copies size bytes the API allocated at p and frees them.
func (c *wlanClient) take(p *byte, size int) []byte {
	if p == nil {
		return nil
	}
	b := append([]byte(nil), unsafe.Slice(p, size)...)
	c.api.free.Call(uintptr(unsafe.Pointer(p)))
	return b
}

func (c *wlanClient) interfaces() ([]wlanInterface, error) {
	var p *byte
	r, _, _ := c.api.enum.Call(uintptr(c.h), 0, uintptr(unsafe.Pointer(&p)))
	if err := wlanErr("WlanEnumInterfaces", r); err != nil {
		return nil, err
	}
	if p == nil {
		return nil, nil
	}
	n := int(*(*uint32)(unsafe.Pointer(p)))
	if n < 0 || n > 64 {
		c.take(p, 0)
		return nil, errShortWLAN
	}
	return decodeInterfaceList(c.take(p, 8+n*sizeInterfaceInfo))
}

// queryRaw runs WlanQueryInterface and returns a copy of the reply.
func (c *wlanClient) queryRaw(guid *[16]byte, op uint32) ([]byte, error) {
	var size uint32
	var p *byte
	r, _, _ := c.api.query.Call(uintptr(c.h), uintptr(unsafe.Pointer(guid)), uintptr(op), 0,
		uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&p)), 0)
	if err := wlanErr("WlanQueryInterface", r); err != nil {
		return nil, err
	}
	return c.take(p, int(size)), nil
}

func (c *wlanClient) queryU32(guid *[16]byte, op uint32) (uint32, error) {
	b, err := c.queryRaw(guid, op)
	if err != nil {
		return 0, err
	}
	if len(b) < 4 {
		return 0, errShortWLAN
	}
	return le32(b, 0), nil
}

func (c *wlanClient) setBool(guid *[16]byte, op uint32, v bool) error {
	var x uint32
	if v {
		x = 1
	}
	r, _, _ := c.api.set.Call(uintptr(c.h), uintptr(unsafe.Pointer(guid)), uintptr(op), 4, uintptr(unsafe.Pointer(&x)), 0)
	return wlanErr("WlanSetInterface", r)
}

func (c *wlanClient) bss(guid *[16]byte) ([]byte, error) {
	var p *byte
	// No SSID filter, any BSS type (dot11_BSS_type_any = 3): every
	// network in the adapter's scan cache. This doesn't start a scan.
	r, _, _ := c.api.bssList.Call(uintptr(c.h), uintptr(unsafe.Pointer(guid)), 0, 3, 0, 0, uintptr(unsafe.Pointer(&p)))
	if err := wlanErr("WlanGetNetworkBssList", r); err != nil {
		return nil, err
	}
	if p == nil {
		return nil, errShortWLAN
	}
	total := int(*(*uint32)(unsafe.Pointer(p)))
	if total < 8 || total > 8<<20 {
		c.take(p, 0)
		return nil, errShortWLAN
	}
	return c.take(p, total), nil
}

// pick finds the Wi-Fi adapter behind interface index (0: the first
// connected one) and its name.
func (c *wlanClient) pick(index int) (wlanInterface, string, error) {
	list, err := c.interfaces()
	if err != nil {
		return wlanInterface{}, "", err
	}
	if len(list) == 0 {
		return wlanInterface{}, "", fmt.Errorf("%w: Windows lists no Wi-Fi adapter", ErrNoWiFi)
	}
	if index > 0 {
		row := windows.MibIfRow2{InterfaceIndex: uint32(index)}
		if err := windows.GetIfEntry2Ex(windows.MibIfEntryNormalWithoutStatistics, &row); err != nil {
			return wlanInterface{}, "", fmt.Errorf("diag: GetIfEntry2Ex(%d): %w", index, err)
		}
		g := *(*[16]byte)(unsafe.Pointer(&row.InterfaceGuid))
		for _, it := range list {
			if it.GUID == g {
				return it, windows.UTF16ToString(row.Alias[:]), connected(it)
			}
		}
		return wlanInterface{}, "", fmt.Errorf("%w: the connection in use isn't a Wi-Fi adapter", ErrNoWiFi)
	}
	for _, it := range list {
		if it.State == wlanInterfaceStateConnected {
			return it, "", nil
		}
	}
	return list[0], "", connected(list[0])
}

func connected(it wlanInterface) error {
	if it.State != wlanInterfaceStateConnected {
		return ErrWiFiDisconnected
	}
	return nil
}

func (w *winWLAN) Link(index int) (WiFiLink, error) {
	c, err := w.api.client()
	if err != nil {
		return WiFiLink{}, err
	}
	defer c.close()
	it, name, err := c.pick(index)
	l := WiFiLink{At: time.Now(), Interface: name, Adapter: it.Description}
	if err != nil {
		return l, err
	}
	g := it.GUID
	rt, rtErr := c.queryRaw(&g, opRealtimeQuality)
	if rtErr == nil {
		rtErr = decodeRealtimeQuality(rt, &l)
	}
	switch {
	case rtErr == nil:
		l.Source = "WlanQueryInterface(realtime_connection_quality)"
	case errors.Is(rtErr, ErrWiFiDisconnected):
		return l, rtErr
	default:
		// Before Windows 11 24H2: current_connection, which has no
		// location rule on those versions; channel_number for the channel.
		l.Source = "WlanQueryInterface(current_connection)"
		if b, err := c.queryRaw(&g, opCurrentConnection); err == nil {
			if a, err := decodeConnAttributes(b); err == nil {
				if a.Quality <= 100 {
					q := int(a.Quality)
					l.Quality = &q
				}
				l.RxMbps, l.TxMbps = rateMbps(a.RxRate), rateMbps(a.TxRate)
				w.channelFallback(c, &g, &l, a.PHY)
			}
		} else if errors.Is(err, ErrWiFiDisconnected) {
			return l, err
		} else if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			l.note("Windows shares the PHY rate only with apps that have location access.")
			w.channelFallback(c, &g, &l, 0)
		} else {
			l.note("Windows didn't report the connection's details (%v).", err)
		}
	}
	if l.RSSI == nil {
		if v, err := c.queryU32(&g, opRSSI); err == nil {
			l.RSSI = validRSSI(int32(v))
		}
	}
	if l.RSSI == nil {
		l.note("The Wi-Fi driver doesn't report the signal strength in dBm.")
	}
	return l, nil
}

// channelFallback reads wlan_intf_opcode_channel_number. Channels 1 to 13
// exist on 2.4 and 6 GHz alike, so a low channel counts as 2.4 GHz only
// when the PHY can't be a 6 GHz one (anything before 802.11ax).
func (w *winWLAN) channelFallback(c *wlanClient, g *[16]byte, l *WiFiLink, phy uint32) {
	ch, err := c.queryU32(g, opChannelNumber)
	if err != nil || ch == 0 || ch > 233 {
		return
	}
	v := int(ch)
	l.Channel = &v
	switch {
	case ch <= 14 && phy != 0 && phy < 10:
		l.Band = "2.4"
	case ch >= 32 && ch <= 177:
		l.Band = "5"
	default:
		l.note("Windows gave channel %d without its band.", ch)
	}
	l.Standard, l.Generation = phyStandard(phy, l.Band)
}

func (w *winWLAN) Survey(index int) (WiFiSurvey, error) {
	c, err := w.api.client()
	if err != nil {
		return WiFiSurvey{}, err
	}
	defer c.close()
	it, _, err := c.pick(index)
	if err != nil {
		return WiFiSurvey{}, err
	}
	g := it.GUID
	var own [6]byte
	b, err := c.queryRaw(&g, opCurrentConnection)
	switch {
	case errors.Is(err, windows.ERROR_ACCESS_DENIED):
		return WiFiSurvey{}, ErrLocationDenied
	case err != nil:
		return WiFiSurvey{}, err
	}
	if a, err := decodeConnAttributes(b); err == nil {
		own = a.BSSID
	}
	raw, err := c.bss(&g)
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return WiFiSurvey{}, ErrLocationDenied
	}
	if err != nil {
		return WiFiSurvey{}, err
	}
	entries, err := decodeBSSList(raw)
	if err != nil {
		return WiFiSurvey{}, err
	}
	freq := 0
	for _, e := range entries {
		if e.BSSID == own {
			freq = e.FreqMHz
		}
	}
	if freq == 0 {
		var l WiFiLink
		if rt, err := c.queryRaw(&g, opRealtimeQuality); err == nil && decodeRealtimeQuality(rt, &l) == nil && l.FreqMHz != nil {
			freq = *l.FreqMHz
		}
	}
	return NewWiFiSurvey(time.Now(), own, freq, entries), nil
}

func (w *winWLAN) Tune(index int) (WiFiTuning, error) {
	c, err := w.api.client()
	if err != nil {
		return nil, err
	}
	it, _, err := c.pick(index)
	if err != nil {
		c.close()
		return nil, err
	}
	t := &winTuning{c: c, guid: it.GUID}
	st := t.Refresh()
	if !st.ScanPaused && !st.Streaming {
		c.close()
		return nil, t.firstErr
	}
	return t, nil
}

// winTuning holds this client's two votes: background scan off, media
// streaming on. The Wi-Fi service ORs the votes of every client (scans
// stay off while any client wants them off) and drops a client's votes
// when its handle closes, which also happens when the process dies. A
// disconnect resets both, so Refresh applies them again.
type winTuning struct {
	mu       sync.Mutex
	c        *wlanClient
	guid     [16]byte
	state    WiFiTuningState
	firstErr error
	closed   bool
}

func (t *winTuning) State() WiFiTuningState {
	t.mu.Lock()
	defer t.mu.Unlock()
	return cloneTuning(t.state)
}

func cloneTuning(s WiFiTuningState) WiFiTuningState {
	s.Notes = append([]string(nil), s.Notes...)
	return s
}

func (t *winTuning) Refresh() WiFiTuningState {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return cloneTuning(t.state)
	}
	// Set every time rather than query first: a query sees every
	// client's votes, so "off" may be another program's vote, which can
	// go away while this boost still needs it.
	var st WiFiTuningState
	if err := t.c.setBool(&t.guid, opBackgroundScan, false); err != nil {
		t.keep(err)
		st.Notes = append(st.Notes, "Background scans couldn't be paused: "+tuneReason(err))
	} else {
		st.ScanPaused = true
	}
	if err := t.c.setBool(&t.guid, opMediaStreaming, true); err != nil {
		t.keep(err)
		st.Notes = append(st.Notes, "Streaming mode couldn't be turned on: "+tuneReason(err))
	} else {
		st.Streaming = true
	}
	t.state = st
	return cloneTuning(st)
}

func (t *winTuning) keep(err error) {
	if t.firstErr == nil {
		t.firstErr = err
	}
}

// tuneReason words a WlanSetInterface failure for the UI.
func tuneReason(err error) string {
	switch {
	case errors.Is(err, ErrWiFiDisconnected):
		return "the Wi-Fi isn't connected."
	case errors.Is(err, errOpUnsupported):
		return "the Wi-Fi driver doesn't support it."
	case errors.Is(err, windows.ERROR_ACCESS_DENIED):
		return "Windows refused (a policy may lock this setting)."
	}
	return err.Error() + "."
}

func (t *winTuning) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	t.closed = true
	// Withdraw the votes before closing, so nothing depends on the service
	// noticing the closed handle. Errors (the Wi-Fi went away meanwhile)
	// don't matter: a disconnect resets both settings anyway.
	e1 := t.c.setBool(&t.guid, opBackgroundScan, true)
	e2 := t.c.setBool(&t.guid, opMediaStreaming, false)
	t.c.close()
	t.state = WiFiTuningState{}
	if e1 != nil && !errors.Is(e1, ErrWiFiDisconnected) {
		return e1
	}
	if e2 != nil && !errors.Is(e2, ErrWiFiDisconnected) && !errors.Is(e2, errOpUnsupported) {
		return e2
	}
	return nil
}
