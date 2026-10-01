package protocol

import (
	"reflect"
	"testing"

	"github.com/shaul/mesh/internal/agentresume"
	"github.com/shaul/mesh/internal/recovery"
)

func TestLeanRecoveryInfoOmitsOnlyDisplayIndependentDetails(t *testing.T) {
	record := recovery.Record{Lines: []string{"saved screen"}, Command: []string{"sh", "-c", "large launch command"}, Agent: &agentresume.Recipe{Launch: agentresume.Launch{Provider: agentresume.Codex, Options: []string{"--model", "model"}}, ConversationID: "conversation"}, AgentResume: &agentresume.Receipt{ConversationID: "conversation"}, Restart: &recovery.Command{Argv: []string{"make", "test"}, Cwd: "/work"}}
	var info SessionInfo
	LeanRecoveryInfo(&info, record)
	if !info.RecoveryDetailsOmitted || len(info.Recovery.Lines) != 0 || len(info.Recovery.Command) != 0 || info.Recovery.AgentResume != nil || len(info.Recovery.Agent.Options) != 0 {
		t.Fatalf("lean fields = %+v", info.Recovery)
	}
	if len(record.Agent.Options) != 2 || len(record.Command) != 3 || len(record.Lines) != 1 || record.AgentResume == nil {
		t.Fatal("lean response mutated the full record")
	}
	want := record
	want.Lines, want.Command, want.AgentResume = nil, nil, nil
	agent := *record.Agent
	agent.Options = nil
	want.Agent = &agent
	if !reflect.DeepEqual(info.Recovery, &want) {
		t.Fatal("lean catalog changed recognition or recovery action metadata")
	}
}
