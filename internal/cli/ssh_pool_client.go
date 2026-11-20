package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/shaul/mesh/internal/sshd"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

func dialSSHOwner(ctx context.Context, state string) (*net.UnixConn, error) {
	path := filepath.Join(state, "ssh-command.sock")
	if conn, err := connectSSHOwner(path); err == nil {
		return conn, nil
	}
	lock, err := openSSHOwnerLock(state, "ssh-start.lock")
	if err != nil {
		return nil, err
	}
	defer lock.Close() //nolint:errcheck // closing releases the startup lock
	if err := acquireClientConfigLock(ctx, lock); err != nil {
		return nil, err
	}
	if conn, err := connectSSHOwner(path); err == nil {
		return conn, nil
	}
	if err := startSSHOwnerProcess(ctx); err != nil {
		return nil, err
	}
	return connectSSHOwner(path)
}

// startSSHOwnerProcess is replaced in tests, which cannot run the helper as a
// separate executable.
var startSSHOwnerProcess = startSSHOwner

func connectSSHOwner(path string) (*net.UnixConn, error) {
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("dial SSH command socket: %w", err)
	}
	if err := validateSSHPeer(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

func startSSHOwner(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate SSH owner executable: %w", err)
	}
	read, write, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("prepare SSH owner readiness: %w", err)
	}
	defer read.Close()                                //nolint:errcheck // readiness only
	defer write.Close()                               //nolint:errcheck // child owns its duplicate
	command := exec.Command(executable, "ssh-helper") //nolint:gosec // our current executable
	command.ExtraFiles = []*os.File{write}
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return fmt.Errorf("start SSH connection owner: %w", err)
	}
	go func() { _ = command.Wait() }()
	_ = write.Close()
	stop := context.AfterFunc(ctx, func() { _ = read.Close(); _ = command.Process.Kill() })
	defer stop()
	if _, err := io.ReadFull(read, make([]byte, 1)); err != nil {
		return fmt.Errorf("SSH connection owner did not start: %w", err)
	}
	return nil
}

type sshCommandPipes struct {
	files  []*os.File
	remote []*os.File
	input  *os.File
	output *os.File
	stderr *os.File
}

func newSSHCommandPipes() (*sshCommandPipes, error) {
	p := &sshCommandPipes{}
	for range 3 {
		read, write, err := os.Pipe()
		if err != nil {
			closeSSHFiles(p.files)
			return nil, fmt.Errorf("prepare private SSH stream: %w", err)
		}
		p.files = append(p.files, read, write)
	}
	p.remote = []*os.File{p.files[0], p.files[3], p.files[5]}
	p.input, p.output, p.stderr = p.files[1], p.files[2], p.files[4]
	return p, nil
}

func (p *sshCommandPipes) send(conn *net.UnixConn, request sshHelperRequest) error {
	rights := unix.UnixRights(int(p.remote[0].Fd()), int(p.remote[1].Fd()), int(p.remote[2].Fd()))
	if _, _, err := conn.WriteMsgUnix([]byte{1}, rights, nil); err != nil {
		return fmt.Errorf("send SSH command streams: %w", err)
	}
	closeSSHFiles(p.remote)
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return fmt.Errorf("send SSH launch request: %w", err)
	}
	return nil
}

type sshPoolOutcome int

const (
	// sshPoolFinished: the helper accepted the command, or it failed for good.
	sshPoolFinished sshPoolOutcome = iota
	// sshPoolInterrupted: the helper went away before accepting; nothing ran.
	sshPoolInterrupted
	// sshPoolUnavailable: no helper, or a full one; nothing ran.
	sshPoolUnavailable
)

// runPipeSSH prefers the shared connection owner. A helper that ends before
// accepting, typically one idling out, gets exactly one more try. A full or
// unreachable helper never blocks a command: it runs on its own connection.
func (a *application) runPipeSSH(ctx context.Context, host HostRecord, state string, port uint16, request sshd.RunRequest) error {
	payload, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("encode SSH launch: %w", err)
	}
	if _, err := sshd.ParseRunRequest(payload); err != nil {
		return fmt.Errorf("validate SSH launch: %w", err)
	}
	for range 2 {
		outcome, err := a.tryPooledSSH(ctx, host, state, port, payload)
		if outcome == sshPoolFinished {
			return err
		}
		if outcome == sshPoolUnavailable {
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("SSH command canceled: %w", err)
	}
	return a.runDirectPipeSSH(ctx, host, state, port, request)
}

func (a *application) tryPooledSSH(ctx context.Context, host HostRecord, state string, port uint16, payload []byte) (sshPoolOutcome, error) {
	conn, err := dialSSHOwner(ctx, state)
	if err != nil {
		return sshPoolUnavailable, fmt.Errorf("connect local SSH owner: %w", err)
	}
	defer conn.Close() //nolint:errcheck // result owns exit status
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	pipes, err := newSSHCommandPipes()
	if err != nil {
		return sshPoolFinished, err
	}
	defer closeSSHFiles(pipes.files)
	if err := pipes.send(conn, sshHelperRequest{Host: host, Port: port, Run: payload}); err != nil {
		return sshPoolInterrupted, err
	}
	status := make([]byte, 1)
	_, err = io.ReadFull(conn, status)
	if ctx.Err() != nil {
		return sshPoolFinished, fmt.Errorf("SSH command canceled: %w", ctx.Err())
	}
	if err != nil {
		return sshPoolInterrupted, fmt.Errorf("SSH connection owner ended before accepting: %w", err)
	}
	switch status[0] {
	case sshOwnerAccepted:
		return sshPoolFinished, a.relayPooledSSH(ctx, conn, pipes)
	case sshOwnerFallback:
		return sshPoolUnavailable, nil
	default:
		return sshPoolFinished, fmt.Errorf("SSH connection owner sent unknown status %q", status[0])
	}
}

