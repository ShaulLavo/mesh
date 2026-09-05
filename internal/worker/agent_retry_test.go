package worker

import (
	"fmt"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/agentresume"
	"github.com/shaul/mesh/internal/protocol"
)

func TestFailedAgentResumeShellCanCaptureNewConversation(t *testing.T) {
	w := agentTestWorker(t)
	launch := agentTestLaunch(agentresume.Claude)
	w.cfg.RecoveredFrom = "8XYZ"
	w.agentExpected = &agentresume.Recipe{Version: 1, Launch: launch, ConversationID: "deleted-conversation",
		InvocationToken: "previous-token", RegisteredAt: time.Now(), Lifecycle: agentresume.Active}
	conn, token := beginTestAgent(t, w, launch, "deleted-conversation", false)
	response := agentLeaseRequest(t, conn, protocol.Control{Type: protocol.TypeAgentFinish, SessionID: w.cfg.ID, AgentToken: token})
	if response.Type != protocol.TypeOK || response.AgentVerified {
		t.Fatalf("failed native resume = %+v", response)
	}

	_, nextToken := beginTestAgent(t, w, agentTestLaunch(agentresume.Codex), "", false)
	if response := agentTestEvent(t, w, nextToken, "new-user-selected-conversation"); response.Type != protocol.TypeAgentRegistered {
		t.Fatalf("new conversation registration = %+v", response)
	}
	saved := readTestAgent(t, w)
	if saved.Agent == nil || saved.Agent.ConversationID != "new-user-selected-conversation" || saved.AgentResume != nil {
		t.Fatalf("new primary must be saved without verifying the failed resume: %+v", saved)
	}
}

func TestNewInvocationCannotVerifyPendingResumeWithMatchingID(t *testing.T) {
	for _, changedLaunch := range []bool{false, true} {
		t.Run(fmt.Sprint(changedLaunch), func(t *testing.T) { testNewInvocationKeepsPendingResumeUnverified(t, changedLaunch) })
	}
}

func testNewInvocationKeepsPendingResumeUnverified(t *testing.T, changedLaunch bool) {
	t.Helper()
	w := agentTestWorker(t)
	launch := agentTestLaunch(agentresume.Claude)
	w.cfg.RecoveredFrom = "8XYZ"
	w.agentExpected = &agentresume.Recipe{Version: 1, Launch: launch, ConversationID: "expected",
		InvocationToken: "previous-token", RegisteredAt: time.Now(), Lifecycle: agentresume.Active}
	expected := ""
	if changedLaunch {
		launch.Options = []string{"--model", "different"}
		expected = "expected"
	}
	_, token := beginTestAgent(t, w, launch, expected, false)
	if response := agentTestEvent(t, w, token, "expected"); response.Type != protocol.TypeAgentRegistered {
		t.Fatal(response.Message)
	}
	if saved := readTestAgent(t, w); saved.Agent == nil || saved.AgentResume != nil {
		t.Fatalf("unrelated invocation verified the pending resume: %+v", saved)
	}
}
