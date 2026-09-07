package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/updatebootstrap"
	"github.com/shaul/mesh/internal/updateinstall"
)

type legacyFixture struct {
	t        *testing.T
	c        *Coordinator
	run      Run
	remote   *fakeRemote
	sessions map[string]protocol.SessionInfo
	created  map[string]string
	commands []protocol.Control
	output   []byte
	loseAck  bool
}

func newLegacyFixture(t *testing.T) *legacyFixture {
	t.Helper()
	c, remote, run := testCoordinator(t, 1)
	run, err := c.Store.Change(run.ID, func(run *Run) error { run.Cached = true; return nil })
	if err != nil {
		t.Fatal(err)
	}
	f := &legacyFixture{t: t, c: c, run: run, remote: remote, sessions: make(map[string]protocol.SessionInfo), created: make(map[string]string)}
	c.LegacyExchange = f.exchange
	return f
}

func (f *legacyFixture) exchange(_ context.Context, host Host, request protocol.Control) (protocol.Control, error) {
	response := protocol.Control{RequestID: request.RequestID}
	switch request.Type {
	case protocol.TypeHostInfo:
		response.Type, response.Host = protocol.TypeHostInfo, &protocol.HostInfo{ID: host.ID}
	case protocol.TypeCreate:
		f.commands = append(f.commands, request)
		id := f.created[request.RequestID]
		if id == "" {
			id = fmt.Sprintf("session-%d", len(f.created)+1)
			f.created[request.RequestID] = id
			f.sessions[id] = protocol.SessionInfo{ID: id, State: "running"}
		}
		if f.loseAck {
			f.loseAck = false
			return response, errors.New("connection lost after durable session creation")
		}
		response.Type, response.SessionID = protocol.TypeCreated, id
	case protocol.TypeList:
		response.Type = protocol.TypeListed
		for _, session := range f.sessions {
			response.Sessions = append(response.Sessions, session)
		}
	case protocol.TypeLogs:
		response.Type, response.SessionID, response.Output = protocol.TypeLogged, request.SessionID, f.output
	default:
		return response, fmt.Errorf("unexpected legacy request %s", request.Type)
	}
	return response, nil
}

func (f *legacyFixture) step() {
	f.t.Helper()
	if err := f.c.bootstrap(context.Background(), f.run, 0); err != nil {
		f.t.Fatal(err)
	}
	var err error
	f.run, err = f.c.Store.Read(f.run.ID)
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f *legacyFixture) finish(id string, code int) {
	f.sessions[id] = protocol.SessionInfo{ID: id, State: "exited", ExitCode: &code}
}

func (f *legacyFixture) receipt(phase updateinstall.Phase) {
	f.t.Helper()
	f.finish(f.run.Targets[0].BootstrapSession, 0)
	f.step()
	f.finish(f.run.Targets[0].BootstrapStatusSession, 0)
	request := bootstrapRequest(f.run, 0)
	status := updateinstall.Status{Schema: 1, Phase: phase, Request: updateinstall.Request{ID: request.ID, TargetID: request.TargetID, Generation: request.Generation, Manifest: request.Manifest}, Error: "candidate failed health"}
	data, err := json.Marshal(status)
	if err != nil {
		f.t.Fatal(err)
	}
	f.output = append([]byte("MESH_UPDATE_RECEIPT="), data...)
}

func TestLegacyLostCreateAcknowledgementReusesPersistedAttempt(t *testing.T) {
	f := newLegacyFixture(t)
	f.loseAck = true
	f.step()
	first := f.run.Targets[0]
	if !first.Grant || first.BootstrapAttempt != 1 || first.BootstrapSession != "" {
		t.Fatalf("grant not reserved before send: %+v", first)
	}
	f.step()
	if len(f.created) != 1 || len(f.commands) != 2 || f.commands[0].RequestID != f.commands[1].RequestID || f.run.Targets[0].Generation != first.Generation {
		t.Fatal("lost acknowledgement changed durable operation identity")
	}
	if f.run.Targets[0].BootstrapSession == "" {
		t.Fatal("did not recover session receipt")
	}
}

