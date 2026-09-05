package worker

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/agentresume"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
)

func agentTestWorker(t *testing.T) *Worker {
	t.Helper()
	w := recoveryTestWorker(t)
	w.agentCaller = func(net.Conn, int) error { return nil }
	t.Cleanup(w.closeAgents)
	return w
}

func agentTestLaunch(provider agentresume.Provider) agentresume.Launch {
	return agentresume.Launch{Provider: provider, Executable: "/provider/tool", ProviderVersion: "1.0.0",
		Directory: "/project", DataRoot: "/provider/data"}
}

func beginTestAgent(t *testing.T, w *Worker, launch agentresume.Launch, expected string, explicit bool) (net.Conn, string) {
	t.Helper()
	return beginTestAgentMode(t, w, launch, expected, explicit, false)
}

func beginTestAgentMode(t *testing.T, w *Worker, launch agentresume.Launch, expected string, explicit, lookupOnly bool) (net.Conn, string) {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	go w.serve(server)
	request := protocol.Control{Type: protocol.TypeAgentBegin, SessionID: w.cfg.ID, AgentHostID: w.cfg.HostID,
		AgentLaunch: &launch, AgentPID: 4322, AgentExpectedID: expected, AgentExplicit: explicit, AgentLookupOnly: lookupOnly}
	response := agentLeaseRequest(t, client, request)
	if response.Type != protocol.TypeAgentBegun || response.AgentToken == "" || response.AgentHostID != w.cfg.HostID {
		t.Fatalf("agent begin = %+v", response)
	}
	return client, response.AgentToken
}

