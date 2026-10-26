package platform

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"runtime"
	"sync"
	"unsafe"

	webview2 "github.com/jchv/go-webview2"
	"github.com/jchv/go-webview2/webviewloader"
	"golang.org/x/sys/windows"
)

const (
	windowTitle = "Lowpath"
	windowClass = "webview" // the class go-webview2 registers
	// Sizes in 96-DPI pixels; the manifest makes the process DPI aware, so
	// they are scaled to the display here.
	winWidth, winHeight = 1100, 720
	minWidth, minHeight = 920, 600
	// iconID is the RT_GROUP_ICON resource "#1" from cmd/lowpath/winres.
	iconID = 1
)

var (
	user32                  = windows.NewLazySystemDLL("user32.dll")
	procGetDpiForSystem     = user32.NewProc("GetDpiForSystem")
	procGetSystemMetrics    = user32.NewProc("GetSystemMetrics")
	procFindWindowW         = user32.NewProc("FindWindowW")
	procShowWindow          = user32.NewProc("ShowWindow")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
)

// WebView2 is the Windows desktop: the UI in a WebView2 window of this
// process, the default browser through the shell, errors in a message box.
type WebView2 struct {
	profile string
	log     *slog.Logger

	mu   sync.Mutex
	open bool
}

// NewDesktop returns the Windows desktop shell.
func NewDesktop(cfg DesktopConfig) Desktop {
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &WebView2{profile: cfg.ProfileDir, log: log}
}

// RunWindow shows url in a WebView2 window on this OS thread. It fails at
// once when the WebView2 runtime isn't installed.
func (d *WebView2) RunWindow(ctx context.Context, url string) error {
	if v, err := webviewloader.GetInstalledVersion(); err != nil || v == "" {
		return fmt.Errorf("%w: the WebView2 runtime is not installed (%v)", ErrNoWindow, err)
	}
	// The window and its message loop must stay on one OS thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	scale := dpiScale()
	w, h := fitScreen(int(winWidth*scale), int(winHeight*scale))
	wv := webview2.NewWithOptions(webview2.WebViewOptions{
		DataPath:  d.profile,
		AutoFocus: true,
		WindowOptions: webview2.WindowOptions{
			Title: windowTitle, Width: uint(w), Height: uint(h), IconId: iconID, Center: true,
		},
	})
	if wv == nil {
		return fmt.Errorf("%w: could not create the WebView2 window", ErrNoWindow)
	}
	wv.SetSize(min(int(minWidth*scale), w), min(int(minHeight*scale), h), webview2.HintMin)
	wv.Navigate(url)
	d.log.Info("window open", "kind", "webview2", "size", fmt.Sprintf("%dx%d", w, h), "dpi_scale", scale)
	d.setOpen(true)
	defer d.setOpen(false)

	closed := make(chan struct{})
	defer close(closed)
	go func() {
		select {
		case <-ctx.Done():
			// Posts WM_CLOSE; Run returns once the window is destroyed.
			wv.Destroy()
		case <-closed:
		}
	}()
	wv.Run()
	return nil
}

func (d *WebView2) setOpen(v bool) { d.mu.Lock(); d.open = v; d.mu.Unlock() }

// ShowWindow raises the window; the WebView2 window keeps its session, so
// url isn't needed.
func (d *WebView2) ShowWindow(string) error {
	d.mu.Lock()
	open := d.open
	d.mu.Unlock()
	if !open || !raiseWindow() {
		return ErrNoWindow
	}
	return nil
}

// AddToAppMenu does nothing: a Start menu entry is the installer's job.
func (*WebView2) AddToAppMenu(string) error { return nil }

func (*WebView2) OpenBrowser(url string) error {
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}

func (*WebView2) ShowError(title, msg string) {
	t, _ := windows.UTF16PtrFromString(title)
	m, _ := windows.UTF16PtrFromString(msg)
	windows.MessageBox(0, m, t, windows.MB_OK|windows.MB_ICONERROR)
}

// dpiScale is the system DPI relative to 96 (1.5 at 150 %).
func dpiScale() float64 {
	if procGetDpiForSystem.Find() != nil {
		return 1
	}
	dpi, _, _ := procGetDpiForSystem.Call()
	if dpi == 0 {
		return 1
	}
	return float64(dpi) / 96
}

// fitScreen keeps the window inside the primary screen on small displays.
func fitScreen(w, h int) (int, int) {
	const smCxScreen, smCyScreen = 0, 1
	sw, _, _ := procGetSystemMetrics.Call(smCxScreen)
	sh, _, _ := procGetSystemMetrics.Call(smCyScreen)
	if sw > 0 {
		w = min(w, int(sw)*92/100)
	}
	if sh > 0 {
		h = min(h, int(sh)*90/100)
	}
	return w, h
}

// raiseWindow brings an open Lowpath window, of any process, to the front.
func raiseWindow() bool {
	class, _ := windows.UTF16PtrFromString(windowClass)
	title, _ := windows.UTF16PtrFromString(windowTitle)
	hwnd, _, _ := procFindWindowW.Call(uintptr(unsafe.Pointer(class)), uintptr(unsafe.Pointer(title)))
	if hwnd == 0 {
		return false
	}
	const swRestore = 9
	procShowWindow.Call(hwnd, swRestore)
	procSetForegroundWindow.Call(hwnd)
	return true
}

// WindowInstance finds the running copy by its window: Windows lets any
// program bring another's window to the front when the user just started
// it, so no channel between the two copies is needed.
type WindowInstance struct{}

// NewInstance returns the single-instance check for the UI at addr.
func NewInstance(addr string) Instance { return WindowInstance{} }

func (WindowInstance) Serve(func()) (io.Closer, error) { return nopCloser{}, nil }
func (WindowInstance) Signal() bool                    { return raiseWindow() }

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// processAlive reports whether process pid runs (it can be opened).
func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	const stillActive = 259
	return windows.GetExitCodeProcess(h, &code) == nil && code == stillActive
}