func TestLegacyRollbackReceiptStopsRunAndClearsGrant(t *testing.T) {
	f := newLegacyFixture(t)
	f.step()
	f.receipt(updateinstall.RolledBack)
	f.step()
	target := f.run.Targets[0]
	if !f.run.Stopped || target.Grant || target.State != Failed || !strings.Contains(target.Problem, "rolled_back") {
		t.Fatalf("rollback was not durable: %+v", target)
	}
	before := len(f.commands)
	f.step()
	if len(f.commands) != before {
		t.Fatal("failed target retried without approval")
	}
}

func TestLegacyExplicitRetryReplacesEndedAttemptAndCarriesToken(t *testing.T) {
	f := newLegacyFixture(t)
	f.step()
	f.receipt(updateinstall.RolledBack)
	f.step()
	generation := f.run.Targets[0].Generation
	var err error
	f.run, err = f.c.Store.Retry(f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.step()
	if !f.run.Targets[0].Grant || f.run.Targets[0].BootstrapRetry || f.run.Targets[0].BootstrapRetryToken != 2 {
		t.Fatalf("retry intent not reserved: %+v", f.run.Targets[0])
	}
	f.step()
	if f.run.Targets[0].Generation != generation || f.run.Targets[0].BootstrapAttempt != 2 {
		t.Fatal("retry changed approved generation")
	}
	command := f.commands[len(f.commands)-1].Command
	var decoded updatebootstrap.Request
	for _, arg := range command {
		request, decodeErr := updatebootstrap.Decode(arg)
		if decodeErr == nil {
			decoded = request
			break
		}
	}
	if decoded.ID != f.run.ID || decoded.RetryToken != 2 {
		t.Fatalf("retry command lost token: %+v", decoded)
	}
}

func TestLegacyStagedReceiptResumesSameGrantButCancelPreventsReplay(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(fmt.Sprint(cancel), func(t *testing.T) {
			f := newLegacyFixture(t)
			f.step()
			f.receipt(updateinstall.Staged)
			generation := f.run.Targets[0].Generation
			if cancel {
				var err error
				f.run, err = f.c.Store.Change(f.run.ID, func(run *Run) error { run.Cancel = true; return nil })
				if err != nil {
					t.Fatal(err)
				}
			}
			before := len(f.commands)
			f.step()
			f.step()
			if cancel {
				if len(f.commands) != before || f.run.Targets[0].BootstrapAttempt != 1 {
					t.Fatal("cancellation spawned another setup attempt")
				}
				return
			}
			if f.run.Targets[0].BootstrapAttempt != 2 || f.run.Targets[0].Generation != generation || len(f.commands) != before+1 {
				t.Fatal("interrupted stage did not resume with a fresh shell and original grant")
			}
		})
	}
}

func TestLegacyCommitRequiresExecutingApprovedImage(t *testing.T) {
	f := newLegacyFixture(t)
	f.step()
	f.receipt(updateinstall.Committed)
	f.step()
	if f.run.Targets[0].State != Failed {
		t.Fatal("shell receipt was accepted without the approved executing image")
	}
}

func TestLegacyReceiptRejectsWrongOperationAndTruncation(t *testing.T) {
	f := newLegacyFixture(t)
	f.step()
	f.receipt(updateinstall.RolledBack)
	wrong := []byte(strings.Replace(string(f.output), f.run.ID, "other-operation", 1))
	for _, output := range [][]byte{wrong, make([]byte, protocol.MaxLogTail), append(f.output, []byte(" {}")...)} {
		if _, err := decodeBootstrapReceipt(output, f.run, 0); err == nil {
			t.Fatal("accepted unauthenticated or truncated receipt")
		}
	}
}

func TestLegacyReadOnlyReceiptLostAcknowledgementReusesAttempt(t *testing.T) {
	f := newLegacyFixture(t)
	f.step()
	f.finish(f.run.Targets[0].BootstrapSession, 0)
	f.loseAck = true
	f.step()
	f.step()
	if len(f.created) != 2 || f.run.Targets[0].BootstrapStatusAttempt != 1 || f.run.Targets[0].BootstrapStatusSession == "" {
		t.Fatal("read-only receipt duplicated after lost acknowledgement")
	}
}
