package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/sshd"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

func sshHelperCommand() *cobra.Command {
	return &cobra.Command{Use: "ssh-helper", Hidden: true, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runSSHHelper(cmd.Context()) }}
}

// The status byte a helper sends once it has read a request. Before it, no
// command can have started, so a client may safely try again or connect
// directly; after sshOwnerAccepted the helper owns the outcome.
const (
	sshOwnerAccepted byte = 'A'
	sshOwnerFallback byte = 'F'
)

// sshOwnerLockWait stays inside the client's ten-second startup budget.
const sshOwnerLockWait = 8 * time.Second

func runSSHHelper(ctx context.Context) error {
	state, err := paths.StateDir()
	if err != nil {
		return fmt.Errorf("locate SSH owner state: %w", err)
	}
	return ownSSHHelper(ctx, state, signalSSHOwnerReady, nil)
}

// ownSSHHelper serves commands for state once it holds the owner lock. busy,
// when set, runs once if another owner still holds the lock.
func ownSSHHelper(ctx context.Context, state string, ready, busy func()) error {
	lock, err := openSSHOwnerLock(state, "ssh-owner.lock")
	if err != nil {
		return err
	}
	defer lock.Close() //nolint:errcheck // dropping the descriptor releases ownership
	if err := waitSSHOwnerLock(ctx, lock, busy); err != nil {
		return err
	}
	path := filepath.Join(state, "ssh-command.sock")
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove inactive SSH owner socket: %w", err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return fmt.Errorf("listen for SSH commands: %w", err)
	}
	defer listener.Close() //nolint:errcheck // listener removes only its own socket
	if err := os.Chmod(path, 0600); err != nil {
		return fmt.Errorf("protect SSH command socket: %w", err)
	}
	ready()
	return serveSSHHelper(ctx, listener, state)
}

