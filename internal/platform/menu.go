package platform

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MenuEntry puts Lowpath in the desktop's app menu (GNOME, KDE and the
// others that follow the freedesktop specs): a desktop entry in
// $XDG_DATA_HOME/applications that starts this program, and its icon in the
// hicolor theme. Lowpath ships as one program file, so the first start
// writes them, and later starts from the menu are one click; a moved
// program updates its entry the next time it starts.
type MenuEntry struct {
	dataHome string // $XDG_DATA_HOME, or ~/.local/share
	icon     []byte
}

// NewMenuEntry returns the menu entry for the current user, or an error
// when there is no home folder to put it in.
func NewMenuEntry(icon []byte, getenv func(string) string, home func() (string, error)) (*MenuEntry, error) {
	dir := getenv("XDG_DATA_HOME")
	if !isAbs(dir) { // the spec says to ignore a relative one
		h, err := home()
		if err != nil {
			return nil, fmt.Errorf("platform: no home folder for the app menu: %w", err)
		}
		dir = filepath.Join(h, ".local", "share")
	}
	return &MenuEntry{dataHome: dir, icon: icon}, nil
}

// Install writes the entry for exe and the icon, unless they already say
// the same.
func (m *MenuEntry) Install(exe string) error {
	if !isAbs(exe) {
		return fmt.Errorf("platform: menu entry needs an absolute program path, not %q", exe)
	}
	// "go run" and "go test" binaries live in a temporary build folder that
	// is gone when they exit.
	if strings.Contains(filepath.ToSlash(exe), "/go-build") {
		return nil
	}
	if len(m.icon) > 0 {
		icon := filepath.Join(m.dataHome, "icons", "hicolor", "256x256", "apps", "lowpath.png")
		if err := writeIfChanged(icon, m.icon); err != nil {
			return err
		}
	}
	return writeIfChanged(filepath.Join(m.dataHome, "applications", "lowpath.desktop"), []byte(desktopEntry(exe)))
}

// Path is where the entry is written.
func (m *MenuEntry) Path() string {
	return filepath.Join(m.dataHome, "applications", "lowpath.desktop")
}

// desktopEntry is the entry that starts exe (Desktop Entry Specification
// 1.5). StartupWMClass and the icon name match what the app windows set.
func desktopEntry(exe string) string {
	return "[Desktop Entry]\n" +
		"Type=Application\n" +
		"Version=1.5\n" +
		"Name=Lowpath\n" +
		"GenericName=Game network optimizer\n" +
		"Comment=Lower ping, jitter and packet loss in online games\n" +
		"TryExec=" + escapeString(exe) + "\n" +
		"Exec=" + escapeString(quoteExecArg(exe)) + "\n" +
		"Icon=lowpath\n" +
		"Terminal=false\n" +
		"Categories=Network;Game;\n" +
		"Keywords=ping;lag;latency;packet loss;relay;\n" +
		"StartupNotify=true\n" +
		"StartupWMClass=lowpath\n"
}

// quoteExecArg quotes one Exec argument: reserved characters need double
// quotes, and inside them ", `, $ and \ take a backslash. A literal % is
// written %%, since % starts a field code.
func quoteExecArg(arg string) string {
	arg = strings.ReplaceAll(arg, "%", "%%")
	if !strings.ContainsAny(arg, " \t\n\"'\\><~|&;$*?#`()") {
		return arg
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range arg {
		if strings.ContainsRune("\"`$\\", r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

// escapeString applies the escapes of a desktop entry string value, which
// are undone before the Exec quoting is read.
func escapeString(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`, "\t", `\t`, "\r", `\r`).Replace(s)
}

// writeIfChanged writes b to path (creating its folders) through a
// temporary file, so a reader never sees half an entry, and leaves a file
// that already holds b alone.
func writeIfChanged(path string, b []byte) error {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, b) {
		return nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".lowpath-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(b)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr, os.Chmod(tmp.Name(), 0o644)); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// isAbs accepts Unix absolute paths on every OS, so the entry can be tested
// anywhere.
func isAbs(p string) bool { return strings.HasPrefix(p, "/") || filepath.IsAbs(p) }
