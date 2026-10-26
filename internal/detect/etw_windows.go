package detect

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// KernelNetwork watches every process's network flows through the ETW
// provider Microsoft-Windows-Kernel-Network in a real-time session. It needs
// no packet driver and never touches the traffic, so it is safe next to
// anti-cheats that refuse WinDivert, and it can't break networking: the
// worst it can do is miss events (ETW drops them when its buffers are full).
//
// Starting the session needs administrator rights or membership in the
// Performance Log Users group; without them Watch returns ErrNeedAdmin.
// One session at a time: Watch fails while another watch is open.
type KernelNetwork struct {
	name     string       // the ETW session name
	provider windows.GUID // Microsoft-Windows-Kernel-Network

	startTrace, controlTrace, enableTraceEx2 *windows.LazyProc
	openTrace, processTrace, closeTrace      *windows.LazyProc

	// The ETW callback is a C function pointer, and Go can make only a
	// limited number of them per process, never freed: one per watcher,
	// made on first use, dispatching to whichever watch is open.
	cbOnce   sync.Once
	callback uintptr
	mu       sync.Mutex // serializes Watch
	current  atomic.Pointer[etwWatch]
}

// NewKernelNetwork returns the Windows flow watcher.
func NewKernelNetwork() *KernelNetwork {
	adv := windows.NewLazySystemDLL("advapi32.dll")
	return &KernelNetwork{
		name:           "Lowpath Flows",
		provider:       windows.GUID{Data1: 0x7dd42a49, Data2: 0x5329, Data3: 0x4832, Data4: [8]byte{0x8d, 0xfd, 0x43, 0xd9, 0x79, 0x15, 0x3a, 0x88}},
		startTrace:     adv.NewProc("StartTraceW"),
		controlTrace:   adv.NewProc("ControlTraceW"),
		enableTraceEx2: adv.NewProc("EnableTraceEx2"),
		openTrace:      adv.NewProc("OpenTraceW"),
		processTrace:   adv.NewProc("ProcessTrace"),
		closeTrace:     adv.NewProc("CloseTrace"),
	}
}

const (
	wnodeFlagTracedGUID          = 0x00020000
	eventTraceRealTimeMode       = 0x00000100
	processTraceModeRealTime     = 0x00000100
	processTraceModeEventRecord  = 0x10000000
	eventControlCodeEnable       = 1
	eventTraceControlStop        = 1
	traceLevelInformation        = 4
	keywordIPv4IPv6              = 0x10 | 0x20 // KERNEL_NETWORK_KEYWORD_IPV4 | _IPV6
	errorAlreadyExists           = windows.Errno(183)
	invalidProcessTraceHandle    = ^uint64(0)
	etwClientContextSystemTime   = 2
	etwBufferKB                  = 64
	etwFlushSeconds              = 1
	etwMinBuffers, etwMaxBuffers = 4, 32
)

// wnodeHeader is WNODE_HEADER.
type wnodeHeader struct {
	BufferSize        uint32
	ProviderID        uint32
	HistoricalContext uint64
	TimeStamp         int64
	GUID              windows.GUID
	ClientContext     uint32
	Flags             uint32
}

// eventTraceProperties is EVENT_TRACE_PROPERTIES; the session name follows
// it in the same allocation (LoggerNameOffset).
type eventTraceProperties struct {
	Wnode               wnodeHeader
	BufferSize          uint32
	MinimumBuffers      uint32
	MaximumBuffers      uint32
	MaximumFileSize     uint32
	LogFileMode         uint32
	FlushTimer          uint32
	EnableFlags         uint32
	AgeLimit            int32
	NumberOfBuffers     uint32
	FreeBuffers         uint32
	EventsLost          uint32
	BuffersWritten      uint32
	LogBuffersLost      uint32
	RealTimeBuffersLost uint32
	LoggerThreadID      uintptr
	LogFileNameOffset   uint32
	LoggerNameOffset    uint32
}

// eventTraceHeader is EVENT_TRACE_HEADER.
type eventTraceHeader struct {
	Size           uint16
	FieldTypeFlags uint16
	Version        uint32
	ThreadID       uint32
	ProcessID      uint32
	TimeStamp      int64
	GUID           windows.GUID
	ProcessorTime  uint64
}

