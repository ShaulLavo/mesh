package sshd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"

	charmssh "charm.land/ssh"
	"github.com/creack/pty"
	"github.com/shaul/mesh/internal/terminal"
	"github.com/shaul/mesh/internal/terminfo"
	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

// RunRequest starts an ordinary process owned by one SSH channel.
type RunRequest struct {
	Args     []string `json:"args,omitempty"`
	Terminfo []byte   `json:"terminfo,omitempty"`
}

func parseRun(raw string) (RunRequest, error) {
	var request RunRequest
	if len(raw) > 256<<10 {
		return request, errors.New("SSH command exceeds 256 KiB")
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(raw, "run ")), &request); err != nil {
		return request, fmt.Errorf("decode SSH command: %w", err)
	}
	for _, arg := range request.Args {
		if strings.ContainsRune(arg, 0) {
			return request, errors.New("SSH command contains a NUL byte")
		}
	}
	if len(request.Args) > 0 && request.Args[0] == "" {
		return request, errors.New("SSH executable is empty")
	}
	if len(request.Terminfo) > terminfo.MaximumBytes {
		return request, errors.New("SSH terminal definition is too large")
	}
	return request, nil
}

func runProcess(channel charmssh.Session, state *windowState, request RunRequest) (status int, result error) {
	var inputDone <-chan struct{}
	defer func() {
		finishSession(channel, status, result)
		_ = channel.Close()
		if inputDone != nil {
			<-inputDone
		}
	}()
	ctx := channel.Context()
	command, err := prepareProcess(ctx, state, request)
	if err != nil {
		return 1, err
	}
	input, writer, err := os.Pipe()
	if err != nil {
		return 1, fmt.Errorf("prepare SSH input: %w", err)
	}
	defer input.Close()  //nolint:errcheck // process result owns the exit status
	defer writer.Close() //nolint:errcheck // release relay on every exit
	var master *os.File
	if state != nil {
		size := state.size()
		master, err = pty.StartWithSize(command, &pty.Winsize{Cols: uint16(size.Cols), Rows: uint16(size.Rows)}) //nolint:gosec // dimensions were checked at the SSH boundary
	} else {
		raw := channel.Context().Value(channelKey{}).(gossh.Channel)
		command.Stdin, command.Stdout, command.Stderr = input, raw, raw.Stderr()
		err = command.Start()
	}
	if err != nil {
		return 1, fmt.Errorf("start SSH command: %w", err)
	}
	if master != nil {
		defer func() { _ = master.Close() }()
	}
	inputRelayDone := make(chan struct{})
	inputDone = inputRelayDone
	go func() { defer close(inputRelayDone); relayProcessInput(channel, writer, master) }()
	outputDone := make(chan struct{})
	if master != nil {
		go func() {
			defer close(outputDone)
			_, _ = io.Copy(channel.Context().Value(channelKey{}).(gossh.Channel), master)
		}()
	} else {
		close(outputDone)
	}
	finished := make(chan struct{})
	signals := make(chan charmssh.Signal, 16)
	channel.Signals(signals)
	defer channel.Signals(nil)
	completed := make(chan struct{})
	go watchProcess(ctx, channel, command, master, state, signals, completed, finished)
	err = command.Wait()
	close(completed)
	<-finished
	_ = writer.Close()
	<-outputDone
	return processResult(err)
}

func processResult(err error) (int, error) {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		status := exit.ExitCode()
		if status < 0 {
			status = 128 + int(exit.Sys().(syscall.WaitStatus).Signal())
		}
		return status, nil
	}
	if err != nil {
		return 1, fmt.Errorf("wait for SSH command: %w", err)
	}
	return 0, nil
}

func prepareProcess(ctx context.Context, state *windowState, request RunRequest) (*exec.Cmd, error) {
	args := request.Args
	if len(args) == 0 {
		shell := os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/sh"
		}
		args = []string{shell, "-l"}
	}
	command := exec.Command(args[0], args[1:]...) //nolint:gosec // an approved Mesh device explicitly requests this program
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("find SSH account home: %w", err)
	}
	command.Dir = home
	command.Env = os.Environ()
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if state != nil {
		command.Env = append(command.Env, "TERM="+state.term)
		if err := terminfo.Install(ctx, state.term, request.Terminfo); err != nil {
			return nil, fmt.Errorf("install SSH terminal definition: %w", err)
		}
	}
	return command, nil
}

func relayProcessInput(channel charmssh.Session, writer, master *os.File) {
	if master != nil {
		_, _ = io.Copy(master, channel)
		return
	}
	_, _ = io.Copy(writer, channel)
	_ = writer.Close()
}

func watchProcess(ctx context.Context, channel charmssh.Session, command *exec.Cmd, master *os.File, state *windowState, signals <-chan charmssh.Signal, completed <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	for {
		select {
		case <-completed:
			return
		default:
		}
		select {
		case <-completed:
			return
		case <-ctx.Done():
			_ = channel.Close()
			if command.Process.Signal(syscall.Signal(0)) == nil {
				killProcess(command.Process.Pid, master)
			}
			return
		case sig := <-signals:
			if value, ok := processSignals[sig]; ok && command.Process.Signal(syscall.Signal(0)) == nil {
				signalProcess(command.Process.Pid, master, value)
			}
		case _, open := <-windowChanges(state):
			if !open {
				state = nil
				continue
			}
			resizeProcess(master, state)
		}
	}
}

func resizeProcess(master *os.File, state *windowState) {
	if state == nil {
		return
	}
	size := state.size()
	_ = pty.Setsize(master, &pty.Winsize{Cols: uint16(size.Cols), Rows: uint16(size.Rows)}) //nolint:gosec // dimensions were checked at the SSH boundary
}

func windowChanges(state *windowState) <-chan terminal.Size {
	if state == nil {
		return nil
	}
	return state.changes
}

var processSignals = map[charmssh.Signal]syscall.Signal{
	"HUP": syscall.SIGHUP, "INT": syscall.SIGINT, "QUIT": syscall.SIGQUIT,
	"TERM": syscall.SIGTERM, "KILL": syscall.SIGKILL, "USR1": syscall.SIGUSR1, "USR2": syscall.SIGUSR2,
}

func signalProcess(pid int, master *os.File, signal syscall.Signal) {
	group := pid
	if master != nil {
		if foreground, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPGRP); err == nil && foreground > 1 {
			group = foreground
		}
	}
	_ = syscall.Kill(-group, signal)
}

func killProcess(pid int, master *os.File) {
	signalProcess(pid, master, syscall.SIGKILL)
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	if master != nil {
		_ = master.Close()
	}
}