// An owner that is idling out holds its lock until its connections close.
// A new owner started in that gap waits for it instead of failing.
func waitSSHOwnerLock(ctx context.Context, lock *os.File, busy func()) error {
	deadline := time.Now().Add(sshOwnerLockWait)
	for {
		err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if !errors.Is(err, unix.EWOULDBLOCK) {
			return wrapSSHOwnerLock(err)
		}
		if busy != nil {
			busy()
			busy = nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("SSH connection owner is already running: %w", err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for SSH connection owner lock: %w", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func wrapSSHOwnerLock(err error) error {
	if err != nil {
		return fmt.Errorf("lock SSH connection owner: %w", err)
	}
	return nil
}

// The starting client passes a pipe as descriptor 3. Anything else there, such
// as a file opened by a hand-run helper, is left alone.
func signalSSHOwnerReady() {
	var stat unix.Stat_t
	if unix.Fstat(3, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFIFO {
		return
	}
	ready := os.NewFile(3, "ssh-owner-ready")
	_, _ = ready.Write([]byte{1})
	_ = ready.Close()
}

func serveSSHHelper(ctx context.Context, listener *net.UnixListener, state string) error {
	ctx, cancel := context.WithCancel(ctx)
	pool := &sshCommandPool{ctx: ctx, entries: make(map[sshPoolKey]*sshPoolEntry), dial: dialMeshSSHWithKey}
	var handlers sync.WaitGroup
	defer func() {
		cancel()
		handlers.Wait()
		pool.connecting.Wait()
		pool.expire(time.Now(), true)
		pool.setups.Wait()
		pool.lifetimes.Wait()
	}()
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()
	slots := make(chan struct{}, 32)
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	for {
		pool.expire(time.Now(), false)
		if len(slots) == 0 && time.Since(time.Unix(0, last.Load())) >= sshPoolIdle {
			return nil
		}
		_ = listener.SetDeadline(time.Now().Add(time.Second))
		conn, err := listener.AcceptUnix()
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			continue
		}
		if err != nil {
			return fmt.Errorf("accept local SSH command: %w", err)
		}
		last.Store(time.Now().UnixNano())
		admitSSHHelper(ctx, conn, state, pool, slots, &handlers, &last)
	}
}

func admitSSHHelper(ctx context.Context, conn *net.UnixConn, state string, pool *sshCommandPool, slots chan struct{}, handlers *sync.WaitGroup, last *atomic.Int64) {
	if err := validateSSHPeer(conn); err != nil {
		_ = conn.Close()
		return
	}
	select {
	case slots <- struct{}{}:
		handlers.Go(func() { serveSSHHelperConn(ctx, conn, state, pool, slots, last) })
	default:
		// Every slot holds a running command; this one runs on its own connection.
		handlers.Go(func() { refuseSSHHelper(conn) })
	}
}

// refuseSSHHelper reads the request before answering. Closing a socket with
// unread data resets it, and the client would lose the fallback answer.
func refuseSSHHelper(conn *net.UnixConn) {
	defer conn.Close() //nolint:errcheck // refusal is best effort
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	files, err := receiveSSHFiles(conn)
	closeSSHFiles(files)
	var request sshHelperRequest
	if err == nil {
		_ = json.NewDecoder(io.LimitReader(conn, 300<<10)).Decode(&request)
	}
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, _ = conn.Write([]byte{sshOwnerFallback})
}

func serveSSHHelperConn(ctx context.Context, conn *net.UnixConn, state string, pool *sshCommandPool, slots chan struct{}, last *atomic.Int64) {
	defer func() { _ = conn.Close(); last.Store(time.Now().UnixNano()); <-slots }()
	accepted := false
	accept := func() {
		if !accepted {
			accepted = true
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			_, _ = conn.Write([]byte{sshOwnerAccepted})
		}
	}
	result := handleSSHHelper(ctx, conn, state, pool, accept)
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if result.fallback {
		_, _ = conn.Write([]byte{sshOwnerFallback})
		return
	}
	accept()
	_ = json.NewEncoder(conn).Encode(result)
}

// sshControlBufferSize covers the largest control message a sender can pass.
// Darwin installs every received descriptor before it truncates the control
// copy, so a buffer sized for exactly three would leak any extra descriptors
// into this process with no way to close them. Darwin rejects control data
// over MCLBYTES (2048) at send time; Linux closes what does not fit.
const sshControlBufferSize = 2048

func receiveSSHFiles(conn *net.UnixConn) ([]*os.File, error) {
	control := make([]byte, sshControlBufferSize)
	_, count, flags, _, err := conn.ReadMsgUnix(make([]byte, 1), control)
	if err != nil {
		return nil, fmt.Errorf("receive SSH stream descriptors: %w", err)
	}
	messages, err := unix.ParseSocketControlMessage(control[:count])
	if err != nil {
		return nil, fmt.Errorf("parse SSH stream descriptors: %w", err)
	}
	// Collect rights from every message before rejecting anything, so a bad
	// message never strands descriptors that arrived in a later one.
	var files []*os.File
	var parseErr error
	for _, message := range messages {
		rights, err := unix.ParseUnixRights(&message)
		if err != nil {
			parseErr = err
			continue
		}
		files = appendSSHFiles(files, rights)
	}
	if parseErr != nil {
		closeSSHFiles(files)
		return nil, fmt.Errorf("parse SSH stream rights: %w", parseErr)
	}
	if len(files) != 3 || flags&unix.MSG_CTRUNC != 0 {
		closeSSHFiles(files)
		return nil, errors.New("SSH connection owner requires three private pipe descriptors")
	}
	if err := validateSSHFiles(files); err != nil {
		closeSSHFiles(files)
		return nil, err
	}
	return files, nil
}

func validateSSHFiles(files []*os.File) error {
	for index, file := range files {
		if err := validateSSHFile(file, index == 0); err != nil {
			return err
		}
	}
	return nil
}

func validateSSHFile(file *os.File, input bool) error {
	raw, err := file.SyscallConn()
	if err != nil {
		return fmt.Errorf("inspect SSH stream: %w", err)
	}
	var stat unix.Stat_t
	var flags int
	var inspectErr error
	err = raw.Control(func(fd uintptr) {
		inspectErr = unix.Fstat(int(fd), &stat)
		if inspectErr == nil {
			flags, inspectErr = unix.FcntlInt(fd, unix.F_GETFL, 0)
		}
	})
	if err != nil || inspectErr != nil {
		return errors.Join(err, inspectErr)
	}
	mode := unix.O_WRONLY
	if input {
		mode = unix.O_RDONLY
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFIFO || flags&unix.O_ACCMODE != mode {
		return errors.New("SSH streams must be private pipes with stdin read-only and outputs write-only")
	}
	return nil
}

func openSSHOwnerLock(state, name string) (*os.File, error) {
	directory, err := openClientConfigDirectory(state, false)
	if err != nil {
		return nil, err
	}
	if directory == nil {
		return nil, os.ErrNotExist
	}
	defer directory.Close() //nolint:errcheck // lock has an independent descriptor
	return openClientConfigFile(directory, name, unix.O_CREAT|unix.O_RDWR, 0600)
}

func appendSSHFiles(files []*os.File, rights []int) []*os.File {
	for _, fd := range rights {
		unix.CloseOnExec(fd)
		// These descriptors are private pipes, never the caller's terminal or files.
		_ = unix.SetNonblock(fd, true)
		files = append(files, os.NewFile(uintptr(fd), "ssh-command-stream"))
	}
	return files
}

func closeSSHFiles(files []*os.File) {
	for _, file := range files {
		_ = file.Close()
	}
}

func handleSSHHelper(parent context.Context, conn *net.UnixConn, state string, pool *sshCommandPool, accept func()) sshHelperResult {
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	files, err := receiveSSHFiles(conn)
	if err != nil {
		return sshHelperResult{Code: 1, Error: err.Error()}
	}
	defer closeSSHFiles(files)
	var request sshHelperRequest
	decoder := json.NewDecoder(io.LimitReader(conn, 300<<10))
	if err := decoder.Decode(&request); err != nil {
		return sshHelperResult{Code: 1, Error: err.Error()}
	}
	run, err := sshd.ParseRunRequest(request.Run)
	if err != nil || request.Port == 0 {
		return sshHelperResult{Code: 1, Error: fmt.Sprintf("invalid SSH launch request: %v", err)}
	}
	ticket, err := pool.admit(request.Host, state, request.Port) //nolint:contextcheck // The dial belongs to the pool, independently of this command.
	if errors.Is(err, errSSHPoolFull) {
		return sshHelperResult{fallback: true}
	}
	accept()
	if err != nil {
		return sshHelperResult{Code: 1, Error: err.Error()}
	}
	_ = conn.SetReadDeadline(time.Time{})
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	signals := make(chan ssh.Signal, 4)
	controlDone := make(chan struct{})
	go func() { defer close(controlDone); readSSHSignals(decoder, signals, cancel) }()
	defer func() { _ = conn.CloseRead(); <-controlDone }()
	stop := context.AfterFunc(ctx, func() { closeSSHFiles(files) })
	defer stop()
	lease, release, err := ticket.wait(ctx)
	if err != nil {
		return sshHelperResult{Code: 1, Error: err.Error()}
	}
	channel, err := openSSHPipeChannel(ctx, lease)
	if sshTransportLost(ctx, err) {
		lease, release, channel, err = reopenSSHPipeChannel(ctx, pool, lease, release, request, state)
	}
	if release != nil {
		defer release()
	}
	if err != nil {
		return sshHelperResult{Code: 1, Error: err.Error()}
	}
	return runSSHPipes(ctx, lease, channel, run, files, signals)
}

func openSSHPipeChannel(ctx context.Context, lease *sshCommandLease) (*ssh.Session, error) {
	channel, err := lease.setup(ctx, lease.NewSession)
	if err != nil {
		return nil, fmt.Errorf("open SSH channel: %w", err)
	}
	return channel, nil
}

// sshTransportLost reports a channel open that failed because the shared
// transport is gone, rather than a refusal from the destination. No exec was
// sent, so one retry on a fresh transport cannot run the command twice.
func sshTransportLost(ctx context.Context, err error) bool {
	var refused *ssh.OpenChannelError
	return err != nil && ctx.Err() == nil && !errors.As(err, &refused) && !errors.Is(err, errSSHPoolFull)
}

func reopenSSHPipeChannel(ctx context.Context, pool *sshCommandPool, lease *sshCommandLease, release func(), request sshHelperRequest, state string) (*sshCommandLease, func(), *ssh.Session, error) {
	pool.forget(lease.key, lease.entry)
	release()
	fresh, release, err := pool.acquire(ctx, request.Host, state, request.Port)
	if err != nil {
		return nil, nil, nil, err
	}
	channel, err := openSSHPipeChannel(ctx, fresh)
	return fresh, release, channel, err
}

func readSSHSignals(decoder *json.Decoder, signals chan<- ssh.Signal, cancel context.CancelFunc) {
	defer cancel()
	for {
		var value ssh.Signal
		if decoder.Decode(&value) != nil {
			return
		}
		select {
		case signals <- value:
		default:
		}
	}
}

func runSSHPipes(ctx context.Context, client *sshCommandLease, channel *ssh.Session, request sshd.RunRequest, files []*os.File, signals <-chan ssh.Signal) sshHelperResult {
	defer channel.Close() //nolint:errcheck // Wait supplies the result
	if err := ctx.Err(); err != nil {
		return sshHelperResult{Code: 1, Error: err.Error()}
	}
	stop := context.AfterFunc(ctx, func() { _ = channel.Close() })
	defer stop()
	input, err := channel.StdinPipe()
	if err != nil {
		return sshHelperResult{Code: 1, Error: err.Error()}
	}
	channel.Stdout, channel.Stderr = files[1], files[2]
	payload, _ := json.Marshal(request)
	if _, err := client.setup(ctx, func() (*ssh.Session, error) { return startSSHPipe(ctx, channel, payload) }); err != nil {
		return sshHelperResult{Code: 1, Error: fmt.Sprintf("start SSH command: %v", err)}
	}
	inputDone := make(chan struct{})
	go func() { defer close(inputDone); _, _ = io.Copy(input, files[0]); _ = input.Close() }()
	defer func() { _ = files[0].Close(); <-inputDone }()
	done := make(chan struct{})
	relayDone := make(chan struct{})
	go func() { defer close(relayDone); relaySSHPipeSignals(channel, signals, done) }()
	err = channel.Wait()
	close(done)
	<-relayDone
	var exit *ssh.ExitError
	if errors.As(err, &exit) {
		return sshHelperResult{Code: exit.ExitStatus()}
	}
	if err != nil {
		return sshHelperResult{Code: 1, Error: fmt.Sprintf("SSH connection ended: %v", err)}
	}
	return sshHelperResult{}
}

func startSSHPipe(ctx context.Context, channel *ssh.Session, payload []byte) (*ssh.Session, error) {
	if err := ctx.Err(); err != nil {
		return channel, fmt.Errorf("SSH launch canceled: %w", err)
	}
	return channel, channel.Start("run " + string(payload)) //nolint:wrapcheck // caller adds the launch context
}

func relaySSHPipeSignals(channel *ssh.Session, signals <-chan ssh.Signal, done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		case value := <-signals:
			_ = channel.Signal(value)
		}
	}
}