// eventTrace is EVENT_TRACE.
type eventTrace struct {
	Header           eventTraceHeader
	InstanceID       uint32
	ParentInstanceID uint32
	ParentGUID       windows.GUID
	MofData          uintptr
	MofLength        uint32
	ClientContext    uint32
}

// timeZoneInformation is TIME_ZONE_INFORMATION.
type timeZoneInformation struct {
	Bias         int32
	StandardName [32]uint16
	StandardDate [8]uint16
	StandardBias int32
	DaylightName [32]uint16
	DaylightDate [8]uint16
	DaylightBias int32
}

// traceLogfileHeader is TRACE_LOGFILE_HEADER.
type traceLogfileHeader struct {
	BufferSize         uint32
	Version            uint32
	ProviderVersion    uint32
	NumberOfProcessors uint32
	EndTime            int64
	TimerResolution    uint32
	MaximumFileSize    uint32
	LogFileMode        uint32
	BuffersWritten     uint32
	LogInstanceGUID    windows.GUID
	LoggerName         *uint16
	LogFileName        *uint16
	TimeZone           timeZoneInformation
	BootTime           int64
	PerfFreq           int64
	StartTime          int64
	ReservedFlags      uint32
	BuffersLost        uint32
}

// eventTraceLogfile is EVENT_TRACE_LOGFILEW.
type eventTraceLogfile struct {
	LogFileName         *uint16
	LoggerName          *uint16
	CurrentTime         int64
	BuffersRead         uint32
	ProcessTraceMode    uint32
	CurrentEvent        eventTrace
	LogfileHeader       traceLogfileHeader
	BufferCallback      uintptr
	BufferSize          uint32
	Filled              uint32
	EventsLost          uint32
	EventRecordCallback uintptr
	IsKernelTrace       uint32
	Context             uintptr
}

// eventRecord is EVENT_RECORD (EVENT_HEADER with the descriptor inlined).
type eventRecord struct {
	Size              uint16
	HeaderType        uint16
	Flags             uint16
	EventProperty     uint16
	ThreadID          uint32
	ProcessID         uint32
	TimeStamp         int64
	ProviderID        windows.GUID
	ID                uint16
	Version           uint8
	Channel           uint8
	Level             uint8
	Opcode            uint8
	Task              uint16
	Keyword           uint64
	ProcessorTime     uint64
	ActivityID        windows.GUID
	BufferContext     uint32
	ExtendedDataCount uint16
	UserDataLength    uint16
	ExtendedData      uintptr
	UserData          *byte
	UserContext       uintptr
}

// properties builds EVENT_TRACE_PROPERTIES with the session name after it.
func (k *KernelNetwork) properties() (*eventTraceProperties, error) {
	name, err := windows.UTF16FromString(k.name)
	if err != nil {
		return nil, err
	}
	size := unsafe.Sizeof(eventTraceProperties{})
	// Room for the name and for a log file name ETW may write back.
	buf := make([]byte, size+uintptr(len(name))*2+1024)
	p := (*eventTraceProperties)(unsafe.Pointer(&buf[0]))
	p.Wnode.BufferSize = uint32(len(buf))
	p.Wnode.Flags = wnodeFlagTracedGUID
	p.Wnode.ClientContext = etwClientContextSystemTime
	p.BufferSize = etwBufferKB
	p.MinimumBuffers, p.MaximumBuffers = etwMinBuffers, etwMaxBuffers
	p.LogFileMode = eventTraceRealTimeMode
	p.FlushTimer = etwFlushSeconds
	p.LoggerNameOffset = uint32(size)
	copy(unsafe.Slice((*uint16)(unsafe.Pointer(&buf[size])), len(name)), name)
	return p, nil
}

