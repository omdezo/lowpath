package detect

import (
	"os"
	"path/filepath"
)

// steamBases are where the Steam client installs on Windows.
func steamBases() []string {
	var out []string
	for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles"} {
		if v := os.Getenv(env); v != "" {
			out = append(out, filepath.Join(v, "Steam"))
		}
	}
	return out
}
