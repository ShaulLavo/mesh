package worker

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/agentresume"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
	"github.com/shaul/mesh/internal/session"
	terminalstate "github.com/shaul/mesh/internal/terminal"
)

func recoveryTestWorker(t *testing.T) *Worker {
	t.Helper()
	return recoveryTestWorkerAt(t, nil)
}

// recoveryTestWorkerAt installs the clock before the checkpoint loop starts
// reading it.
func recoveryTestWorkerAt(t *testing.T, now func() time.Time) *Worker {
	t.Helper()
	w := &Worker{
		now:    now,
		cfg:    Config{ID: "7K3D", HostID: "host-one", Dir: t.TempDir(), Cwd: "/launch", Command: []string{"/bin/bash"}},
		cmd:    &exec.Cmd{Process: &os.Process{Pid: 4321}},
		screen: terminalstate.NewScreen(80, 5), ring: session.NewRing(ringSize),
		observeProcess: func(fd, pid int) processObservation {
			if fd != -1 || pid != 4321 {
				t.Errorf("recovery observed foreground descriptor %d or wrong leader %d", fd, pid)
			}
			return processObservation{directory: "/observed-shell"}
		},
	}
	w.startRecovery()
	t.Cleanup(w.stopRecovery)
	return w
}

func TestShellRecoveryAcceptsOnlyLeaderAndAcknowledgesSavedDirectory(t *testing.T) {
	w := recoveryTestWorker(t)
	request := protocol.Control{Type: protocol.TypeShellUpdate, SessionID: w.cfg.ID,
		ShellPID: 4322, ShellDirectory: "/agent-child", ShellExecutable: "/bin/bash"}
	if response := inspectRequest(t, w, request); response.Type != protocol.TypeError {
		t.Fatalf("subprocess shell update accepted: %+v", response)
	}
	request.ShellPID = 4321
	request.ShellDirectory = "/saved-shell"
	if response := inspectRequest(t, w, request); response.Type != protocol.TypeShellUpdated {
		t.Fatalf("shell update failed: %+v", response)
	}
	w.mu.Lock()
	_, _ = w.screen.Write([]byte("\x1b]7;file:///agent-application\x07visible output"))
	w.mu.Unlock()
	if err := <-w.checkpoint(); err != nil {
		t.Fatal(err)
	}
	saved, err := recovery.Read(w.cfg.Dir)
	if err != nil || saved.ShellDirectory != "/saved-shell" || saved.DirectorySource != recovery.DirectoryShell {
		t.Fatalf("acknowledged shell directory = %+v, error = %v", saved, err)
	}
	if !strings.Contains(strings.Join(saved.Lines, "\n"), "visible output") {
		t.Fatalf("checkpoint lost output: %q", saved.Lines)
	}
}

func TestRecoveryCommandUpdatesAreDurableAndClearable(t *testing.T) {
	w := recoveryTestWorker(t)
	command := &recovery.Command{Argv: []string{"/usr/bin/env", "argument with spaces", "$(not executed)"}, Cwd: "/recipe"}
	request := protocol.Control{Type: protocol.TypeRecoveryCommand, SessionID: w.cfg.ID, RecoveryCommand: command}
	if response := inspectRequest(t, w, request); response.Type != protocol.TypeOK {
		t.Fatalf("recipe update: %+v", response)
	}
	saved, err := recovery.Read(w.cfg.Dir)
	if err != nil || saved.Restart == nil || saved.Restart.Argv[2] != "$(not executed)" {
		t.Fatalf("acknowledged recipe = %+v, error = %v", saved.Restart, err)
	}
	request.RecoveryCommand = nil
	request.ClearRecoveryCommand = true
	if response := inspectRequest(t, w, request); response.Type != protocol.TypeOK {
		t.Fatalf("recipe clear: %+v", response)
	}
	saved, err = recovery.Read(w.cfg.Dir)
	if err != nil || saved.Restart != nil {
		t.Fatalf("cleared recipe = %+v, error = %v", saved.Restart, err)
	}
}

