// Lowpathsvc is the background service, LowpathSvc: it runs the engine and
// packet capture with the rights capture needs, and serves the app's API on
// a named pipe (\\.\pipe\lowpath on Windows, /run/lowpath/api.sock
// elsewhere) instead of a loopback port. The Lowpath window then runs
// unelevated and talks to it (see cmd/lowpath and app.Frontend), so after the
// one-time install nobody has to answer an administrator prompt, a reboot
// included. This is the development version of the service the roadmap's
// M7 hardens; the window works without it as before.
//
//	lowpathsvc install      install and start the service (one administrator prompt)
//	lowpathsvc uninstall    stop and remove it (one administrator prompt)
//	lowpathsvc status       whether it is installed, running and answering
//	lowpathsvc run          serve in the foreground, for debugging (Ctrl+C stops)
//
// The service manager starts it with only -data; it then runs as the
// service until told to stop.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"lowpath/internal/app"
	"lowpath/internal/platform"
)

func main() {
	plat := platform.Native()
	cmd, args := "", os.Args[1:]
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}
	c := newCLI(plat, os.Stdout, os.Stderr)
	err := c.dispatch(cmd, args)
	if c.pause {
		// An install relaunched elevated runs in a console window of its
		// own, which would close before anyone read the result.
		fmt.Fprint(os.Stdout, "\nPress Enter to close this window.")
		bufio.NewReader(os.Stdin).ReadString('\n')
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "lowpathsvc:", err)
		os.Exit(1)
	}
}

// cli runs one subcommand against a platform.
type cli struct {
	plat   *platform.Platform
	runner platform.ServiceRunner
	out    io.Writer
	errOut io.Writer
	pause  bool // wait for Enter before exiting (set by -pause)
	// exe is this program's path (os.Executable).
	exe func() (string, error)
}

func newCLI(plat *platform.Platform, out, errOut io.Writer) *cli {
	return &cli{plat: plat, runner: platform.NewServiceRunner(), out: out, errOut: errOut, exe: os.Executable}
}

const usage = `usage: lowpathsvc <command> [flags]

  install     install the Lowpath service and start it; asks for
              administrator rights once. Settings and relays are copied
              from your Lowpath folder the first time.
  uninstall   stop and remove the service (its settings folder is kept)
  status      show whether the service is installed, running and answering
  run         serve in the foreground (Ctrl+C stops), for debugging

Run "lowpathsvc <command> -h" for the command's flags.`