// runDirectPipeSSH runs one pipe-mode command on its own connection, with the
// same relay the shared owner uses, as when no owner can take it.
func (a *application) runDirectPipeSSH(ctx context.Context, host HostRecord, state string, port uint16, request sshd.RunRequest) error {
	client, err := dialMeshSSH(ctx, host, state, port)
	if err != nil {
		return err
	}
	// A private pool only accounts for this command's setup operations.
	pool := &sshCommandPool{}
	defer func() { _ = client.Close(); pool.setups.Wait() }()
	pipes, err := newSSHCommandPipes()
	if err != nil {
		return err
	}
	defer closeSSHFiles(pipes.files)
	runCtx, cancel := context.WithCancel(ctx)
	signals := make(chan ssh.Signal, 4)
	results := make(chan sshHelperResult, 1)
	var running sync.WaitGroup
	defer func() { cancel(); running.Wait() }()
	running.Go(func() {
		lease := &sshCommandLease{Client: client, pool: pool, entry: &sshPoolEntry{}}
		results <- runDirectSSHCommand(runCtx, lease, request, pipes.remote, signals)
	})
	return a.relaySSHPipes(ctx, pipes, sshPipeOwner{
		result: func() (sshHelperResult, error) { return <-results, nil },
		signal: func(value ssh.Signal) {
			select {
			case signals <- value:
			default:
			}
		},
		abort: cancel,
	})
}

func runDirectSSHCommand(ctx context.Context, lease *sshCommandLease, request sshd.RunRequest, files []*os.File, signals <-chan ssh.Signal) sshHelperResult {
	// Closing our ends gives the local relays end of output.
	defer closeSSHFiles(files)
	channel, err := openSSHPipeChannel(ctx, lease)
	if err != nil {
		return sshHelperResult{Code: 1, Error: err.Error()}
	}
	return runSSHPipes(ctx, lease, channel, request, files, signals)
}

// sshPipeOwner is whoever runs the SSH side of a pipe-mode command: the shared
// helper over its socket, or this process on a direct connection.
type sshPipeOwner struct {
	result func() (sshHelperResult, error)
	signal func(ssh.Signal)
	abort  func()
}

func (a *application) relayPooledSSH(ctx context.Context, conn *net.UnixConn, pipes *sshCommandPipes) error {
	encoder := json.NewEncoder(conn)
	return a.relaySSHPipes(ctx, pipes, sshPipeOwner{
		result: func() (sshHelperResult, error) {
			var result sshHelperResult
			if err := json.NewDecoder(io.LimitReader(conn, 64<<10)).Decode(&result); err != nil {
				return result, fmt.Errorf("read SSH command result: %w", err)
			}
			return result, nil
		},
		signal: func(value ssh.Signal) { _ = encoder.Encode(value) },
		abort:  func() { _ = conn.Close() },
	})
}

func (a *application) relaySSHPipes(parent context.Context, pipes *sshCommandPipes, owner sshPipeOwner) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	input, err := localAttachInput(a.dependencies.Stdin, term.IsTerminal(a.dependencies.Stdin.Fd()))
	if err != nil {
		return err
	}
	defer input.close() //nolint:errcheck // cancellation owns input shutdown
	output, closeOutput, err := localAttachOutput(ctx, a.dependencies.Stdout)
	if err != nil {
		return err
	}
	defer closeOutput()
	stderr, closeError, err := localAttachOutput(ctx, a.dependencies.Stderr)
	if err != nil {
		return err
	}
	defer closeError()
	var streams sync.WaitGroup
	streams.Go(func() { relaySSHOutput(owner.abort, output, pipes.output) })
	streams.Go(func() { relaySSHOutput(owner.abort, stderr, pipes.stderr) })
	inputDone := make(chan struct{})
	go func() { defer close(inputDone); _, _ = io.Copy(pipes.input, input.reader); _ = pipes.input.Close() }()
	defer func() { input.cancel(); _ = pipes.input.Close(); <-inputDone }()
	signalsDone := make(chan struct{})
	go func() { defer close(signalsDone); sendSSHSignals(ctx, owner.signal) }()
	defer func() { cancel(); <-signalsDone }()
	result, err := owner.result()
	if err != nil {
		cancel()
		closeSSHFiles(pipes.files)
	}
	streams.Wait()
	return pooledSSHResult(result, err)
}

func relaySSHOutput(abort func(), output io.Writer, input *os.File) {
	if _, err := io.Copy(output, input); err != nil {
		abort()
		_ = input.Close()
	}
}

func pooledSSHResult(result sshHelperResult, err error) error {
	if err != nil {
		return fmt.Errorf("SSH connection owner ended: %w", err)
	}
	if result.Error != "" {
		return errors.New(result.Error)
	}
	if result.Code != 0 {
		return statusError{code: result.Code}
	}
	return nil
}

func sendSSHSignals(ctx context.Context, send func(ssh.Signal)) {
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	for {
		select {
		case <-ctx.Done():
			return
		case value := <-signals:
			send(map[os.Signal]ssh.Signal{syscall.SIGINT: ssh.SIGINT, syscall.SIGTERM: ssh.SIGTERM, syscall.SIGHUP: ssh.SIGHUP}[value])
		}
	}
}
