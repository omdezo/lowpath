package platform

import (
	"os"
	"path/filepath"
	"testing"
)

// The real filesystem and extended-attribute reads behind Polkit.
func TestPolkitSystemReads(t *testing.T) {
	f := filepath.Join(t.TempDir(), "lowpath")
	if err := os.WriteFile(f, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := statMount(f); err != nil {
		t.Errorf("statMount: %v", err)
	}
	if _, err := statMount(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("statMount of a missing file")
	}
	if hasFileCaps(f) {
		t.Error("a new file has capabilities")
	}
	if exe, err := executable(); err != nil || !filepath.IsAbs(exe) {
		t.Errorf("executable = %q, %v", exe, err)
	}
	// /proc is mounted nosuid on every distribution.
	if m, err := statMount("/proc/self/status"); err == nil && !m.noSUID {
		t.Logf("/proc not reported nosuid: %+v", m)
	}
}
