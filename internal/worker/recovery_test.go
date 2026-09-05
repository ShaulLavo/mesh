package worker

import (
	"bytes"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
	"github.com/shaul/mesh/internal/session"
	terminalstate "github.com/shaul/mesh/internal/terminal"
)

func recoveryTestWorker(t *testing.T) *Worker {
	t.Helper()
	w := &Worker{
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
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	pty := newPipePTY()
	w := &Worker{
		cfg: Config{ID: "7K3D", HostID: "host-one", Dir: t.TempDir(), Cwd: "/", Command: []string{"/bin/bash"}},
		cmd: &exec.Cmd{Process: &os.Process{Pid: 4321}}, pty: pty,
		screen: terminalstate.NewScreen(80, 5), ring: session.NewRing(ringSize), pumpDone: make(chan struct{}),
		observeProcess: func(int, int) processObservation {
			select {
			case started <- struct{}{}:
			default:
			}
			<-release
			return processObservation{}
		},
	}
	w.startRecovery()
	defer w.stopRecovery()
	defer close(release)
	defer pty.Close() //nolint:errcheck // test resource cleanup
	go w.pump()
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
