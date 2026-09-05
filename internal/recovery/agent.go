package recovery

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/shaul/mesh/internal/agentresume"
)

type agentReference struct {
	Version       int    `json:"version"`
	HostID        string `json:"hostId"`
	SourceID      string `json:"sourceId"`
	ReplacementID string `json:"replacementId"`
	Claim         string `json:"claim"`
}

func agentIntentName(token string) string {
	digest := sha256.Sum256([]byte(token))
	return "agent-intent-" + hex.EncodeToString(digest[:]) + ".json"
}

func intentName(saved intent) string {
	if saved.Launch.Agent != nil {
		return agentIntentName(saved.Launch.Agent.InvocationToken)
	}
	return "recovery-intent.json"
}

func recoveryClaim(dir, hostID, sourceID string, record Record, action Action) (string, error) {
	if record.Agent == nil {
		return "recovery-intent.json", nil
	}
	name := agentIntentName(record.Agent.InvocationToken)
	if action == ActionShell && record.Agent.Lifecycle != agentresume.Closed {
		if prior, err := readIntentNamed(dir, hostID, sourceID, name); err == nil {
			return "", fmt.Errorf("recovery: agent replacement %s is already reserved; attach to that terminal or open a new shell session", prior.Launch.ID)
		}
	}
	if action == ActionAgent || action == ActionDefault && record.Agent.Lifecycle != agentresume.Closed {
		return name, nil
	}
	return "recovery-intent.json", nil
}

func writeAgentReference(sessionsDir string, saved intent) error {
	if saved.Launch.Agent == nil {
		return nil
	}
	reference := agentReference{Version: 1, HostID: saved.HostID, SourceID: saved.SourceID,
		ReplacementID: saved.Launch.ID, Claim: intentName(saved)}
	return writeTransaction(filepath.Join(sessionsDir, saved.Launch.ID), "agent-recovery.json", reference)
}

func readAgentIntent(sessionsDir, hostID, sourceID, replacementID string) (intent, error) {
	if err := validateTarget(Target{HostID: hostID, SessionID: sourceID}); err != nil {
		return intent{}, err
	}
	if err := validateTarget(Target{HostID: hostID, SessionID: replacementID}); err != nil {
		return intent{}, err
	}
	var reference agentReference
	path := filepath.Join(sessionsDir, replacementID, "agent-recovery.json")
	if err := readTransaction(path, &reference); err != nil {
		return intent{}, err
	}
	if reference.Version != 1 || reference.HostID != hostID || reference.SourceID != sourceID || reference.ReplacementID != replacementID || !validAgentClaim(reference.Claim) {
		return intent{}, fmt.Errorf("recovery: invalid agent replacement reference")
	}
	saved, err := readIntentNamed(filepath.Join(sessionsDir, sourceID), hostID, sourceID, reference.Claim)
	if err != nil {
		return intent{}, err
	}
	if saved.Launch.ID != replacementID || saved.Launch.Agent == nil {
		return intent{}, fmt.Errorf("recovery: replacement %s does not match the frozen agent plan", replacementID)
	}
	return saved, nil
}

func validAgentClaim(name string) bool {
	value, ok := strings.CutPrefix(name, "agent-intent-")
	if !ok {
		return false
	}
	digest, ok := strings.CutSuffix(value, ".json")
	decoded, err := hex.DecodeString(digest)
	return ok && err == nil && len(decoded) == sha256.Size && strings.ToLower(digest) == digest
}

// A replacement that crashed before its first hook can retry its frozen plan.
// This launch-only value never becomes a checkpoint or an observed identity.
func pendingLaunchRecipe(saved intent) *agentresume.Recipe {
	recipe := *saved.Launch.Agent
	recipe.Options = slices.Clone(recipe.Options)
	recipe.Lifecycle = agentresume.Active
	return &recipe
}

func pendingReplacementRecipe(dir, hostID, id string) (*agentresume.Recipe, error) {
	var reference agentReference
	err := readTransaction(filepath.Join(dir, "agent-recovery.json"), &reference)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	pending, err := readAgentIntent(filepath.Dir(dir), hostID, reference.SourceID, id)
	if err != nil {
		return nil, err
	}
	return pendingLaunchRecipe(pending), nil
}

func chooseAgentLaunch(launch Launch, recipe *agentresume.Recipe) (Launch, string, error) {
	if recipe == nil {
		return Launch{}, "", fmt.Errorf("recovery: no registered agent conversation was saved")
	}
	if err := agentresume.ValidateRecipe(*recipe); err != nil {
		return Launch{}, "", err
	}
	if err := agentresume.CheckAvailable(*recipe); err != nil {
		return Launch{}, "", err
	}
	command, err := agentresume.ResumeCommand(*recipe)
	if err != nil {
		return Launch{}, "", err
	}
	copy := *recipe
	copy.Options = slices.Clone(recipe.Options)
	launch.Agent, launch.Cwd, launch.Command = &copy, recipe.Directory, command
	return launch, "", nil
}

// PendingAgent reads the frozen launch plan, never a source's newer binding.
// A nil recipe means the exact replacement was reserved for ordinary recovery.
func PendingAgent(sessionsDir, hostID, sourceID, replacementID string) (*agentresume.Recipe, error) {
	saved, err := readAgentIntent(sessionsDir, hostID, sourceID, replacementID)
	if errors.Is(err, os.ErrNotExist) {
		saved, err = readIntent(filepath.Join(sessionsDir, sourceID), hostID, sourceID)
	}
	if err != nil {
		return nil, err
	}
	if saved.Launch.ID != replacementID {
		return nil, fmt.Errorf("recovery: replacement %s is not reserved by source %s", replacementID, sourceID)
	}
	return saved.Launch.Agent, nil
}

// AgentStatus describes the exact replacement's durable identity acknowledgement.
func AgentStatus(sessionsDir, hostID, sourceID, replacementID string, receipt *agentresume.Receipt) string {
	saved, err := readAgentIntent(sessionsDir, hostID, sourceID, replacementID)
	if err != nil {
		return ""
	}
	if saved.Phase == "complete" || matchesAgentReceipt(saved, receipt) {
		return "verified"
	}
	return "unverified"
}
