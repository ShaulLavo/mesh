package cli

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/sshd"
	"golang.org/x/crypto/ssh"
)

type setupPeer struct {
	holdOpen    atomic.Bool
	openGate    chan struct{}
	execGate    chan struct{}
	holdGate    chan struct{}
	opens       chan struct{}
	execs       chan string
	effects     atomic.Int32
	cleaned     atomic.Int32
	cleanupSeen chan struct{}
}

func (p *setupPeer) channel(ctx context.Context, incoming ssh.NewChannel) {
	if p.holdOpen.Load() {
		p.opens <- struct{}{}
		select {
		case <-p.openGate:
		case <-ctx.Done():
			return
		}
	}
	channel, requests, err := incoming.Accept()
	if err != nil {
		return
	}
	defer channel.Close() //nolint:errcheck // test channel lifecycle
	request, ok := <-requests
	if !ok {
		return
	}
	var payload struct{ Command string }
	var run sshd.RunRequest
	if request.Type != "exec" || ssh.Unmarshal(request.Payload, &payload) != nil {
		return
	}
	if json.Unmarshal([]byte(strings.TrimPrefix(payload.Command, "run ")), &run) != nil || len(run.Args) != 1 {
		return
	}
	closed := make(chan struct{})
	go func() {
		for range requests {
		}
		close(closed)
	}()
	defer func() { _ = channel.Close(); <-closed }()
	p.execs <- run.Args[0]
	p.execute(ctx, channel, request, run.Args[0], closed)
}

func (p *setupPeer) execute(ctx context.Context, channel ssh.Channel, request *ssh.Request, name string, closed <-chan struct{}) {
	if name == "cancel" {
		// An exec may have run before its acknowledgement. It is never replayed.
		p.effects.Add(1)
		select {
		case <-p.execGate:
		case <-closed:
			p.cleaned.Add(1)
			p.cleanupSeen <- struct{}{}
			return
		case <-ctx.Done():
			return
		}
	}
	if request.Reply(true, nil) != nil {
		return
	}
	_, _ = channel.Write([]byte("before"))
	if name == "hold" {
		select {
		case <-p.holdGate:
		case <-closed:
			return
		case <-ctx.Done():
			return
		}
	}
	_, _ = channel.Write([]byte("after"))
	_, _ = channel.Stderr().Write([]byte("error"))
	_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{}))
}

type boundaryResult struct {
	result sshHelperResult
	stdout string
	stderr string
}

