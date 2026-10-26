package platform

import (
	"context"
	"fmt"
	"strings"
)

// AdminPrompt is the macOS privilege helper: Relaunch shows the system's
// administrator password dialog (osascript's "do shell script ... with
// administrator privileges", the same Authorization Services prompt
// installers use) and starts this program again as root in the
// background. Route mode on macOS (a utun device, issue #42) will need
// root; until then capture isn't available and the prompt isn't offered.
type AdminPrompt struct {
	Root
	x   execer
	exe func() (string, error)
}

const osascript = "/usr/bin/osascript"

func (a *AdminPrompt) CanElevate() bool {
	_, ok := firstFound(a.x, osascript)
	return ok
}

// Relaunch starts exe with args as root. The command travels as an
// argument of the script, never inside its source, so no quoting of the
// script is involved; the shell command itself quotes every word.
func (a *AdminPrompt) Relaunch(args []string) error {
	exe, err := a.exe()
	if err != nil {
		return err
	}
	words := []string{shellQuote(exe)}
	for _, arg := range args {
		words = append(words, shellQuote(arg))
	}
	// In the background, with no output to wait for, so the script returns
	// as soon as the root copy has started.
	cmd := strings.Join(words, " ") + " >/dev/null 2>&1 &"
	code, stderr, err := a.x.Run(context.Background(), command{path: osascript, args: []string{
		"-e", "on run argv",
		"-e", "do shell script (item 1 of argv) with prompt (item 2 of argv) with administrator privileges",
		"-e", "end run",
		cmd, "Lowpath needs administrator rights for route mode.",
	}})
	switch {
	case err != nil:
		return fmt.Errorf("platform: run osascript: %w", err)
	case code != 0 && strings.Contains(stderr, "-128"): // userCanceledErr
		return fmt.Errorf("restart as administrator: %w", ErrElevateCancelled)
	case code != 0:
		return fmt.Errorf("platform: restart as administrator: %s", strings.TrimSpace(lastLine(stderr)))
	}
	return nil
}

// shellQuote quotes s for sh: inside single quotes only ' itself needs
// care.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