// Watch starts the ETW session and calls onEvent for every TCP and UDP flow
// event of every process, on the session's own thread, until Close.
// onEvent must be quick: ETW drops events while it runs.
func (k *KernelNetwork) Watch(onEvent func(FlowEvent)) (FlowWatch, error) {
	if err := k.startTrace.Find(); err != nil {
		return nil, fmt.Errorf("ETW: %w", err)
	}
	k.cbOnce.Do(func() { k.callback = windows.NewCallback(k.onRecord) })
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.current.Load() != nil {
		return nil, errors.New("ETW: a flow watch is already running")
	}
	name, _ := windows.UTF16PtrFromString(k.name)

	var session uint64
	start := func() error {
		props, err := k.properties()
		if err != nil {
			return err
		}
		r, _, _ := k.startTrace.Call(uintptr(unsafe.Pointer(&session)), uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(props)))
		if r != 0 {
			return windows.Errno(r)
		}
		return nil
	}
	err := start()
	if errors.Is(err, errorAlreadyExists) {
		// Left over from a Lowpath that didn't exit cleanly: a real-time
		// session outlives its process. Stop it and start ours.
		k.stop(0)
		err = start()
	}
	switch {
	case errors.Is(err, windows.ERROR_ACCESS_DENIED):
		return nil, ErrNeedAdmin
	case err != nil:
		return nil, fmt.Errorf("ETW: start session: %w", err)
	}

	r, _, _ := k.enableTraceEx2.Call(uintptr(session), uintptr(unsafe.Pointer(&k.provider)),
		eventControlCodeEnable, traceLevelInformation, keywordIPv4IPv6, 0, 0, 0)
	if r != 0 {
		k.stop(session)
		if windows.Errno(r) == windows.ERROR_ACCESS_DENIED {
			return nil, ErrNeedAdmin
		}
		return nil, fmt.Errorf("ETW: enable Microsoft-Windows-Kernel-Network: %w", windows.Errno(r))
	}

	lf := &eventTraceLogfile{
		LoggerName:          name,
		ProcessTraceMode:    processTraceModeRealTime | processTraceModeEventRecord,
		EventRecordCallback: k.callback,
	}
	h, _, callErr := k.openTrace.Call(uintptr(unsafe.Pointer(lf)))
	if uint64(h) == invalidProcessTraceHandle {
		k.stop(session)
		if errors.Is(callErr, windows.ERROR_ACCESS_DENIED) {
			return nil, ErrNeedAdmin
		}
		return nil, fmt.Errorf("ETW: open session: %w", callErr)
	}

	w := &etwWatch{k: k, session: session, consumer: uint64(h), onEvent: onEvent, done: make(chan struct{})}
	k.current.Store(w)
	go func() {
		defer close(w.done)
		handle := w.consumer
		// Blocks, delivering events on this thread, until the session stops.
		k.processTrace.Call(uintptr(unsafe.Pointer(&handle)), 1, 0, 0)
	}()
	return w, nil
}

// stop ends the session (by handle, or by name when session is 0).
func (k *KernelNetwork) stop(session uint64) {
	props, err := k.properties()
	if err != nil {
		return
	}
	var name *uint16
	if session == 0 {
		name, _ = windows.UTF16PtrFromString(k.name)
	}
	k.controlTrace.Call(uintptr(session), uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(props)), eventTraceControlStop)
}

// onRecord is the ETW event callback: decode and hand over, nothing more.
func (k *KernelNetwork) onRecord(rec *eventRecord) uintptr {
	if rec == nil || rec.ProviderID != k.provider || rec.UserData == nil {
		return 0
	}
	w := k.current.Load()
	if w == nil {
		return 0
	}
	data := unsafe.Slice(rec.UserData, rec.UserDataLength)
	if ev, ok := parseKernelNetwork(rec.ID, data); ok {
		w.onEvent(ev)
	}
	return 0
}

type etwWatch struct {
	k                 *KernelNetwork
	session, consumer uint64
	onEvent           func(FlowEvent)
	done              chan struct{}
	once              sync.Once
}

func (w *etwWatch) Close() error {
	w.once.Do(func() {
		w.k.stop(w.session)
		w.k.closeTrace.Call(uintptr(w.consumer))
		select {
		case <-w.done:
		case <-time.After(5 * time.Second):
			// ProcessTrace should return within a flush period; if it
			// doesn't, events still stop below.
		}
		w.k.current.CompareAndSwap(w, nil)
	})
	return nil
}
