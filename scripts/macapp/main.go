// Macapp packs Lowpath's macOS builds into Lowpath.app, the bundle a Mac
// user starts with one click: a universal program (Apple silicon and Intel
// in one file), Info.plist and the icon. It runs on any OS, so the bundle
// is built next to the Windows and Linux binaries.
//
//	GOOS=darwin GOARCH=arm64 go build -o lowpath-arm64 ./cmd/lowpath
//	GOOS=darwin GOARCH=amd64 go build -o lowpath-amd64 ./cmd/lowpath
//	go run ./scripts/macapp -out dist/Lowpath-macos.zip -version 0.3.0 lowpath-arm64 lowpath-amd64
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

func main() {
	out := flag.String("out", "Lowpath.app", "bundle folder to write (replaced if it exists), or a .zip archive of it")
	icon := flag.String("icon", "cmd/lowpath/winres/icon.png", "app icon, a 256x256 PNG")
	version := flag.String("version", "0.0.0", "version shown in Finder")
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: macapp [-out Lowpath.app] [-icon icon.png] [-version x.y.z] <darwin binary>...")
		os.Exit(2)
	}
	png, err := os.ReadFile(*icon)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	b := Bundle{Name: "Lowpath", Executable: "lowpath", ID: "io.github.omdezo.lowpath", Version: *version, Icon: png}
	if err := write(b, *out, flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func write(b Bundle, out string, binaries []string) error {
	if !strings.HasSuffix(out, ".zip") {
		return b.Write(out, binaries)
	}
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	if err := b.WriteZip(f, binaries); err != nil {
		f.Close()
		os.Remove(out)
		return err
	}
	return f.Close()
}