func (c *cli) dispatch(cmd string, args []string) error {
	switch cmd {
	case "install":
		return c.install(args)
	case "uninstall":
		return c.uninstall(args)
	case "status":
		return c.status(args)
	case "run":
		return c.run(args, false)
	case "":
		// Started by the service manager (with -data), or by hand.
		return c.run(args, true)
	case "help":
		fmt.Fprintln(c.out, usage)
		return nil
	default:
		fmt.Fprintln(c.errOut, usage)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// runOptions are the flags of run (and of the service's command line).
type runOptions struct {
	data, relays, pipe string
}

func (c *cli) runFlags(name string, args []string) (runOptions, error) {
	var o runOptions
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(c.errOut)
	def, _ := c.plat.Paths.ServiceDataDir()
	fs.StringVar(&o.data, "data", def, "folder for the service's settings, relays and log")
	fs.StringVar(&o.relays, "relays", "", "optional read-only relay list (JSON)")
	fs.StringVar(&o.pipe, "pipe", "", "serve on this pipe (Windows) or socket path instead of "+c.plat.ServiceAPI.String()+" (development and tests)")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if o.data == "" {
		return o, errors.New("no data folder: pass -data")
	}
	return o, nil
}

// run serves the API until stopped: as the service when the service
// manager started this process (asService and the runner agrees), in the
// foreground otherwise.
func (c *cli) run(args []string, asService bool) error {
	o, err := c.runFlags("run", args)
	if err != nil {
		return err
	}
	svc := newService(c.plat, o)
	if asService {
		ok, err := c.runner.Run(platform.ServiceName, svc.serve)
		if ok {
			return err
		}
		if err != nil {
			return err
		}
		if len(args) == 0 {
			// Double-clicked: say what this program is for.
			fmt.Fprintln(c.out, usage)
			return nil
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return svc.serve(ctx)
}

func (c *cli) install(args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(c.errOut)
	userData, _ := c.plat.Paths.DataDir()
	importDir := fs.String("import", userData, "copy state.json (settings, relays, custom games) from this folder when the service has none yet")
	fs.BoolVar(&c.pause, "pause", false, "wait for Enter before exiting (used when restarting elevated)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !c.plat.Privileges.Elevated() {
		// The administrator's own folder may not be this user's: pass this
		// user's along.
		return c.elevate("install", "-import", *importDir, "-pause")
	}
	exe, err := c.exe()
	if err != nil {
		return err
	}
	data, err := c.plat.Paths.ServiceDataDir()
	if err != nil {
		return err
	}
	cfg := platform.ServiceConfig{Exe: exe, Args: []string{"-data", data}, DataDir: data}
	if seed, err := seedState(*importDir); err != nil {
		fmt.Fprintf(c.errOut, "Not copying your settings: %v\n", err)
	} else if seed != nil {
		cfg.Seed = map[string][]byte{"state.json": seed}
	}
	fmt.Fprintf(c.out, "Installing the Lowpath service (%s)...\n", platform.ServiceName)
	if err := c.plat.Service.Install(cfg); err != nil {
		return err
	}
	st, _ := c.plat.Service.State()
	fmt.Fprintf(c.out, "Installed and started: %s\nSettings and log: %s\n", st.Exe, data)
	fmt.Fprintln(c.out, "Open Lowpath as usual: it uses the service and won't ask for administrator rights again.")
	return nil
}

func (c *cli) uninstall(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	fs.SetOutput(c.errOut)
	fs.BoolVar(&c.pause, "pause", false, "wait for Enter before exiting (used when restarting elevated)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !c.plat.Privileges.Elevated() {
		return c.elevate("uninstall", "-pause")
	}
	if err := c.plat.Service.Uninstall(); err != nil {
		return err
	}
	data, _ := c.plat.Paths.ServiceDataDir()
	fmt.Fprintf(c.out, "The Lowpath service is removed. Its settings stay in %s.\n", data)
	return nil
}

// elevate runs this command again with administrator rights (a UAC prompt
// on Windows), in a window of its own.
func (c *cli) elevate(args ...string) error {
	if !c.plat.Privileges.CanElevate() {
		return fmt.Errorf("%s needs administrator rights: run it as root (sudo) or from an administrator terminal", args[0])
	}
	if err := c.plat.Privileges.Relaunch(args); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "Asked for administrator rights; %s continues in the new window.\n", args[0])
	return nil
}

// maxSeed bounds the state file copied at install (it is a few KB).
const maxSeed = 4 << 20

// seedState reads <dir>/state.json for the service's first start; nil, nil
// when there is none.
func seedState(dir string) ([]byte, error) {
	if dir == "" {
		return nil, nil
	}
	f, err := os.Open(filepath.Join(dir, "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxSeed+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxSeed {
		return nil, fmt.Errorf("%s is larger than %d bytes", f.Name(), maxSeed)
	}
	return b, nil
}

// status prints what the service manager knows and whether the API
// answers. It needs no rights.
func (c *cli) status(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(c.errOut)
	pipe := fs.String("pipe", "", "check a service run by hand on this pipe or socket")
	if err := fs.Parse(args); err != nil {
		return err
	}
	api := c.plat.ServiceAPI
	if *pipe != "" && *pipe != api.String() {
		api = platform.ServiceAPIAt(*pipe)
	} else {
		st, err := c.plat.Service.State()
		switch {
		case errors.Is(err, errors.ErrUnsupported):
			fmt.Fprintln(c.out, "Service manager: not supported on this system yet; checking the API only.")
		case err != nil:
			fmt.Fprintf(c.out, "Service manager: %v\n", err)
		case !st.Installed:
			fmt.Fprintln(c.out, "Not installed. Run: lowpathsvc install")
			return nil
		case st.Running:
			fmt.Fprintf(c.out, "Installed and running, process %d: %s\n", st.PID, st.Exe)
		default:
			fmt.Fprintf(c.out, "Installed, not running: %s (Lowpath starts it when it opens)\n", st.Exe)
			return nil
		}
	}
	fe, err := app.NewFrontend(app.FrontendConfig{Dial: api.Dial, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		return err
	}
	defer fe.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ver, err := fe.Check(ctx)
	if err != nil {
		return fmt.Errorf("API %s: %w", api.String(), err)
	}
	fmt.Fprintf(c.out, "API %s answers: Lowpath %s\n", api.String(), ver)
	if ver != app.Version {
		fmt.Fprintf(c.out, "This lowpathsvc is %s: reinstall to update the service.\n", app.Version)
	}
	return nil
}
