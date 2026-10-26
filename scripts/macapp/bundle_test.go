package main

import (
	"archive/zip"
	"bytes"
	"debug/macho"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// buildDarwin cross-builds a tiny program for macOS on arch.
func buildDarwin(t *testing.T, arch string) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	os.WriteFile(src, []byte("package main\nfunc main() {}\n"), 0o600)
	out := filepath.Join(dir, "prog-"+arch)
	cmd := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", out, src)
	cmd.Env = append(os.Environ(), "GOOS=darwin", "GOARCH="+arch, "CGO_ENABLED=0", "GOFLAGS=")
	cmd.Dir = dir
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build for darwin/%s: %v\n%s", arch, err, b)
	}
	return out
}

func TestBundle(t *testing.T) {
	arm, intel := buildDarwin(t, "arm64"), buildDarwin(t, "amd64")
	icon, err := os.ReadFile(filepath.Join("..", "..", "cmd", "lowpath", "winres", "icon.png"))
	if err != nil {
		t.Fatal(err)
	}
	app := filepath.Join(t.TempDir(), "Lowpath.app")
	b := Bundle{Name: "Lowpath", Executable: "lowpath", ID: "io.github.omdezo.lowpath", Version: "1.2.3", Icon: icon}
	if err := b.Write(app, []string{arm, intel}); err != nil {
		t.Fatal(err)
	}

	// The program is a universal file with both slices intact.
	fat, err := macho.OpenFat(filepath.Join(app, "Contents", "MacOS", "lowpath"))
	if err != nil {
		t.Fatalf("not a universal program: %v", err)
	}
	defer fat.Close()
	var cpus []macho.Cpu
	for _, a := range fat.Arches {
		cpus = append(cpus, a.Cpu)
		if a.Offset%(1<<14) != 0 {
			t.Errorf("%v slice at %d, not page aligned", a.Cpu, a.Offset)
		}
	}
	if len(cpus) != 2 || cpus[0] != macho.CpuArm64 || cpus[1] != macho.CpuAmd64 {
		t.Errorf("architectures %v", cpus)
	}
	whole, _ := os.ReadFile(filepath.Join(app, "Contents", "MacOS", "lowpath"))
	thin, _ := os.ReadFile(arm)
	if a := fat.Arches[0]; !bytes.Equal(whole[a.Offset:a.Offset+a.Size], thin) {
		t.Error("arm64 slice differs from the program")
	}
	if fi, _ := os.Stat(filepath.Join(app, "Contents", "MacOS", "lowpath")); runtime.GOOS != "windows" && fi.Mode().Perm()&0o111 == 0 {
		t.Error("program not executable")
	}

	plist, _ := os.ReadFile(filepath.Join(app, "Contents", "Info.plist"))
	for _, want := range []string{
		"<key>CFBundleExecutable</key><string>lowpath</string>",
		"<key>CFBundleIdentifier</key><string>io.github.omdezo.lowpath</string>",
		"<key>CFBundleShortVersionString</key><string>1.2.3</string>",
		"<key>CFBundleIconFile</key><string>Lowpath</string>",
		"<key>LSUIElement</key><true/>",
	} {
		if !strings.Contains(string(plist), want) {
			t.Errorf("Info.plist lacks %s", want)
		}
	}

	icns, _ := os.ReadFile(filepath.Join(app, "Contents", "Resources", "Lowpath.icns"))
	if string(icns[:4]) != "icns" || binary.BigEndian.Uint32(icns[4:]) != uint32(len(icns)) || string(icns[8:12]) != "ic08" || !bytes.Equal(icns[16:], icon) {
		t.Errorf("icon file header % x", icns[:16])
	}
}

func TestBundleRejects(t *testing.T) {
	arm := buildDarwin(t, "arm64")
	icon, _ := os.ReadFile(filepath.Join("..", "..", "cmd", "lowpath", "winres", "icon32.png"))
	app := filepath.Join(t.TempDir(), "x.app")
	if err := (Bundle{Icon: icon}).Write(app, []string{arm}); err == nil || !strings.Contains(err.Error(), "256x256") {
		t.Errorf("small icon: %v", err)
	}
	notMacho := filepath.Join(t.TempDir(), "x")
	os.WriteFile(notMacho, []byte("MZ not a mac program"), 0o600)
	if _, err := universal([][]byte{[]byte("MZ")}); err == nil {
		t.Error("a non-Mach-O file accepted")
	}
	data, _ := os.ReadFile(arm)
	if _, err := universal([][]byte{data, data}); err == nil {
		t.Error("two arm64 programs accepted")
	}
	if one, _ := universal([][]byte{data}); !bytes.Equal(one, data) {
		t.Error("a single program was changed")
	}
}

// The archive keeps the program executable even when made on Windows.
func TestBundleZip(t *testing.T) {
	arm := buildDarwin(t, "arm64")
	icon, _ := os.ReadFile(filepath.Join("..", "..", "cmd", "lowpath", "winres", "icon.png"))
	out := filepath.Join(t.TempDir(), "Lowpath-macos.zip")
	b := Bundle{Name: "Lowpath", Executable: "lowpath", ID: "x", Version: "1", Icon: icon}
	if err := write(b, out, []string{arm}); err != nil {
		t.Fatal(err)
	}
	z, err := zip.OpenReader(out)
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	modes := map[string]os.FileMode{}
	for _, f := range z.File {
		modes[f.Name] = f.Mode()
	}
	if m := modes["Lowpath.app/Contents/MacOS/lowpath"]; m.Perm() != 0o755 || !m.IsRegular() {
		t.Errorf("program mode %v, want 0755", m)
	}
	if m := modes["Lowpath.app/Contents/"]; !m.IsDir() {
		t.Errorf("Contents/ mode %v", m)
	}
	if m := modes["Lowpath.app/Contents/Info.plist"]; m.Perm() != 0o644 {
		t.Errorf("Info.plist mode %v", m)
	}
	// A failed bundle leaves no half archive.
	bad := filepath.Join(t.TempDir(), "bad.zip")
	if err := write(Bundle{Icon: icon}, bad, []string{filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Error("missing program accepted")
	}
	if _, err := os.Stat(bad); !os.IsNotExist(err) {
		t.Error("half archive left behind")
	}
}
