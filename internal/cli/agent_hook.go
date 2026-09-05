package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/spf13/cobra"

	"github.com/shaul/mesh/internal/agentresume"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
)

func (a *application) agentHookCommand() *cobra.Command {
	return &cobra.Command{Use: "agent-hook PROVIDER", Hidden: true, DisableFlagParsing: true,
		Run: func(cmd *cobra.Command, args []string) {
			_ = reportAgentHook(cmd, args)
		},
	}
}

func reportAgentHook(cmd *cobra.Command, args []string) error {
	if len(args) != 1 {
		return errors.New("agent hook requires one provider")
	}
	provider, err := parseAgentProvider(args[0])
	if err != nil {
		return err
	}
	token, socket := os.Getenv("MESH_AGENT_TOKEN"), os.Getenv("MESH_AGENT_SOCKET")
	owner := protocol.SessionIdentity{HostID: os.Getenv("MESH_AGENT_HOST_ID"), SessionID: os.Getenv("MESH_AGENT_SESSION_ID")}
	if token == "" || len(token) > 256 || protocol.ValidateSessionIdentity(owner) != nil {
		return errors.New("agent hook has no invocation route")
	}
	directory := filepath.Dir(socket)
	if !filepath.IsAbs(socket) || filepath.Clean(socket) != socket || paths.Socket(directory) != socket || filepath.Base(directory) != owner.SessionID {
		return errors.New("agent hook has an invalid worker socket")
	}
	event, err := agentresume.DecodeEvent(provider, cmd.InOrStdin())
	if err != nil || event.Subagent {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), agentRequestTimeout)
	defer cancel()
	response, err := daemonControlRequest(ctx, socket, protocol.Control{Type: protocol.TypeAgentEvent,
		SessionID: owner.SessionID, AgentHostID: owner.HostID, AgentToken: token, AgentProvider: provider, AgentEvent: &event})
	if err != nil || response.Type != protocol.TypeAgentRegistered || response.SessionID != owner.SessionID {
		return agentResponseError(response, err)
	}
	return nil
}

func (a *application) agentDoctorCommand() *cobra.Command {
	return &cobra.Command{Use: "doctor PROVIDER", Short: "Check provider recovery hooks and compatibility", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return diagnoseAgentRecovery(cmd, args[0]) },
	}
}

func diagnoseAgentRecovery(cmd *cobra.Command, name string) error {
	provider, err := parseAgentProvider(name)
	if err != nil {
		return err
	}
	executable, err := resolveAgentExecutable(provider)
	if err != nil {
		_, writeErr := fmt.Fprintf(cmd.OutOrStdout(), "%s: executable missing (%v)\n", provider, err)
		return writeErr
	}
	launch, err := inspectAgentLaunch(cmd.Context(), provider, executable, nil)
	if err != nil {
		_, writeErr := fmt.Fprintf(cmd.OutOrStdout(), "%s: recovery unavailable (%v)\n", provider, err)
		return writeErr
	}
	path, err := agentSettingsPath(provider)
	if err != nil {
		return err
	}
	settings, readErr := readAgentSettings(path)
	meshExecutable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate Mesh hook helper: %w", err)
	}
	status := agentHookSetupStatus(settings, readErr, provider, meshExecutable)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\nHooks: %s (%s)\n", provider, launch.ProviderVersion, status, path)
	if provider == agentresume.Codex {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Trust: review Mesh hooks in Codex; Mesh does not override hook trust or disabled hooks.")
	}
	if reason := agentresume.Compatibility(launch); reason != "" {
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Automatic recovery: unavailable (%s)\nExplicit binding: mesh agent bind %s CONVERSATION_ID\n", reason, provider)
		return err
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), "Automatic recovery: identity remains unverified until a hook is durably acknowledged by its Mesh worker.")
	return err
}

func agentHookSetupStatus(settings []byte, readErr error, provider agentresume.Provider, executable string) string {
	if errors.Is(readErr, os.ErrNotExist) {
		return "missing"
	}
	if readErr != nil {
		return "unreadable"
	}
	var config struct {
		DisableAllHooks bool                         `json:"disableAllHooks"`
		Hooks           map[string][]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(settings, &config); err != nil {
		return "invalid JSON"
	}
	if config.DisableAllHooks {
		return "disabled"
	}
	command, err := agentresume.StableHookCommand(provider, executable)
	if err != nil {
		return "Mesh hook unavailable"
	}
	commands, err := agentHookCommands(config.Hooks["SessionStart"])
	if err != nil {
		return "invalid hook configuration"
	}
	if !slices.Contains(commands, command) {
		return "Mesh hook missing"
	}
	return "configured; delivery unverified"
}
