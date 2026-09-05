package recovery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/agentresume"
)

func agentTransactionFixture(t *testing.T) (Config, *transactionRuntime, Record) {
	t.Helper()
	cfg, runtime := transactionFixture(t)
	source := runtime.sources["7K3D"]
	record := fallbackRecord(cfg.HostID, source)
	record.CheckpointAt = time.Now()
	record.Agent = &agentresume.Recipe{Version: 1, ConversationID: "opaque-conversation", InvocationToken: "source-token",
		RegisteredAt: time.Now(), Lifecycle: agentresume.Active,
		Launch: agentresume.Launch{Provider: agentresume.Claude, Executable: "/bin/sh", ProviderVersion: "2.1.258",
			Directory: cfg.SessionsDir, DataRoot: cfg.SessionsDir, Options: []string{"--model", "sonnet"}}}
	if err := Write(filepath.Join(cfg.SessionsDir, source.ID), record); err != nil {
		t.Fatal(err)
	}
	return cfg, runtime, record
}

func TestAgentRecoveryConcurrentUnverifiedRequestsReuseOneTerminal(t *testing.T) {
	cfg, runtime, record := agentTransactionFixture(t)
	var callers sync.WaitGroup
	results := make(chan Result, 12)
	failures := make(chan error, 12)
	for range 12 {
		callers.Go(func() {
			result, err := Recover(context.Background(), cfg, Request{SessionID: "7K3D"})
			results <- result
			failures <- err
		})
	}
	callers.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(runtime.launches) != 1 || runtime.launches[0].Agent == nil {
		t.Fatalf("agent launches = %+v", runtime.launches)
	}
	for result := range results {
		if result.SessionID != runtime.launches[0].ID || result.AgentStatus != "unverified" {
			t.Fatalf("unverified result = %+v", result)
		}
	}
	intent, err := readIntentNamed(filepath.Join(cfg.SessionsDir, "7K3D"), cfg.HostID, "7K3D", agentIntentName(record.Agent.InvocationToken))
	if err != nil || intent.Phase == "complete" {
		t.Fatalf("socket publication completed provider recovery: %+v %v", intent, err)
	}
	plan, err := PendingAgent(cfg.SessionsDir, cfg.HostID, "7K3D", runtime.launches[0].ID)
	if err != nil || plan.ConversationID != record.Agent.ConversationID {
		t.Fatalf("frozen native plan = %+v %v", plan, err)
	}
	if _, err := PendingAgent(cfg.SessionsDir, cfg.HostID, "7K3D", "8XYZ"); err == nil {
		t.Fatal("different worker could consume the reserved plan")
	}
}

func TestAgentRecoveryCompletesOnlyWithMatchingDurableReceipt(t *testing.T) {
	cfg, runtime, record := agentTransactionFixture(t)
	first, err := Recover(context.Background(), cfg, Request{SessionID: "7K3D"})
	if err != nil {
		t.Fatal(err)
	}
	receipt := agentresume.Receipt{SourceSessionID: "7K3D", Provider: record.Agent.Provider,
		ConversationID: "wrong", InvocationToken: "replacement-token", VerifiedAt: time.Now()}
	replacement := runtime.sources[first.SessionID]
	replacement.AgentResume = &receipt
	runtime.sources[first.SessionID] = replacement
	wrong, err := Recover(context.Background(), cfg, Request{SessionID: "7K3D"})
	if err != nil || wrong.AgentStatus != "unverified" || !wrong.Existing {
		t.Fatalf("mismatched identity = %+v %v", wrong, err)
	}
	receipt.ConversationID = record.Agent.ConversationID
	matched, err := Recover(context.Background(), cfg, Request{SessionID: "7K3D"})
	if err != nil || matched.AgentStatus != "verified" || matched.SessionID != first.SessionID || len(runtime.launches) != 1 {
		t.Fatalf("verified result = %+v %v", matched, err)
	}
	retained, err := Read(filepath.Join(cfg.SessionsDir, "7K3D"))
	if err != nil || retained.Agent.ConversationID != record.Agent.ConversationID {
		t.Fatalf("source recipe was lost: %+v %v", retained, err)
	}
	if status := AgentStatus(cfg.SessionsDir, cfg.HostID, "7K3D", first.SessionID, &receipt); status != "verified" {
		t.Fatalf("catalog receipt status = %q", status)
	}
}

