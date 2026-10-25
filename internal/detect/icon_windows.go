package detect

import (
	"errors"
	"fmt"
	"image"
	"image/color"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32                  = windows.NewLazySystemDLL("user32.dll")
	gdi32                   = windows.NewLazySystemDLL("gdi32.dll")
	procPrivateExtractIcons = user32.NewProc("PrivateExtractIconsW")
	procGetIconInfo         = user32.NewProc("GetIconInfo")
	procDestroyIcon         = user32.NewProc("DestroyIcon")
	procCreateCompatibleDC  = gdi32.NewProc("CreateCompatibleDC")
	procDeleteDC            = gdi32.NewProc("DeleteDC")
	procGetDIBits           = gdi32.NewProc("GetDIBits")
	procDeleteObject        = gdi32.NewProc("DeleteObject")
)

type iconInfo struct {
	fIcon    int32
	xHotspot uint32
	yHotspot uint32
	hbmMask  windows.Handle
	hbmColor windows.Handle
}

type bitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

var errNoIcon = errors.New("detect: the program has no icon")

// ExeIcon returns the main icon of a program, rendered at size×size. Windows
// picks the closest size the icon has and scales it.
func ExeIcon(path string, size int) (image.Image, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	var hicon windows.Handle
	var id uint32
	n, _, _ := procPrivateExtractIcons.Call(uintptr(unsafe.Pointer(p)), 0, uintptr(size), uintptr(size),
		uintptr(unsafe.Pointer(&hicon)), uintptr(unsafe.Pointer(&id)), 1, 0)
	if n == 0 || n == 0xFFFFFFFF || hicon == 0 {
		return nil, errNoIcon
	}
	defer procDestroyIcon.Call(uintptr(hicon))

	var ii iconInfo
	if r, _, err := procGetIconInfo.Call(uintptr(hicon), uintptr(unsafe.Pointer(&ii))); r == 0 {
		return nil, fmt.Errorf("detect: GetIconInfo: %w", err)
	}
	defer procDeleteObject.Call(uintptr(ii.hbmMask))
	if ii.hbmColor == 0 {
		return nil, errNoIcon // monochrome icon: not worth showing
	}
	defer procDeleteObject.Call(uintptr(ii.hbmColor))

	dc, _, _ := procCreateCompatibleDC.Call(0)
	if dc == 0 {
		return nil, errors.New("detect: CreateCompatibleDC failed")
	}
	defer procDeleteDC.Call(dc)

	px, err := dibits(dc, ii.hbmColor, size)
	if err != nil {
		return nil, err
	}
	// Old icons carry no alpha channel; their AND mask says what's transparent.
	hasAlpha := false
	for i := 3; i < len(px); i += 4 {
		if px[i] != 0 {
			hasAlpha = true
			break
		}
	}
	var mask []byte
	if !hasAlpha {
		if mask, err = dibits(dc, ii.hbmMask, size); err != nil {
			return nil, err
		}
	}

	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	for i := 0; i < size*size; i++ {
		b, g, r, a := px[i*4], px[i*4+1], px[i*4+2], px[i*4+3]
		if !hasAlpha {
			a = 255
			if mask[i*4] != 0 { // white in the mask = transparent
				a = 0
			}
		}
		img.SetNRGBA(i%size, i/size, color.NRGBA{r, g, b, a})
	}
	return img, nil
}

// dibits reads a bitmap as top-down 32-bit BGRA, size×size.
func dibits(dc uintptr, bm windows.Handle, size int) ([]byte, error) {
	bi := bitmapInfoHeader{Width: int32(size), Height: -int32(size), Planes: 1, BitCount: 32}
	bi.Size = uint32(unsafe.Sizeof(bi))
	// BITMAPINFO = header + color table; room for a 2-entry table (masks).
	var info [unsafe.Sizeof(bitmapInfoHeader{}) + 8]byte
	*(*bitmapInfoHeader)(unsafe.Pointer(&info[0])) = bi
	buf := make([]byte, size*size*4)
	r, _, _ := procGetDIBits.Call(dc, uintptr(bm), 0, uintptr(size), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&info[0])), 0)
	if r == 0 {
		return nil, errors.New("detect: GetDIBits failed")
	}
	return buf, nil
}

// RunningExe returns the full path of a running process with one of the given
// image names, or "" when none runs or Windows won't tell (protected games).
func RunningExe(names []string) string {
	want := map[string]bool{}
	for _, n := range names {
		want[strings.ToLower(n)] = true
	}
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(snap)
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		if !want[strings.ToLower(windows.UTF16ToString(e.ExeFile[:]))] {
			continue
		}
		if p := processPath(e.ProcessID); p != "" {
			return p
		}
	}
	return ""
}

func processPath(pid uint32) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if windows.QueryFullProcessImageName(h, 0, &buf[0], &n) != nil {
		return ""
	}
	return windows.UTF16ToString(buf[:n])
}
