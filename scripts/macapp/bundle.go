package main

import (
	"archive/zip"
	"bytes"
	"debug/macho"
	"encoding/binary"
	"errors"
	"fmt"
	"html"
	"image/png"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"time"
)

// Bundle is a macOS application bundle with one program in it.
type Bundle struct {
	Name, Executable, ID, Version string
	Icon                          []byte // PNG, 256x256
}

// bundleFile is one file of the bundle, by its path inside it.
type bundleFile struct {
	path string // slash-separated, under <Name>.app/
	data []byte
	mode os.FileMode
}

// files are the bundle's files: Contents/Info.plist, the program in
// Contents/MacOS (one Mach-O per architecture combined into a universal
// file) and the icon in Contents/Resources.
func (b Bundle) files(binaries []string) ([]bundleFile, error) {
	var thin [][]byte
	for _, path := range binaries {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		thin = append(thin, data)
	}
	exe, err := universal(thin)
	if err != nil {
		return nil, err
	}
	icns, err := icnsFromPNG(b.Icon)
	if err != nil {
		return nil, err
	}
	return []bundleFile{
		{"Contents/Info.plist", []byte(b.infoPlist()), 0o644},
		{"Contents/PkgInfo", []byte("APPL????"), 0o644},
		{"Contents/MacOS/" + b.Executable, exe, 0o755},
		{"Contents/Resources/" + b.Name + ".icns", icns, 0o644},
	}, nil
}

// Write makes the bundle as the folder dir, replacing it.
func (b Bundle) Write(dir string, binaries []string) error {
	files, err := b.files(binaries)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	for _, f := range files {
		path := filepath.Join(dir, filepath.FromSlash(f.path))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, f.data, f.mode); err != nil {
			return err
		}
	}
	return nil
}

// WriteZip writes the bundle into a zip archive with Unix file modes, so
// the program stays executable when the archive is made on Windows: a
// folder copied from there loses the mode, and macOS then calls the app
// damaged.
func (b Bundle) WriteZip(w io.Writer, binaries []string) error {
	files, err := b.files(binaries)
	if err != nil {
		return err
	}
	z := zip.NewWriter(w)
	root := b.Name + ".app/"
	dirs := map[string]bool{}
	for _, f := range files {
		// Folders first, so unzip tools create them with their mode.
		for d := path.Dir(f.path); d != "."; d = path.Dir(d) {
			dirs[d] = true
		}
	}
	names := []string{""}
	for d := range dirs {
		names = append(names, d+"/")
	}
	slices.Sort(names)
	for _, d := range names {
		h := &zip.FileHeader{Name: root + d, Method: zip.Store, Modified: zipTime()}
		h.SetMode(os.ModeDir | 0o755)
		if _, err := z.CreateHeader(h); err != nil {
			return err
		}
	}
	for _, f := range files {
		h := &zip.FileHeader{Name: root + f.path, Method: zip.Deflate, Modified: zipTime()}
		h.SetMode(f.mode)
		fw, err := z.CreateHeader(h)
		if err != nil {
			return err
		}
		if _, err := fw.Write(f.data); err != nil {
			return err
		}
	}
	return z.Close()
}

// zipTime is a fixed time for the archive's entries, so the same build
// gives the same archive.
func zipTime() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

// infoPlist describes the bundle. LSUIElement keeps the program itself out
// of the Dock: it has no window of its own, the app window is a helper
// process that shows its own Dock icon.
func (b Bundle) infoPlist() string {
	e := html.EscapeString
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleDevelopmentRegion</key><string>en</string>
	<key>CFBundleDisplayName</key><string>` + e(b.Name) + `</string>
	<key>CFBundleExecutable</key><string>` + e(b.Executable) + `</string>
	<key>CFBundleIconFile</key><string>` + e(b.Name) + `</string>
	<key>CFBundleIdentifier</key><string>` + e(b.ID) + `</string>
	<key>CFBundleInfoDictionaryVersion</key><string>6.0</string>
	<key>CFBundleName</key><string>` + e(b.Name) + `</string>
	<key>CFBundlePackageType</key><string>APPL</string>
	<key>CFBundleShortVersionString</key><string>` + e(b.Version) + `</string>
	<key>CFBundleVersion</key><string>` + e(b.Version) + `</string>
	<key>LSApplicationCategoryType</key><string>public.app-category.utilities</string>
	<key>LSMinimumSystemVersion</key><string>12.0</string>
	<key>LSUIElement</key><true/>
	<key>NSHighResolutionCapable</key><true/>
</dict>
</plist>
`
}

// universal combines thin Mach-O programs into one fat (universal) file,
// the format lipo writes: a big-endian header listing each architecture's
// slice, the slices aligned to 16 KiB pages. One program is returned as is.
func universal(thin [][]byte) ([]byte, error) {
	if len(thin) == 0 {
		return nil, errors.New("macapp: no programs")
	}
	type arch struct{ cpu, sub uint32 }
	var archs []arch
	for i, data := range thin {
		f, err := macho.NewFile(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("macapp: program %d isn't a thin Mach-O file: %w", i+1, err)
		}
		a := arch{uint32(f.Cpu), f.SubCpu}
		for _, seen := range archs {
			if seen.cpu == a.cpu {
				return nil, fmt.Errorf("macapp: two programs for %v", f.Cpu)
			}
		}
		archs = append(archs, a)
	}
	if len(thin) == 1 {
		return thin[0], nil
	}
	const align = 14 // 2^14: arm64 pages
	var out bytes.Buffer
	binary.Write(&out, binary.BigEndian, [2]uint32{macho.MagicFat, uint32(len(thin))})
	offset := uint32(8 + 20*len(thin))
	offsets := make([]uint32, len(thin))
	for i, data := range thin {
		offset = (offset + 1<<align - 1) &^ (1<<align - 1)
		offsets[i] = offset
		binary.Write(&out, binary.BigEndian, [5]uint32{archs[i].cpu, archs[i].sub, offset, uint32(len(data)), align})
		offset += uint32(len(data))
	}
	for i, data := range thin {
		out.Write(make([]byte, int(offsets[i])-out.Len()))
		out.Write(data)
	}
	return out.Bytes(), nil
}

// icnsFromPNG wraps a 256x256 PNG in an Apple icon file: the "icns" header
// and one "ic08" (256x256 PNG) element.
func icnsFromPNG(data []byte) ([]byte, error) {
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("macapp: icon: %w", err)
	}
	if cfg.Width != 256 || cfg.Height != 256 {
		return nil, fmt.Errorf("macapp: icon is %dx%d, want 256x256", cfg.Width, cfg.Height)
	}
	var out bytes.Buffer
	out.WriteString("icns")
	binary.Write(&out, binary.BigEndian, uint32(8+8+len(data)))
	out.WriteString("ic08")
	binary.Write(&out, binary.BigEndian, uint32(8+len(data)))
	out.Write(data)
	return out.Bytes(), nil
}