func TestAgentRecoveryLiveShellRemainsDefaultAfterExplicitConversationResume(t *testing.T) {
	cfg, runtime, record := agentTransactionFixture(t)
	source := runtime.sources["7K3D"]
	source.State = "detached"
	runtime.sources[source.ID] = source
	record.Agent.Lifecycle = agentresume.Closed
	if err := Write(filepath.Join(cfg.SessionsDir, source.ID), record); err != nil {
		t.Fatal(err)
	}
	conversation, err := Recover(context.Background(), cfg, Request{SessionID: source.ID, Action: ActionAgent})
	if err != nil || conversation.SessionID == source.ID || conversation.AgentStatus != "unverified" {
		t.Fatalf("explicit closed conversation = %+v %v", conversation, err)
	}
	ordinary, err := Recover(context.Background(), cfg, Request{SessionID: source.ID})
	if err != nil || ordinary.SessionID != source.ID || !ordinary.Existing || len(runtime.launches) != 1 {
		t.Fatalf("default attachment followed old agent intent: %+v %v", ordinary, err)
	}
}

func TestAgentRecoveryDoesNotChangeMissingProjectOrDataRoot(t *testing.T) {
	for _, missing := range []string{"project", "data-root"} {
		t.Run(missing, func(t *testing.T) { testMissingAgentDirectory(t, missing) })
	}
}

func testMissingAgentDirectory(t *testing.T, missing string) {
	t.Helper()
	cfg, runtime, record := agentTransactionFixture(t)
	path := filepath.Join(cfg.SessionsDir, "missing")
	if missing == "project" {
		record.Agent.Directory = path
	} else {
		record.Agent.DataRoot = path
	}
	dir := filepath.Join(cfg.SessionsDir, "7K3D")
	if err := Write(dir, record); err != nil {
		t.Fatal(err)
	}
	if _, err := Recover(context.Background(), cfg, Request{SessionID: "7K3D"}); err == nil {
		t.Fatal("agent recovery changed an unavailable saved directory")
	}
	if len(runtime.launches) != 0 {
		t.Fatal("missing context still launched a provider")
	}
	if _, err := os.Stat(filepath.Join(dir, "recovery.json")); err != nil {
		t.Fatal(err)
	}
	if result, err := Recover(context.Background(), cfg, Request{SessionID: "7K3D", Action: ActionShell}); err != nil || result.Cwd != cfg.SessionsDir {
		t.Fatalf("explicit shell unavailable: %+v %v", result, err)
	}
}

func TestAgentRecoveryWithoutRegistrationNeverLaunchesAnAgent(t *testing.T) {
	cfg, runtime := transactionFixture(t)
	if _, err := Recover(context.Background(), cfg, Request{SessionID: "7K3D", Action: ActionAgent}); err == nil {
		t.Fatal("unregistered provider recovery was accepted")
	}
	if len(runtime.launches) != 0 {
		t.Fatal("unregistered source launched a provider")
	}
	result, err := Recover(context.Background(), cfg, Request{SessionID: "7K3D"})
	if err != nil || result.AgentStatus != "" || runtime.launches[0].Agent != nil {
		t.Fatalf("ordinary shell recovery = %+v %v", result, err)
	}
}

func TestAgentRecoveryActiveSurvivingWorkerNeverStartsSecondProvider(t *testing.T) {
	cfg, runtime, _ := agentTransactionFixture(t)
	source := runtime.sources["7K3D"]
	source.State = "running"
	runtime.sources[source.ID] = source
	result, err := Recover(context.Background(), cfg, Request{SessionID: source.ID, Action: ActionAgent})
	if err != nil || result.SessionID != source.ID || !result.Existing || len(runtime.launches) != 0 {
		t.Fatalf("active conversation duplicated: %+v %v", result, err)
	}
}