func TestRemoteRecoveryHintSurvivesDisconnectedRegistration(t *testing.T) {
	w := recoveryTestWorker(t)
	client, server := net.Pipe()
	go w.serve(server)
	request := protocol.Control{Type: protocol.TypeNest, SessionID: w.cfg.ID,
		NestedSession: &protocol.SessionIdentity{HostID: "remote-host", SessionID: "8XYZ"}}
	if err := protocol.NewWriter(client).WriteControlMsg(request); err != nil {
		t.Fatal(err)
	}
	frame, err := protocol.NewReader(client).ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	response, err := protocol.DecodeControl(frame.Payload)
	if err != nil || response.Type != protocol.TypeNesting {
		t.Fatalf("nest registration = %+v, %v", response, err)
	}
	_ = client.Close()
	w.nestReaders.Wait()
	if err := <-w.checkpoint(); err != nil {
		t.Fatal(err)
	}
	saved, err := recovery.Read(w.cfg.Dir)
	if err != nil || saved.Remote == nil || saved.Remote.SessionID != "8XYZ" || saved.Remote.HostID != "remote-host" {
		t.Fatalf("saved exact remote hint = %+v, %v", saved.Remote, err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.nesting) != 0 {
		t.Fatal("saved recovery hint retained a live registration")
	}
}

func TestCheckpointObservationDoesNotBlockPTYPump(t *testing.T) {
	for _, mode := range []string{"initial", "periodic"} {
		t.Run(mode, func(t *testing.T) { checkpointObservationPump(t, mode) })
	}
}

func checkpointObservationPump(t *testing.T, mode string) {
	t.Helper()
	started, release := make(chan struct{}), make(chan struct{})
	var block atomic.Bool
	var entered sync.Once
	notify := func() { close(started) }
	w, _ := checkpointTestWorker(t, func(int, int) processObservation {
		if block.Load() {
			entered.Do(notify)
			<-release
		}
		return processObservation{directory: "/observed"}
	})
	pty := newPipePTY()
	w.pty, w.pumpDone = pty, make(chan struct{})
	done := make(chan error, 1)
	t.Cleanup(func() {
		block.Store(false)
		close(release)
		_ = pty.Close()
		<-w.pumpDone
		if mode == "initial" {
			w.stopRecovery()
			return
		}
		if err := <-done; err != nil {
			t.Error(err)
		}
		w.checkpointWriter.Close()
	})
	go w.pump()
	if mode == "periodic" {
		w.openRecovery()
		checkpointOK(t, w.checkpoint())
	}
	block.Store(true)
	if mode == "initial" {
		w.startRecovery()
	} else {
		go func() { ack, _ := w.periodicCheckpoint(); done <- <-ack }()
	}
	<-started
	output := []byte("output during blocked checkpoint")
	if _, err := pty.emit(output); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		matched := bytes.Equal(w.ring.Last(len(output)), output)
		w.mu.Unlock()
		if matched {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("PTY pump stalled behind checkpoint observation")
}

type countingScreen struct {
	terminalstate.Screen
	saves atomic.Int64
	saved chan struct{}
}

func (s *countingScreen) SaveText(lines, bytes int) terminalstate.TextSnapshot {
	s.saves.Add(1)
	snapshot := s.Screen.SaveText(lines, bytes)
	if s.saved != nil {
		select {
		case s.saved <- struct{}{}:
		default:
		}
	}
	return snapshot
}

func periodicTestWorker(t *testing.T, source recovery.DirectorySource, observer func(int, int) processObservation) (*Worker, *countingScreen) {
	t.Helper()
	w, screen := checkpointTestWorker(t, observer)
	if !w.openRecovery() {
		t.Fatal("recovery did not open")
	}
	w.recoveryState.DirectorySource = source
	if source == recovery.DirectoryShell {
		w.recoveryState.ShellDirectory = "/known"
	}
	t.Cleanup(w.checkpointWriter.Close)
	return w, screen
}

func checkpointTestWorker(t *testing.T, observer func(int, int) processObservation) (*Worker, *countingScreen) {
	t.Helper()
	screen := &countingScreen{Screen: terminalstate.NewScreen(120, 40)}
	w := &Worker{
		now:    func() time.Time { return time.Unix(1700000000, 0) },
		cfg:    Config{ID: "7K3D", HostID: "host-one", Dir: t.TempDir(), Cwd: "/launch", Command: []string{"/bin/bash"}},
		cmd:    &exec.Cmd{Process: &os.Process{Pid: 4321}},
		screen: screen, ring: session.NewRing(ringSize), observeProcess: observer,
	}
	return w, screen
}

func checkpointOK(t testing.TB, ack <-chan error) {
	t.Helper()
	if ack == nil {
		t.Fatal("missing durable acknowledgement")
	}
	if err := <-ack; err != nil {
		t.Fatal(err)
	}
}

func checkpointSaved(t testing.TB, w *Worker) recovery.Record {
	t.Helper()
	saved, err := recovery.Read(w.cfg.Dir)
	if err != nil {
		t.Fatal(err)
	}
	return saved
}

func requirePeriodicPreparation(t *testing.T, w *Worker, screen *countingScreen) <-chan error {
	t.Helper()
	before := screen.saves.Load()
	ack, prepared := w.periodicCheckpoint()
	if !prepared || ack == nil || screen.saves.Load() != before+1 {
		t.Fatalf("periodic preparation = %v, ack = %v, saves = %d, want %d", prepared, ack, screen.saves.Load(), before+1)
	}
	return ack
}

func requirePeriodicSkip(t *testing.T, w *Worker, screen *countingScreen) {
	t.Helper()
	before := screen.saves.Load()
	ack, prepared := w.periodicCheckpoint()
	if ack != nil {
		checkpointOK(t, ack)
	}
	if prepared || ack != nil || screen.saves.Load() != before {
		t.Fatalf("covered periodic checkpoint prepared = %v, saves = %d, want %d", prepared, screen.saves.Load(), before)
	}
}

func TestPeriodicCheckpointSkipsOnlyDurableUnchangedState(t *testing.T) {
	for _, source := range []recovery.DirectorySource{recovery.DirectoryShell, recovery.DirectoryLaunch} {
		t.Run(string(source), func(t *testing.T) { periodicUnchanged(t, source) })
	}
}

func periodicUnchanged(t *testing.T, source recovery.DirectorySource) {
	t.Helper()
	observations := 0
	w, screen := periodicTestWorker(t, source, func(int, int) processObservation {
		observations++
		return processObservation{directory: "/observed"}
	})
	checkpointOK(t, w.checkpoint())
	before, err := os.ReadFile(filepath.Join(w.cfg.Dir, "recovery.json"))
	if err != nil {
		t.Fatal(err)
	}
	count := observations
	requirePeriodicSkip(t, w, screen)
	after, err := os.ReadFile(filepath.Join(w.cfg.Dir, "recovery.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("covered checkpoint changed bytes, error = %v", err)
	}
	want := count
	if source != recovery.DirectoryShell {
		want++
	}
	if observations != want {
		t.Fatalf("observation calls = %d, want %d", observations, want)
	}
}

func TestPeriodicCheckpointPreparesScreenChanges(t *testing.T) {
	for _, change := range []string{"output", "resize", "title"} {
		t.Run(change, func(t *testing.T) { periodicScreenChange(t, change) })
	}
}

func periodicScreenChange(t *testing.T, change string) {
	t.Helper()
	w, screen := periodicTestWorker(t, recovery.DirectoryShell, nil)
	w.mu.Lock()
	_, _ = w.screen.Write([]byte("before\r\n"))
	w.lastOutputAt = w.currentTime()
	w.mu.Unlock()
	checkpointOK(t, w.checkpoint())
	before := checkpointSaved(t, w)
	w.mu.Lock()
	switch change {
	case "output":
		_, _ = w.screen.Write([]byte("after\r\n"))
		w.lastOutputAt = w.currentTime()
	case "resize":
		w.screen.Resize(80, 20)
	case "title":
		_, _ = w.screen.Write([]byte("\x1b]2;new title\x07"))
	}
	expected := screen.Screen.SaveText(recovery.MaxLines, recovery.MaxTextBytes)
	w.mu.Unlock()
	checkpointOK(t, requirePeriodicPreparation(t, w, screen))
	saved := checkpointSaved(t, w)
	if saved.LastOutputAt != before.LastOutputAt || !reflect.DeepEqual(saved.Lines, expected.Render()) || saved.Title != expected.Title {
		t.Fatalf("frozen-time %s checkpoint = %+v", change, saved)
	}
	requirePeriodicSkip(t, w, screen)
}

func periodicRecipe() *agentresume.Recipe {
	return &agentresume.Recipe{
		Version: agentresume.Version,
		Launch: agentresume.Launch{Provider: agentresume.Codex, Executable: "/usr/bin/codex", ProviderVersion: "codex-cli 0.153.4",
			Directory: "/project", DataRoot: "/agent-data", Options: []string{"--model", "test-model", "--sandbox", "read-only"}},
		ConversationID: "conversation-one", InvocationToken: "invocation-one", RegisteredAt: time.Unix(1700000000, 0), Lifecycle: agentresume.Active,
	}
}

func periodicReceipt() *agentresume.Receipt {
	return &agentresume.Receipt{SourceSessionID: "8XYZ", Provider: agentresume.Codex, ConversationID: "conversation-one",
		InvocationToken: "invocation-one", VerifiedAt: time.Unix(1700000000, 0)}
}

func TestPeriodicCheckpointPreparesEveryMetadataChange(t *testing.T) {
	changes := []struct {
		name   string
		mutate func(*recovery.Record)
	}{
		{"shell", func(r *recovery.Record) { r.Shell = "/bin/zsh" }},
		{"directory", func(r *recovery.Record) { r.ShellDirectory = "/new" }},
		{"directory-source", func(r *recovery.Record) { r.DirectorySource = recovery.DirectoryObserved }},
		{"title", func(r *recovery.Record) { r.Title = "inherited title" }},
		{"command", func(r *recovery.Record) { r.Command = []string{"/bin/sh", "-l"} }},
		{"restart-added", func(r *recovery.Record) { r.Restart = &recovery.Command{Argv: []string{"/bin/sh"}, Cwd: "/new"} }},
		{"restart-argv-in-place-after-ack", func(r *recovery.Record) { r.Restart.Argv[1] = "new argument" }},
		{"restart-cwd", func(r *recovery.Record) { r.Restart.Cwd = "/new" }},
		{"restart-cleared", func(r *recovery.Record) { r.Restart = nil }},
		{"remote", func(r *recovery.Record) { r.Remote = &recovery.Target{HostID: "remote-two", SessionID: "9XYZ"} }},
		{"remote-cleared", func(r *recovery.Record) { r.Remote = nil }},
		{"agent-lifecycle", func(r *recovery.Record) { recipe := *r.Agent; recipe.Lifecycle = agentresume.Closed; r.Agent = &recipe }},
		{"agent-options", func(r *recovery.Record) {
			recipe := *r.Agent
			recipe.Options = []string{"--model", "another-model"}
			r.Agent = &recipe
		}},
		{"agent-cleared", func(r *recovery.Record) { r.Agent = nil }},
		{"receipt", func(r *recovery.Record) {
			receipt := periodicReceipt()
			receipt.VerifiedAt = receipt.VerifiedAt.Add(time.Second)
			r.AgentResume = receipt
		}},
		{"receipt-identity", func(r *recovery.Record) {
			receipt := *r.AgentResume
			receipt.ConversationID = "conversation-two"
			r.AgentResume = &receipt
		}},
		{"receipt-cleared", func(r *recovery.Record) { r.AgentResume = nil }},
		{"host", func(r *recovery.Record) { r.HostID = "host-two" }},
		{"session", func(r *recovery.Record) { r.SessionID = "8XYZ" }},
	}
	for _, change := range changes {
		t.Run(change.name, func(t *testing.T) { periodicMetadataChange(t, change.mutate) })
	}
}

func periodicMetadataChange(t *testing.T, mutate func(*recovery.Record)) {
	t.Helper()
	w, screen := periodicTestWorker(t, recovery.DirectoryShell, func(int, int) processObservation { return processObservation{} })
	w.recoveryState.Restart = &recovery.Command{Argv: []string{"/bin/sh", "old argument"}, Cwd: "/known"}
	w.recoveryState.Remote = &recovery.Target{HostID: "remote-one", SessionID: "8XYZ"}
	w.recoveryState.Agent, w.recoveryState.AgentResume = periodicRecipe(), periodicReceipt()
	checkpointOK(t, w.checkpoint())
	w.mu.Lock()
	mutate(&w.recoveryState)
	expected := w.recoveryState
	w.mu.Unlock()
	checkpointOK(t, requirePeriodicPreparation(t, w, screen))
	saved := checkpointSaved(t, w)
	expected.CheckpointAt = saved.CheckpointAt
	expected.Lines = saved.Lines
	if !reflect.DeepEqual(saved, expected) {
		t.Fatalf("saved metadata = %+v, want %+v", saved, expected)
	}
	requirePeriodicSkip(t, w, screen)
}

func TestPeriodicCheckpointEqualMetadataPointersSkip(t *testing.T) {
	w, screen := periodicTestWorker(t, recovery.DirectoryShell, nil)
	w.recoveryState.Restart = &recovery.Command{Argv: []string{"/bin/sh"}, Cwd: "/known"}
	w.recoveryState.Agent, w.recoveryState.AgentResume = periodicRecipe(), periodicReceipt()
	w.recoveryState.Remote = &recovery.Target{HostID: "remote-one", SessionID: "8XYZ"}
	checkpointOK(t, w.checkpoint())
	w.mu.Lock()
	w.recoveryState.Restart = &recovery.Command{Argv: []string{"/bin/sh"}, Cwd: "/known"}
	w.recoveryState.Agent, w.recoveryState.AgentResume = periodicRecipe(), periodicReceipt()
	w.recoveryState.Remote = &recovery.Target{HostID: "remote-one", SessionID: "8XYZ"}
	w.mu.Unlock()
	requirePeriodicSkip(t, w, screen)
}

func TestPeriodicCheckpointObservesQuietDirectoryChanges(t *testing.T) {
	directory := "/a"
	w, screen := periodicTestWorker(t, recovery.DirectoryLaunch, func(int, int) processObservation {
		return processObservation{directory: directory}
	})
	checkpointOK(t, w.checkpoint())
	directory = "/b"
	checkpointOK(t, requirePeriodicPreparation(t, w, screen))
	saved := checkpointSaved(t, w)
	if saved.ShellDirectory != "/b" || saved.DirectorySource != recovery.DirectoryObserved {
		t.Fatalf("quiet directory = %+v", saved)
	}
	requirePeriodicSkip(t, w, screen)
}

func TestPeriodicCheckpointPreparesLastOutputChange(t *testing.T) {
	w, screen := periodicTestWorker(t, recovery.DirectoryShell, nil)
	checkpointOK(t, w.checkpoint())
	w.mu.Lock()
	w.lastOutputAt = w.currentTime().Add(time.Second)
	expected := w.lastOutputAt
	w.mu.Unlock()
	checkpointOK(t, requirePeriodicPreparation(t, w, screen))
	if saved := checkpointSaved(t, w); saved.LastOutputAt != expected {
		t.Fatalf("last output = %v, want %v", saved.LastOutputAt, expected)
	}
	requirePeriodicSkip(t, w, screen)
}

func blockPeriodicCheckpoint(t *testing.T, w *Worker) string {
	t.Helper()
	path := filepath.Join(w.cfg.Dir, "recovery.json.tmp")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func requirePeriodicFailure(t *testing.T, w *Worker, screen *countingScreen) {
	t.Helper()
	if err := <-requirePeriodicPreparation(t, w, screen); err == nil {
		t.Fatal("failed checkpoint received a successful acknowledgement")
	}
}

func TestFailedPeriodicCheckpointRetriesUntilDurable(t *testing.T) {
	w, screen := periodicTestWorker(t, recovery.DirectoryShell, nil)
	checkpointOK(t, w.checkpoint())
	blocker := blockPeriodicCheckpoint(t, w)
	w.mu.Lock()
	_, _ = w.screen.Write([]byte("retry output\r\n"))
	w.mu.Unlock()
	requirePeriodicFailure(t, w, screen)
	requirePeriodicFailure(t, w, screen)
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	checkpointOK(t, requirePeriodicPreparation(t, w, screen))
	saved := checkpointSaved(t, w)
	if !strings.Contains(strings.Join(saved.Lines, "\n"), "retry output") || saved.Restart != nil {
		t.Fatalf("retry checkpoint = %+v", saved)
	}
	requirePeriodicSkip(t, w, screen)
}

func TestFailedPeriodicMetadataRevertStillPrepares(t *testing.T) {
	w, screen := periodicTestWorker(t, recovery.DirectoryShell, nil)
	checkpointOK(t, w.checkpoint())
	before := checkpointSaved(t, w)
	revision := screen.Revision()
	blocker := blockPeriodicCheckpoint(t, w)
	w.mu.Lock()
	w.recoveryState.Restart = &recovery.Command{Argv: []string{"/bin/sh"}, Cwd: "/b"}
	w.mu.Unlock()
	requirePeriodicFailure(t, w, screen)
	w.mu.Lock()
	w.recoveryState.Restart = nil
	w.mu.Unlock()
	requirePeriodicFailure(t, w, screen)
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	checkpointOK(t, requirePeriodicPreparation(t, w, screen))
	after := checkpointSaved(t, w)
	if screen.Revision() != revision || !reflect.DeepEqual(after, before) {
		t.Fatalf("metadata A/B/A changed screen or frozen record: before %+v, after %+v", before, after)
	}
	requirePeriodicSkip(t, w, screen)
}

func TestRejectedPeriodicCheckpointKeepsPreparing(t *testing.T) {
	w, screen := periodicTestWorker(t, recovery.DirectoryShell, nil)
	checkpointOK(t, w.checkpoint())
	w.checkpointWriter.Close()
	if err := <-w.checkpoint(); err == nil {
		t.Fatal("closed writer accepted forced checkpoint")
	}
	requirePeriodicFailure(t, w, screen)
	requirePeriodicFailure(t, w, screen)
}

func TestExplicitCheckpointsAlwaysSubmitAndWait(t *testing.T) {
	w, screen := periodicTestWorker(t, recovery.DirectoryShell, nil)
	checkpointOK(t, w.checkpoint())
	before := screen.saves.Load()
	checkpointOK(t, w.checkpoint())
	if screen.saves.Load() != before+1 {
		t.Fatal("unchanged explicit checkpoint skipped preparation")
	}
	w.mu.Lock()
	_, _ = w.screen.Write([]byte("latest explicit\r\n"))
	w.mu.Unlock()
	periodic := requirePeriodicPreparation(t, w, screen)
	forced := w.checkpoint()
	checkpointOK(t, forced)
	checkpointOK(t, periodic)
	if saved := checkpointSaved(t, w); !strings.Contains(strings.Join(saved.Lines, "\n"), "latest explicit") {
		t.Fatalf("explicit ack lost output: %q", saved.Lines)
	}
	requirePeriodicSkip(t, w, screen)
}

func TestStopRecoveryFlushesLatestState(t *testing.T) {
	screen := &countingScreen{Screen: terminalstate.NewScreen(120, 40), saved: make(chan struct{}, 1)}
	w := &Worker{now: func() time.Time { return time.Unix(1700000000, 0) },
		cfg: Config{ID: "7K3D", HostID: "host-one", Dir: t.TempDir(), Cwd: "/launch", Command: []string{"/bin/sh"}}, screen: screen}
	var stop sync.Once
	w.startRecovery()
	t.Cleanup(func() { stop.Do(w.stopRecovery) })
	<-screen.saved
	checkpointOK(t, w.checkpoint())
	before := screen.saves.Load()
	w.mu.Lock()
	_, _ = w.screen.Write([]byte("final flush marker\r\n"))
	w.screen.Resize(80, 20)
	w.mu.Unlock()
	stop.Do(w.stopRecovery)
	if screen.saves.Load() <= before {
		t.Fatal("final checkpoint skipped preparation")
	}
	if saved := checkpointSaved(t, w); !strings.Contains(strings.Join(saved.Lines, "\n"), "final flush marker") {
		t.Fatalf("final checkpoint lost marker: %q", saved.Lines)
	}
}

func TestPeriodicCheckpointRacesWithOutputAndResize(t *testing.T) {
	w, screen := periodicTestWorker(t, recovery.DirectoryShell, nil)
	now := time.Unix(1700000000, 0)
	w.now = func() time.Time { return now }
	start, done := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { <-done })
	go func() {
		defer close(done)
		<-start
		for i := 0; i < 500; i++ {
			writePeriodicOutput(w, i, &now)
		}
	}()
	close(start)
	for {
		select {
		case <-done:
			finishPeriodicOutput(t, w, screen)
			return
		default:
			ackPeriodicCheckpoint(t, w)
		}
	}
}

func ackPeriodicCheckpoint(t testing.TB, w *Worker) {
	t.Helper()
	ack, _ := w.periodicCheckpoint()
	if ack != nil {
		checkpointOK(t, ack)
	}
}

func writePeriodicOutput(w *Worker, i int, now *time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, _ = fmt.Fprintf(w.screen, "n%03d\r\n", i)
	*now = now.Add(time.Millisecond)
	w.lastOutputAt = w.currentTime()
	if i%50 == 0 {
		w.screen.Resize(80+i%3, 20)
	}
}

func finishPeriodicOutput(t *testing.T, w *Worker, screen *countingScreen) {
	t.Helper()
	ack, _ := w.periodicCheckpoint()
	if ack != nil {
		checkpointOK(t, ack)
	}
	requirePeriodicSkip(t, w, screen)
	lines := checkpointSaved(t, w).Lines
	if len(lines) == 0 || strings.TrimSpace(strings.Join(lines, "\n")) == "" {
		t.Fatal("no final output")
	}
	if last := strings.TrimSpace(strings.Join(lines, "\n")); !strings.HasSuffix(last, "n499") {
		t.Fatalf("final output = %q", lines)
	}
}

func BenchmarkPeriodicCheckpoint(b *testing.B) {
	for _, metadata := range []string{"ordinary", "rich", "near-limit"} {
		b.Run(metadata, func(b *testing.B) { benchmarkPeriodicDirectories(b, metadata) })
	}
}

func benchmarkPeriodicDirectories(b *testing.B, metadata string) {
	for _, directory := range []string{"known-directory", "observed-directory"} {
		b.Run(directory, func(b *testing.B) { benchmarkPeriodicCases(b, metadata, directory) })
	}
}

func benchmarkPeriodicCases(b *testing.B, metadata, directory string) {
	for _, mode := range []string{"unchanged", "changed"} {
		b.Run(mode, func(b *testing.B) { benchmarkPeriodicCheckpoint(b, metadata, directory, mode) })
	}
	if directory == "observed-directory" {
		b.Run("observation-only", func(b *testing.B) { benchmarkPeriodicObservation(b, metadata) })
	}
}

func benchmarkPeriodicCheckpoint(b *testing.B, metadata, directory, mode string) {
	w, now := checkpointBenchmarkWorker(b, metadata, directory)
	checkpointOK(b, w.checkpoint())
	b.ReportAllocs()
	for b.Loop() {
		if mode == "changed" {
			writeCheckpointBenchmarkOutput(w, now)
		}
		ack, prepared := w.periodicCheckpoint()
		if prepared {
			checkpointOK(b, ack)
		}
	}
}

func writeCheckpointBenchmarkOutput(w *Worker, now *time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, _ = w.screen.Write([]byte("changed\r\n"))
	*now = now.Add(time.Millisecond)
	w.lastOutputAt = w.currentTime()
}

func benchmarkPeriodicObservation(b *testing.B, metadata string) {
	w, _ := checkpointBenchmarkWorker(b, metadata, "observed-directory")
	checkpointOK(b, w.checkpoint())
	b.ReportAllocs()
	for b.Loop() {
		w.recoveryDirectoryObservation()
	}
}

// This fixture also runs against the actual pre-Plan011 forced implementation.
func BenchmarkCheckpointMetadata(b *testing.B) {
	for _, metadata := range []string{"ordinary", "rich", "near-limit"} {
		b.Run(metadata, func(b *testing.B) { benchmarkForcedDirectories(b, metadata) })
	}
}

func benchmarkForcedDirectories(b *testing.B, metadata string) {
	for _, directory := range []string{"known-directory", "observed-directory"} {
		b.Run(directory, func(b *testing.B) { benchmarkForcedCheckpoint(b, metadata, directory) })
	}
}

func benchmarkForcedCheckpoint(b *testing.B, metadata, directory string) {
	w, _ := checkpointBenchmarkWorker(b, metadata, directory)
	checkpointOK(b, w.checkpoint())
	b.ReportAllocs()
	for b.Loop() {
		checkpointOK(b, w.checkpoint())
	}
}

func checkpointBenchmarkWorker(b *testing.B, metadata, directory string) (*Worker, *time.Time) {
	b.Helper()
	now := time.Unix(1700000000, 0)
	w := &Worker{
		now:    func() time.Time { return now },
		cfg:    Config{ID: "7K3D", HostID: "bench-host", Dir: b.TempDir(), Cwd: "/work", Command: []string{"/bin/sh"}},
		cmd:    &exec.Cmd{Process: &os.Process{Pid: os.Getpid()}},
		screen: terminalstate.NewScreen(120, 40), ring: session.NewRing(ringSize),
		recoveryState: recovery.Record{Version: recovery.Version, HostID: "bench-host", SessionID: "7K3D", Command: []string{"/bin/sh"},
			Shell: "/bin/sh", ShellDirectory: "/work", DirectorySource: recovery.DirectoryShell},
	}
	if directory == "observed-directory" {
		w.recoveryState.DirectorySource = recovery.DirectoryLaunch
	}
	checkpointBenchmarkMetadata(&w.recoveryState, metadata)
	_, _ = w.screen.Write([]byte(strings.Repeat("\x1b[32mINFO\x1b[0m compiled package; tests passed\r\n", 400)))
	w.checkpointWriter = recovery.NewWriter(w.cfg.Dir, func(err error) { b.Error(err) })
	b.Cleanup(w.checkpointWriter.Close)
	return w, &now
}

func checkpointBenchmarkMetadata(record *recovery.Record, metadata string) {
	if metadata == "ordinary" {
		return
	}
	record.Restart = &recovery.Command{Argv: []string{"/bin/sh", "argument with spaces"}, Cwd: "/work"}
	record.Agent, record.AgentResume = periodicRecipe(), periodicReceipt()
	record.Remote = &recovery.Target{HostID: "remote-host", SessionID: "8XYZ"}
	if metadata == "near-limit" {
		record.Command = []string{"/bin/sh", strings.Repeat("a", recovery.MaxCommandBytes-len("/bin/sh"))}
		record.Restart.Argv = []string{"/bin/sh", strings.Repeat("b", recovery.MaxCommandBytes-len("/bin/sh"))}
		record.Agent.Options = nil
		for range 8 {
			record.Agent.Options = append(record.Agent.Options, "--model", strings.Repeat("c", 4089))
		}
	}
}
