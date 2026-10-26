package platform

import (
	"context"
	"os"
	"strings"
	"sync"
)

// command is a program the desktop shell or a privilege helper starts.
type command struct {
	path string
	args []string
	env  []string // added to this process's environment
}

func (c command) String() string { return strings.Join(append([]string{c.path}, c.args...), " ") }

// process is a started command.
type process interface {
	// Wait returns when the process has exited: nil for exit status 0.
	Wait() error
	Signal(sig os.Signal) error
	Kill() error
}

// execer runs programs. The desktop shell and the privilege helpers take it
// as a dependency, so tests can play windows that open, fail or close, and
// prompts that are accepted or cancelled, without starting anything.
type execer interface {
	LookPath(name string) (string, error)
	// Start starts c and returns at once.
	Start(c command) (process, error)
	// Run runs c to the end and returns its exit code and the end of its
	// error output. err is set only when it could not be run at all.
	Run(ctx context.Context, c command) (code int, stderr string, err error)
	// Spawn starts c in a session of its own, detached: it keeps running
	// after this process exits and isn't sent this terminal's signals.
	Spawn(c command) error
}

// tailBuffer keeps the last max bytes written to it: enough of a browser's
// chatty error output to say why it didn't start.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if over := len(t.b) - t.max; over > 0 {
		t.b = append(t.b[:0], t.b[over:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { t.mu.Lock(); defer t.mu.Unlock(); return string(t.b) }

// lastLine is the last non-empty line of s, cut to 200 bytes.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	l := strings.TrimSpace(lines[len(lines)-1])
	if len(l) > 200 {
		l = l[:200]
	}
	return l
}

// firstFound returns the first of paths (absolute, or names looked up on
// PATH) that exists as a program.
func firstFound(x execer, paths ...string) (string, bool) {
	for _, p := range paths {
		if found, err := x.LookPath(p); err == nil {
			return found, true
		}
	}
	return "", false
}