func TestAgentRecoveryLostLaunchReplyRetainsUnverifiedReplacement(t *testing.T) {
	cfg, runtime, _ := agentTransactionFixture(t)
	runtime.launchError = errors.New("lost launch acknowledgement")
	runtime.publishBeforeError = true
	if _, err := Recover(context.Background(), cfg, Request{SessionID: "7K3D"}); !errors.Is(err, ErrUncertain) {
		t.Fatalf("lost launch response = %v", err)
	}
	result, err := Recover(context.Background(), cfg, Request{SessionID: "7K3D"})
	if err != nil || result.AgentStatus != "unverified" || !result.Existing || len(runtime.launches) != 1 {
		t.Fatalf("retry duplicated or verified startup: %+v %v", result, err)
	}
	if _, err := Recover(context.Background(), cfg, Request{SessionID: "7K3D", Action: ActionShell}); err == nil {
		t.Fatal("shell request pretended the reserved agent terminal was a new shell")
	}
	if len(runtime.launches) != 1 {
		t.Fatal("shell request violated the reserved replacement")
	}
}

func TestAgentRecoveryMissingExecutableRetainsSourceWithoutReservingReplacement(t *testing.T) {
	cfg, runtime, record := agentTransactionFixture(t)
	record.Agent.Executable = filepath.Join(cfg.SessionsDir, "missing-provider")
	dir := filepath.Join(cfg.SessionsDir, "7K3D")
	if err := Write(dir, record); err != nil {
		t.Fatal(err)
	}
	if _, err := Recover(context.Background(), cfg, Request{SessionID: "7K3D"}); err == nil {
		t.Fatal("missing executable was substituted")
	}
	if len(runtime.launches) != 0 {
		t.Fatal("unavailable executable created a replacement")
	}
	if _, err := os.Stat(filepath.Join(dir, "recovery-intent.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unavailable provider reserved an intent: %v", err)
	}
}

func TestAgentRecoveryNewInvocationDoesNotFollowLiveShellsOlderConversation(t *testing.T) {
	cfg, runtime, record := agentTransactionFixture(t)
	source := runtime.sources["7K3D"]
	source.State = "detached"
	runtime.sources[source.ID] = source
	record.Agent.Lifecycle = agentresume.Closed
	dir := filepath.Join(cfg.SessionsDir, source.ID)
	if err := Write(dir, record); err != nil {
		t.Fatal(err)
	}
	first, err := Recover(context.Background(), cfg, Request{SessionID: source.ID, Action: ActionAgent})
	if err != nil {
		t.Fatal(err)
	}
	oldID := record.Agent.ConversationID
	record.Agent.ConversationID = "newer-conversation"
	record.Agent.InvocationToken = "newer-invocation"
	record.Agent.Lifecycle = agentresume.Active
	if err := Write(dir, record); err != nil {
		t.Fatal(err)
	}
	source.State = "interrupted"
	runtime.sources[source.ID] = source
	second, err := Recover(context.Background(), cfg, Request{SessionID: source.ID})
	if err != nil || second.SessionID == first.SessionID || len(runtime.launches) != 2 {
		t.Fatalf("new invocation followed old recovery claim: first=%+v second=%+v error=%v", first, second, err)
	}
	oldPlan, err := PendingAgent(cfg.SessionsDir, cfg.HostID, source.ID, first.SessionID)
	if err != nil || oldPlan.ConversationID != oldID {
		t.Fatalf("old replacement lost frozen plan: %+v %v", oldPlan, err)
	}
	newPlan, err := PendingAgent(cfg.SessionsDir, cfg.HostID, source.ID, second.SessionID)
	if err != nil || newPlan.ConversationID != "newer-conversation" {
		t.Fatalf("new replacement used stale plan: %+v %v", newPlan, err)
	}
	current, err := ReplacementID(dir, cfg.HostID, source.ID)
	if err != nil || current != second.SessionID {
		t.Fatalf("catalog followed old claim: %q %v", current, err)
	}
}

