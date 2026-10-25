//go:build !windows

package detect

import (
	"os"
	"path/filepath"
	"strings"
)

func running() map[string]bool {
	out := map[string]bool{}
	matches, _ := filepath.Glob("/proc/[0-9]*/comm")
	for _, m := range matches {
		if b, err := os.ReadFile(m); err == nil {
			out[strings.ToLower(strings.TrimSpace(string(b)))] = true
		}
	}
	return out
}
