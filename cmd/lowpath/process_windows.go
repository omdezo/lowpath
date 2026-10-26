package main

import (
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// waitForExit blocks until process pid has exited (or timeout passes). A
// process that is already gone can't be opened, which also means "done".
func waitForExit(pid int, timeout time.Duration) {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)
	windows.WaitForSingleObject(h, uint32(timeout/time.Millisecond))
}

// stopSignals end the app cleanly: Ctrl+C, and the console closing or the
// session ending (which Go delivers as SIGTERM).
func stopSignals() []os.Signal { return []os.Signal{os.Interrupt, syscall.SIGTERM} }

// handBack has nothing to do on Windows: the elevated copy runs as the same
// user, so its files are the user's.
func handBack(...string) {}