func TestAgentDefaultAndExplicitRecoveryShareOneInvocationClaim(t *testing.T) {
	cfg, runtime, _ := agentTransactionFixture(t)
	var callers sync.WaitGroup
	failures := make(chan error, 12)
	results := make(chan Result, 12)
	for index := range 12 {
		callers.Go(func() {
			action := ActionDefault
			if index%2 == 0 {
				action = ActionAgent
			}
			result, err := Recover(context.Background(), cfg, Request{SessionID: "7K3D", Action: action})
			failures <- err
			results <- result
		})
	}
	callers.Wait()
	close(failures)
	close(results)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(runtime.launches) != 1 {
		t.Fatalf("default and explicit requests created %d providers", len(runtime.launches))
	}
	for result := range results {
		if result.SessionID != runtime.launches[0].ID || result.AgentStatus != "unverified" {
			t.Fatalf("mixed action result = %+v", result)
		}
	}
}

func TestAgentReplacementReferenceRejectsWrongOwnerAndTraversal(t *testing.T) {
	cfg, _, _ := agentTransactionFixture(t)
	result, err := Recover(context.Background(), cfg, Request{SessionID: "7K3D"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PendingAgent(cfg.SessionsDir, "another-host", "7K3D", result.SessionID); err == nil {
		t.Fatal("wrong host consumed agent plan")
	}
	reference := agentReference{Version: 1, HostID: cfg.HostID, SourceID: "7K3D", ReplacementID: result.SessionID,
		Claim: "../../recovery-intent.json"}
	if err := writeTransaction(filepath.Join(cfg.SessionsDir, result.SessionID), "agent-recovery.json", reference); err != nil {
		t.Fatal(err)
	}
	if _, err := PendingAgent(cfg.SessionsDir, cfg.HostID, "7K3D", result.SessionID); err == nil {
		t.Fatal("replacement reference accepted path traversal")
	}
}

func TestClosedConversationClaimDoesNotHijackLaterShellRecovery(t *testing.T) {
	cfg, runtime, record := agentTransactionFixture(t)
	source := runtime.sources["7K3D"]
	source.State = "detached"
	runtime.sources[source.ID] = source
	record.Agent.Lifecycle = agentresume.Closed
	dir := filepath.Join(cfg.SessionsDir, source.ID)
	if err := Write(dir, record); err != nil {
		t.Fatal(err)
	}
	conversation, err := Recover(context.Background(), cfg, Request{SessionID: source.ID, Action: ActionAgent})
	if err != nil {
		t.Fatal(err)
	}
	source.State = "interrupted"
	runtime.sources[source.ID] = source
	shell, err := Recover(context.Background(), cfg, Request{SessionID: source.ID, Action: ActionShell})
	if err != nil || shell.SessionID == conversation.SessionID || shell.AgentStatus != "" {
		t.Fatalf("closed conversation blocked shell recovery: %+v %v", shell, err)
	}
	current, err := ReplacementID(dir, cfg.HostID, source.ID)
	if err != nil || current != shell.SessionID {
		t.Fatalf("catalog did not prefer shell replacement: %q %v", current, err)
	}
	again, err := Recover(context.Background(), cfg, Request{SessionID: source.ID})
	if err != nil || again.SessionID != shell.SessionID {
		t.Fatalf("default recovery followed closed agent claim: %+v %v", again, err)
	}
	explicit, err := Recover(context.Background(), cfg, Request{SessionID: source.ID, Action: ActionAgent})
	if err != nil || explicit.SessionID != conversation.SessionID || len(runtime.launches) != 2 {
		t.Fatalf("explicit conversation lost its own claim: %+v %v", explicit, err)
	}
}
