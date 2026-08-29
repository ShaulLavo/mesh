// Command mesh runs Mesh in each of its modes.
//
// This is the v0 skeleton: local persistent sessions only. Cobra and Fang
// arrive with the real CLI surface; there is no point dressing up four
// subcommands.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/shaul/mesh/internal/cli"
	meshdaemon "github.com/shaul/mesh/internal/daemon"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/worker"
)

const usage = `mesh - resumable terminal sessions

usage:
  mesh daemon [--tailnet-port PORT]     run this host's coordinator
  mesh local [-r] [--] [command...]   start a session, or resume the latest with -r
  mesh attach <id>                    attach to a session by id
  mesh ls                             list sessions on this host
  mesh logs <id>                      print a session's recent output
  mesh kill <id>                      terminate a session
  mesh sig <id> <signal>              send a signal (int, term, quit, hup, kill, usr1, usr2)

inside a session, ctrl+] detaches and leaves everything running.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "session-worker":
		err = runWorker(os.Args[2:])
	case "daemon":
		err = runDaemon(os.Args[2:])
	case "local":
		err = runLocal(os.Args[2:])
	case "attach":
		err = runAttach(os.Args[2:])
	case "ls", "list":
		err = runList()
	case "logs":
		err = runLogs(os.Args[2:])
	case "kill":
		err = runKill(os.Args[2:])
	case "sig", "signal":
		err = runSignal(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "mesh: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "mesh: %v\n", err)
		os.Exit(1)
	}
}

func runDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	tailnetPort := fs.Uint("tailnet-port", 0, "Tailnet WebSocket port (zero disables remote listening)")
	webSocketPath := fs.String("websocket-path", "/mesh", "Tailnet WebSocket path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: mesh daemon [--tailnet-port PORT]")
	}
	if *tailnetPort > 65535 {
		return fmt.Errorf("tailnet port %d is out of range", *tailnetPort)
	}
	stateDir, err := paths.StateDir()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return meshdaemon.Run(ctx, meshdaemon.Config{
		StateDir:      stateDir,
		TailnetPort:   uint16(*tailnetPort),
		WebSocketPath: *webSocketPath,
		ReportError: func(err error) {
			fmt.Fprintf(os.Stderr, "mesh daemon: %v\n", err)
		},
	})
}

func runWorker(args []string) error {
	fs := flag.NewFlagSet("session-worker", flag.ExitOnError)
	id := fs.String("id", "", "session id")
	dir := fs.String("dir", "", "session state directory")
	cols := fs.Int("cols", 80, "initial terminal width")
	rows := fs.Int("rows", 24, "initial terminal height")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" || *dir == "" {
		return fmt.Errorf("session-worker requires --id and --dir")
	}
	cwd, _ := os.Getwd()

	code, err := worker.Run(worker.Config{
		ID:                 *id,
		Dir:                *dir,
		Command:            fs.Args(),
		Cwd:                cwd,
		Env:                os.Environ(),
		Cols:               *cols,
		Rows:               *rows,
		AwaitInitialAttach: true,
	})
	if err != nil {
		return err
	}
	os.Exit(code)
	return nil
}

func runLocal(args []string) error {
	fs := flag.NewFlagSet("local", flag.ExitOnError)
	resume := fs.Bool("r", false, "resume the latest live session instead of starting a new one")
	fs.BoolVar(resume, "resume", false, "resume the latest live session instead of starting a new one")
	detachKey := fs.String("detach-key", "", "key that detaches, e.g. ctrl+] or none")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var s cli.Session
	var socketPath string
	var lastSeq *uint64
	var err error
	if *resume {
		s, err = cli.Latest()
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "resuming %s\n", s.ID)
	} else {
		command := fs.Args()
		if len(command) == 0 {
			command = []string{defaultShell()}
		}
		cwd, _ := os.Getwd()
		stateDir, stateErr := paths.StateDir()
		if stateErr != nil {
			return stateErr
		}
		id, createErr := cli.CreateViaDaemon(context.Background(), cli.DaemonCreateOptions{
			SocketPath: meshdaemon.SocketPath(stateDir),
			Command:    command,
			Cwd:        cwd,
			Cols:       80,
			Rows:       24,
		})
		switch {
		case createErr == nil:
			s = cli.Session{Meta: worker.Meta{ID: id}, Alive: true}
			socketPath = meshdaemon.SocketPath(stateDir)
		case errors.Is(createErr, cli.ErrDaemonUnavailable):
			s, err = cli.Spawn(command, cwd)
			if err != nil {
				return err
			}
		default:
			return createErr
		}
		// A newly spawned command may have produced all its output before the
		// readiness probe completes. Its first real attachment must start at
		// zero rather than asking for the normal bounded reattach tail.
		initialSeq := uint64(0)
		lastSeq = &initialSeq
		fmt.Fprintf(os.Stderr, "session %s\n", s.ID)
	}
	if socketPath == "" {
		return attach(s, *detachKey, lastSeq)
	}
	return attachAt(s, socketPath, *detachKey, lastSeq)
}

func runAttach(args []string) error {
	fs := flag.NewFlagSet("attach", flag.ExitOnError)
	detachKey := fs.String("detach-key", "", "key that detaches, e.g. ctrl+] or none")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: mesh attach <id>")
	}
	s, err := cli.Find(fs.Arg(0))
	if err != nil {
		return err
	}
	if !s.Alive {
		return fmt.Errorf("session %s is %s", s.ID, s.State())
	}
	return attach(s, *detachKey, nil)
}

func attach(s cli.Session, detachKey string, lastSeq *uint64) error {
	return attachAt(s, paths.Socket(s.Dir), detachKey, lastSeq)
}

func attachAt(s cli.Session, socketPath, detachKey string, lastSeq *uint64) error {
	key, raw, err := cli.ParseDetachKey(detachKey)
	if err != nil {
		return err
	}
	res, err := cli.Attach(cli.AttachOptions{
		SocketPath: socketPath,
		SessionID:  s.ID,
		LastSeq:    lastSeq,
		DetachKey:  key,
		Raw:        raw,
	})
	if err != nil {
		return err
	}
	switch {
	case res.Exited:
		fmt.Fprintf(os.Stderr, "\r\nsession %s exited (%d)\r\n", s.ID, res.ExitCode)
		if res.ExitCode != 0 {
			os.Exit(res.ExitCode)
		}
	case res.Detached:
		fmt.Fprintf(os.Stderr, "\r\ndetached from %s, still running\r\n", s.ID)
	default:
		fmt.Fprintf(os.Stderr, "\r\ndisconnected from %s\r\n", s.ID)
	}
	return nil
}

func runList() error {
	sessions, err := cli.List()
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		fmt.Println("no sessions on this host")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATE\tAGE\tCOMMAND")
	for _, s := range sessions {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			s.ID, s.State(), age(s.CreatedAt), strings.Join(s.Command, " "))
	}
	return w.Flush()
}

func runLogs(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: mesh logs <id>")
	}
	s, err := cli.Find(args[0])
	if err != nil {
		return err
	}
	b, err := os.ReadFile(paths.Log(s.Dir))
	if err != nil {
		return fmt.Errorf("read worker log: %w", err)
	}
	_, err = os.Stdout.Write(b)
	return err
}

func runKill(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: mesh kill <id>")
	}
	s, err := cli.Find(args[0])
	if err != nil {
		return err
	}
	if !s.Alive {
		return fmt.Errorf("session %s is already %s", s.ID, s.State())
	}
	if err := cli.Kill(s); err != nil {
		return err
	}
	fmt.Printf("killed %s\n", s.ID)
	return nil
}

func runSignal(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: mesh sig <id> <signal>")
	}
	s, err := cli.Find(args[0])
	if err != nil {
		return err
	}
	if err := cli.Signal(s, args[1]); err != nil {
		return err
	}
	fmt.Printf("sent %s to %s\n", args[1], s.ID)
	return nil
}

func defaultShell() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh
	}
	if path, err := exec.LookPath("bash"); err == nil {
		return path
	}
	return "/bin/sh"
}

func age(t time.Time) string {
	d := time.Since(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