func agentLeaseRequest(t *testing.T, conn net.Conn, request protocol.Control) protocol.Control {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	defer conn.SetDeadline(time.Time{}) //nolint:errcheck // reset the bounded request deadline
	if err := protocol.NewWriter(conn).WriteControlMsg(request); err != nil {
		t.Fatal(err)
	}
	frame, err := protocol.NewReader(conn).ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	response, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func agentTestEvent(t *testing.T, w *Worker, token, id string) protocol.Control {
	t.Helper()
	w.agentMu.Lock()
	provider := agentresume.Claude
	if w.agentInvocation != nil {
		provider = w.agentInvocation.launch.Provider
	}
	w.agentMu.Unlock()
	return inspectRequest(t, w, protocol.Control{Type: protocol.TypeAgentEvent, SessionID: w.cfg.ID,
		AgentHostID: w.cfg.HostID, AgentToken: token, AgentProvider: provider,
		AgentEvent: &agentresume.Event{Kind: agentresume.Start, ConversationID: id, Directory: "/project"}})
}

func readTestAgent(t *testing.T, w *Worker) recovery.Record {
	t.Helper()
	saved, err := recovery.Read(w.cfg.Dir)
	if err != nil {
		t.Fatal(err)
	}
	return saved
}

func TestAgentRegistrationAcknowledgesDurableIdentityAndNormalClosure(t *testing.T) {
	w := agentTestWorker(t)
	w.mu.Lock()
	w.recoveryState.Remote = &recovery.Target{HostID: "previous-remote", SessionID: "8XYZ"}
	w.mu.Unlock()
	conn, token := beginTestAgent(t, w, agentTestLaunch(agentresume.Claude), "", false)
	if response := agentTestEvent(t, w, token, "exact-conversation"); response.Type != protocol.TypeAgentRegistered {
		t.Fatalf("registration = %+v", response)
	}
	saved := readTestAgent(t, w)
	if saved.Agent == nil || saved.Agent.ConversationID != "exact-conversation" || saved.Agent.InvocationToken != token || saved.Agent.Lifecycle != agentresume.Active {
		t.Fatalf("acknowledged recipe = %+v", saved.Agent)
	}
	if saved.ShellDirectory == saved.Agent.Directory {
		t.Fatal("agent directory replaced the saved shell directory")
	}
	if saved.Remote != nil {
		t.Fatal("new local agent retained a stale remote recovery target")
	}
	finish := agentLeaseRequest(t, conn, protocol.Control{Type: protocol.TypeAgentFinish, SessionID: w.cfg.ID, AgentToken: token})
	if finish.Type != protocol.TypeOK || !finish.AgentVerified {
		t.Fatalf("finish = %+v", finish)
	}
	saved = readTestAgent(t, w)
	if saved.Agent.Lifecycle != agentresume.Closed || saved.Agent.ConversationID != "exact-conversation" {
		t.Fatalf("closed recipe = %+v", saved.Agent)
	}
}

func TestAgentNewInvocationRejectsOldHooksWithoutLosingPreviousRecipe(t *testing.T) {
	w := agentTestWorker(t)
	_, old := beginTestAgent(t, w, agentTestLaunch(agentresume.Codex), "", false)
	if response := agentTestEvent(t, w, old, "codex-conversation"); response.Type != protocol.TypeAgentRegistered {
		t.Fatal(response.Message)
	}
	_, current := beginTestAgent(t, w, agentTestLaunch(agentresume.Claude), "", false)
	if saved := readTestAgent(t, w); saved.Agent.ConversationID != "codex-conversation" {
		t.Fatal("unregistered invocation superseded the saved identity")
	}
	if response := agentTestEvent(t, w, old, "late-old-start"); response.Type != protocol.TypeError {
		t.Fatalf("old token accepted: %+v", response)
	}
	if response := agentTestEvent(t, w, current, "claude-conversation"); response.Type != protocol.TypeAgentRegistered {
		t.Fatal(response.Message)
	}
	saved := readTestAgent(t, w)
	if saved.Agent.Provider != agentresume.Claude || saved.Agent.ConversationID != "claude-conversation" || saved.Agent.InvocationToken == old {
		t.Fatalf("new primary = %+v", saved.Agent)
	}
}

func TestAgentLeaseLossPreservesCrashBindingAndRejectsLateEvents(t *testing.T) {
	w := agentTestWorker(t)
	conn, token := beginTestAgent(t, w, agentTestLaunch(agentresume.Claude), "", false)
	if response := agentTestEvent(t, w, token, "crashed"); response.Type != protocol.TypeAgentRegistered {
		t.Fatal(response.Message)
	}
	_ = conn.Close()
	w.agentReaders.Wait()
	if response := agentTestEvent(t, w, token, "late"); response.Type != protocol.TypeError {
		t.Fatal("lost lease still accepts events")
	}
	saved := readTestAgent(t, w)
	if saved.Agent.ConversationID != "crashed" || saved.Agent.Lifecycle != agentresume.Active {
		t.Fatalf("unexpected disconnect closed or cleared recipe: %+v", saved.Agent)
	}
}

func TestAgentRecoveryRequiresExpectedIDAndRetainsReceiptAcrossConversationChanges(t *testing.T) {
	w := agentTestWorker(t)
	launch := agentTestLaunch(agentresume.Claude)
	w.cfg.RecoveredFrom = "8XYZ"
	w.agentExpected = &agentresume.Recipe{Version: 1, Launch: launch, ConversationID: "expected", InvocationToken: "old", RegisteredAt: time.Now(), Lifecycle: agentresume.Active}
	_, token := beginTestAgent(t, w, launch, "expected", false)
	if response := agentTestEvent(t, w, token, "wrong"); response.Type != protocol.TypeError {
		t.Fatal("wrong provider identity was acknowledged")
	}
	if err := <-w.checkpoint(); err != nil {
		t.Fatal(err)
	}
	if saved := readTestAgent(t, w); saved.Agent != nil || saved.AgentResume != nil {
		t.Fatalf("mismatch became recovery evidence: %+v", saved)
	}
	if response := agentTestEvent(t, w, token, "expected"); response.Type != protocol.TypeAgentRegistered {
		t.Fatal(response.Message)
	}
	before := readTestAgent(t, w)
	if before.AgentResume == nil || before.AgentResume.SourceSessionID != "8XYZ" || before.AgentResume.ConversationID != "expected" {
		t.Fatalf("matching receipt = %+v", before.AgentResume)
	}
	for _, event := range []agentresume.Event{
		{Kind: agentresume.End, ConversationID: "expected", Directory: "/project"},
		{Kind: agentresume.Start, ConversationID: "subagent", Directory: "/project", Subagent: true},
	} {
		response := inspectRequest(t, w, protocol.Control{Type: protocol.TypeAgentEvent, SessionID: w.cfg.ID,
			AgentHostID: w.cfg.HostID, AgentToken: token, AgentProvider: launch.Provider, AgentEvent: &event})
		if response.Type != protocol.TypeAgentRegistered {
			t.Fatalf("ignored event = %+v", response)
		}
	}
	if saved := readTestAgent(t, w); saved.Agent.ConversationID != "expected" || saved.Agent.Lifecycle != agentresume.Active {
		t.Fatalf("end/subagent changed primary: %+v", saved.Agent)
	}
	if response := agentTestEvent(t, w, token, "forked"); response.Type != protocol.TypeAgentRegistered {
		t.Fatal(response.Message)
	}
	saved := readTestAgent(t, w)
	if saved.Agent.ConversationID != "forked" || *saved.AgentResume != *before.AgentResume {
		t.Fatalf("conversation change rewrote resume receipt: %+v", saved)
	}
}

func TestAgentExplicitBindingWithoutHookRemainsUnverified(t *testing.T) {
	w := agentTestWorker(t)
	conn, token := beginTestAgent(t, w, agentTestLaunch(agentresume.Codex), "explicit-id", true)
	finish := agentLeaseRequest(t, conn, protocol.Control{Type: protocol.TypeAgentFinish, SessionID: w.cfg.ID, AgentToken: token})
	if finish.Type != protocol.TypeOK || finish.AgentVerified {
		t.Fatalf("unobserved explicit binding = %+v", finish)
	}
	if err := <-w.checkpoint(); err != nil {
		t.Fatal(err)
	}
	if saved := readTestAgent(t, w); saved.Agent != nil {
		t.Fatalf("explicit input was treated as proof: %+v", saved.Agent)
	}
}

func TestAgentExplicitBindingPreservesProvenanceAfterClosing(t *testing.T) {
	w := agentTestWorker(t)
	conn, token := beginTestAgent(t, w, agentTestLaunch(agentresume.Claude), "explicit-id", true)
	if response := agentTestEvent(t, w, token, "explicit-id"); response.Type != protocol.TypeAgentRegistered {
		t.Fatal(response.Message)
	}
	finish := agentLeaseRequest(t, conn, protocol.Control{Type: protocol.TypeAgentFinish, SessionID: w.cfg.ID, AgentToken: token})
	if finish.Type != protocol.TypeOK || !finish.AgentVerified {
		t.Fatalf("explicit finish = %+v", finish)
	}
	saved := readTestAgent(t, w)
	if !saved.Agent.Explicit || saved.Agent.Lifecycle != agentresume.Closed {
		t.Fatalf("explicit provenance was lost: %+v", saved.Agent)
	}
}

func TestAgentHookCannotRegisterAnotherProvidersToken(t *testing.T) {
	w := agentTestWorker(t)
	_, token := beginTestAgent(t, w, agentTestLaunch(agentresume.Codex), "", false)
	response := inspectRequest(t, w, protocol.Control{Type: protocol.TypeAgentEvent, SessionID: w.cfg.ID,
		AgentHostID: w.cfg.HostID, AgentToken: token, AgentProvider: agentresume.Claude,
		AgentEvent: &agentresume.Event{Kind: agentresume.Start, ConversationID: "claude-id", Directory: "/project"}})
	if response.Type != protocol.TypeError {
		t.Fatal("another provider used the current invocation token")
	}
	if err := <-w.checkpoint(); err != nil {
		t.Fatal(err)
	}
	if saved := readTestAgent(t, w); saved.Agent != nil {
		t.Fatalf("cross-provider event changed recovery: %+v", saved.Agent)
	}
}

func TestAgentLookupInReplacementShellCannotVerifyResume(t *testing.T) {
	w := agentTestWorker(t)
	launch := agentTestLaunch(agentresume.Codex)
	w.cfg.RecoveredFrom = "8XYZ"
	w.agentExpected = &agentresume.Recipe{Version: 1, Launch: launch, ConversationID: "expected", InvocationToken: "old", RegisteredAt: time.Now(), Lifecycle: agentresume.Active}
	conn, token := beginTestAgentMode(t, w, launch, "expected", true, true)
	if response := agentTestEvent(t, w, token, "expected"); response.Type != protocol.TypeAgentRegistered {
		t.Fatal(response.Message)
	}
	finish := agentLeaseRequest(t, conn, protocol.Control{Type: protocol.TypeAgentFinish, SessionID: w.cfg.ID, AgentToken: token})
	if finish.Type != protocol.TypeOK || !finish.AgentVerified {
		t.Fatalf("lookup did not save a validated binding: %+v", finish)
	}
	saved := readTestAgent(t, w)
	if saved.Agent == nil || !saved.Agent.Explicit || saved.Agent.ConversationID != "expected" || saved.AgentResume != nil {
		t.Fatalf("lookup was treated as native resume evidence: %+v", saved)
	}
	_, actualToken := beginTestAgent(t, w, launch, "expected", true)
	if response := agentTestEvent(t, w, actualToken, "expected"); response.Type != protocol.TypeAgentRegistered {
		t.Fatal(response.Message)
	}
	saved = readTestAgent(t, w)
	if saved.AgentResume == nil || saved.AgentResume.InvocationToken != actualToken {
		t.Fatalf("actual resumed explicit conversation did not verify: %+v", saved)
	}
}

func TestAgentRegistrationDiskFailureIsNotAcknowledged(t *testing.T) {
	w := agentTestWorker(t)
	_, token := beginTestAgent(t, w, agentTestLaunch(agentresume.Claude), "", false)
	if err := <-w.checkpoint(); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(w.cfg.Dir, "recovery.json.tmp")
	if err := os.Mkdir(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	if response := agentTestEvent(t, w, token, "not-durable"); response.Type != protocol.TypeError {
		t.Fatalf("disk failure acknowledged: %+v", response)
	}
	if saved := readTestAgent(t, w); saved.Agent != nil {
		t.Fatal("failed write changed durable checkpoint")
	}
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	if response := agentTestEvent(t, w, token, "not-durable"); response.Type != protocol.TypeAgentRegistered {
		t.Fatalf("retry registration = %+v", response)
	}
}

func TestAgentBeginRequiresExactOwnerAndForegroundCaller(t *testing.T) {
	w := agentTestWorker(t)
	launch := agentTestLaunch(agentresume.Claude)
	request := protocol.Control{Type: protocol.TypeAgentBegin, SessionID: w.cfg.ID, AgentHostID: "another-host", AgentLaunch: &launch, AgentPID: 1}
	if response := inspectRequest(t, w, request); response.Type != protocol.TypeError {
		t.Fatal("wrong host accepted")
	}
	w.agentCaller = func(net.Conn, int) error { return fmt.Errorf("not foreground") }
	request.AgentHostID = w.cfg.HostID
	if response := inspectRequest(t, w, request); response.Type != protocol.TypeError {
		t.Fatal("nonforeground helper accepted")
	}
}
