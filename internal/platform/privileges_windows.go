package platform

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// UAC is the Windows privilege helper: elevation is the process token's,
// and Relaunch goes through the UAC prompt (ShellExecute "runas").
type UAC struct{}

func NewUAC() *UAC { return &UAC{} }

func (*UAC) Elevated() bool   { return windows.GetCurrentProcessToken().IsElevated() }
func (*UAC) CanElevate() bool { return true }

// Relaunch starts this exe again with args through the UAC prompt. The
// prompt belongs to the new process; this one only learns whether it was
// accepted (ShellExecute fails with ERROR_CANCELLED when the user declines).
func (*UAC) Relaunch(args []string) error {
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
		return fmt.Errorf("restart as administrator: %w", ErrElevateCancelled)
	}
	if err != nil {
		return fmt.Errorf("restart as administrator: %w", err)
	}
	return nil
}
