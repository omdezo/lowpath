//go:build !windows

package platform

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// osExec runs real programs.
type osExec struct{}

func (osExec) LookPath(name string) (string, error) { return exec.LookPath(name) }

func (osExec) Start(c command) (process, error) {
	cmd := c.cmd(context.Background())
	tail := &tailBuffer{max: 4 << 10}
	cmd.Stderr = tail
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &osProcess{cmd: cmd, stderr: tail}, nil
}

func (osExec) Run(ctx context.Context, c command) (int, string, error) {
	cmd := c.cmd(ctx)
	tail := &tailBuffer{max: 4 << 10}
	cmd.Stderr = tail
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, tail.String(), nil
	case errors.As(err, &exit) && exit.ExitCode() >= 0:
		return exit.ExitCode(), tail.String(), nil
	default:
		return -1, tail.String(), err
	}
}

func (osExec) Spawn(c command) error {
	cmd := c.cmd(context.Background())
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func (c command) cmd(ctx context.Context) *exec.Cmd {
	cmd := exec.CommandContext(ctx, c.path, c.args...)
	if len(c.env) > 0 {
		cmd.Env = append(os.Environ(), c.env...)
	}
	return cmd
}

type osProcess struct {
	cmd    *exec.Cmd
	stderr *tailBuffer
}

func (p *osProcess) Wait() error {
	err := p.cmd.Wait()
	if err != nil {
		if msg := lastLine(p.stderr.String()); msg != "" {
			err = fmt.Errorf("%w: %s", err, msg)
		}
	}
	return err
}

func (p *osProcess) Signal(sig os.Signal) error { return p.cmd.Process.Signal(sig) }
func (p *osProcess) Kill() error                { return p.cmd.Process.Kill() }

func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	return err == nil && p.Signal(syscall.Signal(0)) == nil
}
