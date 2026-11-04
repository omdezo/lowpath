package detect

import (
	"errors"
	"fmt"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// snapshotTries bounds the retries of a snapshot that fails with
// ERROR_BAD_LENGTH, which Microsoft documents as transient ("call the
// function again until it succeeds") while processes start and exit.
const snapshotTries = 4

// running reads the Toolhelp32 process snapshot. A snapshot or walk that
// fails is an error, not an empty list: a game whose name was simply not
// read must not look like it exited.
func running() (map[string]bool, error) {
	var err error
	for range snapshotTries {
		var out map[string]bool
		if out, err = snapshotProcesses(); err == nil {
			return out, nil
		}
		if !errors.Is(err, windows.ERROR_BAD_LENGTH) {
			break
		}
	}
	return nil, fmt.Errorf("detect: read the process snapshot: %w", err)
}

func snapshotProcesses() (map[string]bool, error) {
	snap, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer syscall.CloseHandle(snap)
	out := map[string]bool{}
	var e syscall.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = syscall.Process32First(snap, &e); err == nil; err = syscall.Process32Next(snap, &e) {
		out[strings.ToLower(syscall.UTF16ToString(e.ExeFile[:]))] = true
	}
	if !errors.Is(err, syscall.ERROR_NO_MORE_FILES) {
		return nil, err
	}
	return out, nil
}
