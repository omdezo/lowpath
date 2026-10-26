package main

import (
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

// showError tells the user why the app could not start: a GUI build has no
// console to print to.
func showError(title, msg string) {
	t, _ := windows.UTF16PtrFromString(title)
	m, _ := windows.UTF16PtrFromString(msg)
	windows.MessageBox(0, m, t, windows.MB_OK|windows.MB_ICONERROR)
}
