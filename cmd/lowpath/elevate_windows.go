package main

import (
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows"

	"lowpath/internal/app"
)

const elevationSupported = true

// relaunchElevated starts this exe again with args through the UAC prompt.
// It returns once the elevated process is starting, or ErrElevateCancelled
// when the user declines.
func relaunchElevated(args []string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cwd, _ := os.Getwd()
	verb, _ := windows.UTF16PtrFromString("runas")
	file, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return err
	}
	params, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(args))
	if err != nil {
		return err
	}
	dir, _ := windows.UTF16PtrFromString(cwd)
	err = windows.ShellExecute(0, verb, file, params, dir, windows.SW_SHOWNORMAL)
	if errors.Is(err, windows.ERROR_CANCELLED) {
		return app.ErrElevateCancelled
	}
	if err != nil {
		return fmt.Errorf("restart as administrator: %w", err)
	}
	return nil
}

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