func startBoundaryCommand(ctx context.Context, t *testing.T, host HostRecord, port uint16, name string, commands *sync.WaitGroup) <-chan boundaryResult {
	t.Helper()
	files := make([]*os.File, 0, 3)
	for range 3 {
		file, err := os.CreateTemp(t.TempDir(), "stream")
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	result := make(chan boundaryResult, 1)
	command := NewCommand(Dependencies{Stdin: files[0], Stdout: files[1], Stderr: files[2]}) //nolint:contextcheck // Construction has no context parameter; ExecuteContext supplies it below.
	command.SetArgs([]string{"ssh", "--ssh-port", strconv.Itoa(int(port)), host.MeshIdentity, "-T", "--", name})
	commands.Go(func() {
		value := sshHelperResult{}
		if err := command.ExecuteContext(ctx); err != nil {
			value.Code = 1
			value.Error = err.Error()
		}
		out, _ := os.ReadFile(files[1].Name())
		stderr, _ := os.ReadFile(files[2].Name())
		closeSSHFiles(files)
		result <- boundaryResult{result: value, stdout: string(out), stderr: string(stderr)}
	})
	return result
}

func awaitBoundaryResult(t *testing.T, result <-chan boundaryResult) boundaryResult {
	t.Helper()
	select {
	case value := <-result:
		return value
	case <-time.After(time.Minute):
		t.Fatal("SSH setup outcome exceeded hang cap")
		return boundaryResult{}
	}
}

func awaitBoundaryStage(t *testing.T, peer *setupPeer, opening bool, name string) {
	t.Helper()
	if opening {
		select {
		case <-peer.opens:
			return
		case <-time.After(time.Minute):
			t.Fatal("channel-open barrier was not reached")
		}
	}
	select {
	case value := <-peer.execs:
		if value != name {
			t.Fatalf("exec barrier=%q, want %q", value, name)
		}
	case <-time.After(time.Minute):
		t.Fatal("exec barrier was not reached")
	}
}

func TestSSHSetupCancellationBarriers(t *testing.T) {
	for _, mode := range []string{"open-late", "open-never", "exec-uncertain"} {
		t.Run(mode, func(t *testing.T) { assertSSHSetupCancellation(t, mode) })
	}
}

func assertSSHSetupCancellation(t *testing.T, mode string) {
	peer := &setupPeer{openGate: make(chan struct{}), execGate: make(chan struct{}),
		holdGate: make(chan struct{}), opens: make(chan struct{}, sshPoolChannels), execs: make(chan string, sshPoolChannels+2), cleanupSeen: make(chan struct{}, 1)}
	host, port := poolTestServerWith(t, peer.channel)
	state := compactSocketTempDir(t)
	if _, _, err := identity.LoadOrCreate(state); err != nil {
		t.Fatal(err)
	}
	pool := &sshCommandPool{ctx: t.Context(), entries: make(map[sshPoolKey]*sshPoolEntry), dial: dialMeshSSHWithKey}
	stopOwner := boundaryOwner(t, pool, host, state)
	var commands sync.WaitGroup
	t.Cleanup(func() {
		stopOwner()
		pool.connecting.Wait()
		pool.expire(time.Now(), true)
		commands.Wait()
		pool.setups.Wait()
		pool.lifetimes.Wait()
	})
	sibling := startBoundaryCommand(t.Context(), t, host, port, "hold", &commands)
	awaitBoundaryStage(t, peer, false, "hold")
	opening := strings.HasPrefix(mode, "open-")
	cancelAtBarrier(t, peer, host, port, opening, &commands)
	awaitSSHPoolUsers(t, pool, 1)
	peer.holdOpen.Store(false)
	small := startBoundaryCommand(t.Context(), t, host, port, "sibling", &commands)
	assertBoundarySuccess(t, awaitBoundaryResult(t, small))
	if opening {
		fillAbandonedOpenBudget(t, peer, pool, host, state, port, &commands)
	}
	pool.mu.Lock()
	live := pool.live
	pool.mu.Unlock()
	if live != 1 {
		t.Fatalf("cancellation closed the sibling transport: live=%d", live)
	}
	if mode == "open-late" {
		close(peer.openGate)
	}
	if mode == "exec-uncertain" {
		select {
		case <-peer.cleanupSeen:
		case <-time.After(time.Minute):
			t.Fatal("uncertain exec did not clean up")
		}
		close(peer.execGate)
	}
	close(peer.holdGate)
	assertBoundarySuccess(t, awaitBoundaryResult(t, sibling))
	pool.expire(time.Now().Add(2*sshPoolIdle), false)
	awaitBoundaryCleanup(t, pool)
	want := int32(0)
	if !opening {
		want = 1
	}
	if peer.effects.Load() != want {
		t.Fatalf("late acceptance/replay effects=%d, want %d", peer.effects.Load(), want)
	}
	if !opening && peer.cleaned.Load() != 1 {
		t.Fatal("uncertain exec did not observe channel cleanup")
	}
}

func cancelAtBarrier(t *testing.T, peer *setupPeer, host HostRecord, port uint16, opening bool, commands *sync.WaitGroup) {
	t.Helper()
	peer.holdOpen.Store(opening)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := startBoundaryCommand(ctx, t, host, port, "cancel", commands)
	awaitBoundaryStage(t, peer, opening, "cancel")
	cancel()
	value := awaitBoundaryResult(t, result)
	if value.result.Code == 0 || value.result.Error == "" {
		t.Fatalf("cancelled setup succeeded: %+v", value)
	}
	// Neither withheld confirmation gate has been released at this point.
}

func fillAbandonedOpenBudget(t *testing.T, peer *setupPeer, pool *sshCommandPool, host HostRecord, state string, port uint16, commands *sync.WaitGroup) {
	t.Helper()
	for range sshPoolChannels - 2 {
		cancelAtBarrier(t, peer, host, port, true, commands)
		awaitSSHPoolUsers(t, pool, 1)
	}
	if _, _, err := pool.acquire(t.Context(), host, state, port); err == nil {
		t.Fatal("abandoned opens escaped the per-host bound")
	}
	pool.mu.Lock()
	pending := pool.pendingSetups
	pool.mu.Unlock()
	if pending != sshPoolChannels-1 {
		t.Fatalf("pending setup accounting=%d", pending)
	}
}

func boundaryOwner(t *testing.T, pool *sshCommandPool, host HostRecord, state string) func() {
	t.Helper()
	writeBoundaryHosts(t, state, host)
	return boundaryOwnerListener(t, pool, state)
}

func writeBoundaryHosts(t *testing.T, state string, hosts ...HostRecord) {
	t.Helper()
	config := t.TempDir()
	t.Setenv("MESH_CONFIG_DIR", config)
	t.Setenv("MESH_STATE_DIR", state)
	records := make([]HostRecord, 0, len(hosts))
	for _, host := range hosts {
		host.ID, host.Addresses = host.MeshIdentity, []string{"127.0.0.1"}
		records = append(records, host)
	}
	data, err := json.Marshal(map[string]any{"version": 1, "hosts": records})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "hosts.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func boundaryOwnerListener(t *testing.T, pool *sshCommandPool, state string) func() {
	t.Helper()
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(state, "ssh-command.sock"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	slots := make(chan struct{}, 32)
	var handlers sync.WaitGroup
	var last atomic.Int64
	done := make(chan struct{})
	go acceptBoundaryIPC(ctx, listener, pool, state, slots, &handlers, &last, done)
	return func() { cancel(); _ = listener.Close(); <-done; handlers.Wait() }
}

func acceptBoundaryIPC(ctx context.Context, listener *net.UnixListener, pool *sshCommandPool, state string, slots chan struct{}, handlers *sync.WaitGroup, last *atomic.Int64, done chan<- struct{}) {
	defer close(done)
	for {
		conn, err := listener.AcceptUnix()
		if err != nil {
			return
		}
		admitSSHHelper(ctx, conn, state, pool, slots, handlers, last)
	}
}

func assertBoundarySuccess(t *testing.T, value boundaryResult) {
	t.Helper()
	if value.result.Code != 0 || value.result.Error != "" || value.stdout != "beforeafter" || value.stderr != "error" {
		t.Fatalf("sibling I/O changed: %+v", value)
	}
}

func awaitBoundaryCleanup(t *testing.T, pool *sshCommandPool) {
	t.Helper()
	done := make(chan struct{})
	go func() { pool.setups.Wait(); pool.lifetimes.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Minute):
		t.Fatal("pending SSH setup was not reclaimed")
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.pendingSetups != 0 || pool.live != 0 {
		t.Fatalf("retained setups=%d transports=%d", pool.pendingSetups, pool.live)
	}
}

func TestSSHSetupAggregateBudget(t *testing.T) {
	pool := &sshCommandPool{}
	for range sshPoolSetups {
		if err := pool.beginSetup(); err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.beginSetup(); err == nil {
		t.Fatal("unbounded global pending setup budget")
	}
	for range sshPoolSetups {
		pool.endSetup()
	}
	if pool.pendingSetups != 0 {
		t.Fatal("global setup budget leaked")
	}
}
