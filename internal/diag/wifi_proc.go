package diag

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// procWLAN reads the signal strength from Linux's /proc/net/wireless (the
// wireless extensions' table, which mac80211 still fills in) and whether
// the link is up from sysfs. Band, channel and rates need nl80211, which
// Lowpath doesn't speak yet; Survey and Tune report errors.ErrUnsupported
// (Linux has no per-application switch for background scans).
type procWLAN struct {
	root  string                          // "/" on a real system; a fixture folder in tests
	names func(index int) (string, error) // interface index to name
}

func newProcWLAN(root string, names func(int) (string, error)) *procWLAN {
	if names == nil {
		names = func(i int) (string, error) {
			ifc, err := net.InterfaceByIndex(i)
			if err != nil {
				return "", err
			}
			return ifc.Name, nil
		}
	}
	return &procWLAN{root: root, names: names}
}

// wirelessRow is one interface of /proc/net/wireless.
type wirelessRow struct {
	Name  string
	Level *int // dBm
}

// parseProcWireless reads /proc/net/wireless. The level column is dBm on
// current kernels ("-56."); old drivers wrote it as an unsigned byte
// ("200." for -56 dBm), which only values from 128 up can be.
func parseProcWireless(data string) []wirelessRow {
	var rows []wirelessRow
	for _, line := range strings.Split(data, "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok || strings.Contains(name, "|") {
			continue
		}
		f := strings.Fields(rest)
		r := wirelessRow{Name: strings.TrimSpace(name)}
		if len(f) >= 3 {
			if v, err := strconv.ParseFloat(strings.TrimRight(f[2], "."), 64); err == nil {
				lv := int(v)
				if lv >= 128 && lv <= 255 {
					lv -= 256
				}
				r.Level = validRSSI(int32(lv))
			}
		}
		rows = append(rows, r)
	}
	return rows
}

func (p *procWLAN) path(parts ...string) string {
	return filepath.Join(append([]string{p.root}, parts...)...)
}

func (p *procWLAN) Link(index int) (WiFiLink, error) {
	l := WiFiLink{At: time.Now(), Source: "/proc/net/wireless"}
	data, err := os.ReadFile(p.path("proc", "net", "wireless"))
	if errors.Is(err, fs.ErrNotExist) {
		return l, fmt.Errorf("%w: the kernel has no /proc/net/wireless", ErrNoWiFi)
	}
	if err != nil {
		return l, fmt.Errorf("diag: read /proc/net/wireless: %w", err)
	}
	rows := parseProcWireless(string(data))
	want := ""
	if index > 0 {
		if want, err = p.names(index); err != nil {
			return l, fmt.Errorf("diag: interface %d: %w", index, err)
		}
	}
	var row *wirelessRow
	for i := range rows {
		if want == "" || rows[i].Name == want {
			row = &rows[i]
			break
		}
	}
	switch {
	case row == nil && want != "":
		return l, fmt.Errorf("%w: the connection in use isn't a Wi-Fi adapter", ErrNoWiFi)
	case row == nil:
		return l, fmt.Errorf("%w: the kernel lists no Wi-Fi adapter", ErrNoWiFi)
	}
	l.Interface = row.Name
	if st, err := os.ReadFile(p.path("sys", "class", "net", row.Name, "operstate")); err == nil && strings.TrimSpace(string(st)) != "up" {
		return l, ErrWiFiDisconnected
	}
	l.RSSI = row.Level
	if l.RSSI == nil {
		l.note("The Wi-Fi driver doesn't report the signal strength in dBm.")
	}
	l.note("On Linux Lowpath reads only the signal strength so far; band, channel and rates need nl80211.")
	return l, nil
}

func (p *procWLAN) Survey(int) (WiFiSurvey, error) {
	return WiFiSurvey{}, wifiUnsupported{"reading the Wi-Fi channel"}
}

func (p *procWLAN) Tune(int) (WiFiTuning, error) {
	return nil, wifiUnsupported{"pausing Wi-Fi scans"}
}
