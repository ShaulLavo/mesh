package recovery

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/agentresume"
)

func TestUnverifiedAgentReplacementSurvivesAnotherCrash(t *testing.T) {
	for _, test := range []struct {
		name      string
		action    Action
		lifecycle agentresume.Lifecycle
	}{
		{"default", ActionDefault, agentresume.Active},
		{"explicit", ActionAgent, agentresume.Active},
		{"closed-conversation", ActionAgent, agentresume.Closed},
	} {
		t.Run(test.name, func(t *testing.T) { testUnverifiedAgentReplacementCrash(t, test.action, test.lifecycle) })
	}
}

func testUnverifiedAgentReplacementCrash(t *testing.T, action Action, lifecycle agentresume.Lifecycle) {
	t.Helper()
	cfg, runtime, original := agentTransactionFixture(t)
	original.Agent.Lifecycle = lifecycle
	if err := Write(filepath.Join(cfg.SessionsDir, "7K3D"), original); err != nil {
		t.Fatal(err)
	}
	first, err := Recover(context.Background(), cfg, Request{SessionID: "7K3D", Action: action})
	if err != nil {
		t.Fatal(err)
	}
	replacement := runtime.sources[first.SessionID]
	replacement.State = "interrupted"
	replacement.Command = []string{"/mesh", "agent-resume"}
	runtime.sources[first.SessionID] = replacement
	checkpoint := fallbackRecord(cfg.HostID, replacement)
	checkpoint.Shell = "/bin/sh"
	checkpoint.CheckpointAt = time.Now()
	if err := Write(filepath.Join(cfg.SessionsDir, first.SessionID), checkpoint); err != nil {
		t.Fatal(err)
	}

	followed, err := Recover(context.Background(), cfg, Request{SessionID: "7K3D", Action: action})
	if err != nil || followed.SessionID != first.SessionID || followed.State != "interrupted" || followed.AgentStatus != "unverified" {
		t.Fatalf("follow original claim = %+v, %v", followed, err)
	}
	second, err := Recover(context.Background(), cfg, Request{SessionID: followed.SessionID, Action: action})
	if err != nil {
		t.Fatalf("a second worker crash lost the frozen exact conversation plan: %v", err)
	}
	if second.SessionID == first.SessionID || second.AgentStatus != "unverified" || len(runtime.launches) != 2 {
		t.Fatalf("replacement retry = %+v, launches=%d", second, len(runtime.launches))
	}
	plan := runtime.launches[1].Agent
	if plan == nil || plan.ConversationID != original.Agent.ConversationID {
		t.Fatalf("retry did not preserve exact conversation: %+v", plan)
	}
	current, err := ReplacementID(filepath.Join(cfg.SessionsDir, first.SessionID), cfg.HostID, first.SessionID)
	if err != nil || current != second.SessionID {
		t.Fatalf("pending replacement catalog = %q, %v", current, err)
	}
	again, err := Recover(context.Background(), cfg, Request{SessionID: followed.SessionID})
	if err != nil || again.SessionID != second.SessionID || len(runtime.launches) != 2 {
		t.Fatalf("default retry duplicated pending replacement: %+v, %v", again, err)
	}
	retained, err := Read(filepath.Join(cfg.SessionsDir, first.SessionID))
	if err != nil || retained.Agent != nil || retained.AgentResume != nil {
		t.Fatalf("pending plan became acknowledged identity: %+v, %v", retained, err)
	}
}

func TestPendingAgentPlanDoesNotReplaceNewObservedIdentityOrRemoteTarget(t *testing.T) {
	for _, kind := range []string{"agent", "remote"} {
		t.Run(kind, func(t *testing.T) { testPendingAgentKeepsCurrentTarget(t, kind) })
	}
}

func testPendingAgentKeepsCurrentTarget(t *testing.T, kind string) {
	t.Helper()
	cfg, runtime, original := agentTransactionFixture(t)
	first, err := Recover(context.Background(), cfg, Request{SessionID: "7K3D"})
	if err != nil {
		t.Fatal(err)
	}
	replacement := runtime.sources[first.SessionID]
	replacement.State = "interrupted"
	runtime.sources[first.SessionID] = replacement
	checkpoint := fallbackRecord(cfg.HostID, replacement)
	checkpoint.CheckpointAt, checkpoint.Shell = time.Now(), "/bin/sh"
	if kind == "agent" {
		recipe := *original.Agent
		recipe.ConversationID, recipe.InvocationToken = "new-conversation", "new-invocation"
		checkpoint.Agent = &recipe
	} else {
		checkpoint.Remote = &Target{HostID: "remote-host", SessionID: "8XYZ"}
	}
	if err := Write(filepath.Join(cfg.SessionsDir, first.SessionID), checkpoint); err != nil {
		t.Fatal(err)
	}
	result, err := Recover(context.Background(), cfg, Request{SessionID: first.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if kind == "remote" {
		if result.Remote == nil || *result.Remote != *checkpoint.Remote || len(runtime.launches) != 1 {
			t.Fatalf("pending plan replaced remote target: %+v", result)
		}
		return
	}
	if len(runtime.launches) != 2 || runtime.launches[1].Agent.ConversationID != "new-conversation" {
		t.Fatalf("pending plan replaced new primary: %+v", runtime.launches)
	}
}
