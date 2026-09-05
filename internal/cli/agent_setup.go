package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/spf13/cobra"

	"github.com/shaul/mesh/internal/agentresume"
)

const maximumAgentSettingsBytes = 1 << 20

func (a *application) agentSetupCommand() *cobra.Command {
	var install, uninstall bool
	var path string
	command := &cobra.Command{
		Use: "setup PROVIDER", Short: "Print or install stable conversation recovery hooks", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return setupAgentHooks(cmd, args[0], path, install, uninstall)
		},
	}
	command.Flags().BoolVar(&install, "install", false, "merge Mesh hooks into provider settings")
	command.Flags().BoolVar(&uninstall, "uninstall", false, "remove Mesh hooks while preserving other settings")
	command.Flags().StringVar(&path, "path", "", "provider hook settings file")
	command.MarkFlagsMutuallyExclusive("install", "uninstall")
	return command
}

func setupAgentHooks(cmd *cobra.Command, name, path string, install, uninstall bool) error {
	provider, err := parseAgentProvider(name)
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate Mesh hook helper: %w", err)
	}
	fragment, err := agentresume.StableHookFragment(provider, executable)
	if err != nil {
		return err
	}
	if !install && !uninstall {
		_, err = fmt.Fprintln(cmd.OutOrStdout(), string(fragment))
		return err
	}
	if path == "" {
		path, err = agentSettingsPath(provider)
	}
	if err != nil {
		return err
	}
	current, err := readAgentSettings(path)
	if errors.Is(err, os.ErrNotExist) && uninstall {
		return nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	updated, err := mergeAgentHooks(current, fragment, uninstall)
	if err != nil {
		return err
	}
	if err := writeAgentSettings(path, updated); err != nil {
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Updated %s\n", path)
	if provider == agentresume.Codex && install {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Review and trust the Mesh hooks in Codex's normal hook trust flow. Run mesh agent doctor codex to check the installed setup.")
	}
	return err
}

func agentSettingsPath(provider agentresume.Provider) (string, error) {
	variable, directory, file := "CLAUDE_CONFIG_DIR", ".claude", "settings.json"
	if provider == agentresume.Codex {
		variable, directory, file = "CODEX_HOME", ".codex", "hooks.json"
	}
	root := os.Getenv(variable)
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate provider home: %w", err)
		}
		root = filepath.Join(home, directory)
	}
	return filepath.Abs(filepath.Join(root, file))
}

func readAgentSettings(path string) ([]byte, error) {
	file, err := os.Open(path) //nolint:gosec // explicitly selected provider settings file
	if err != nil {
		return nil, fmt.Errorf("read provider hooks: %w", err)
	}
	defer file.Close() //nolint:errcheck // read-only settings
	data, err := io.ReadAll(io.LimitReader(file, maximumAgentSettingsBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read provider hooks: %w", err)
	}
	if len(data) > maximumAgentSettingsBytes {
		return nil, errors.New("provider hook settings exceed 1 MiB")
	}
	return data, nil
}

func mergeAgentHooks(current, fragment []byte, remove bool) ([]byte, error) {
	settings := make(map[string]json.RawMessage)
	if len(bytes.TrimSpace(current)) > 0 {
		if err := json.Unmarshal(current, &settings); err != nil {
			return nil, fmt.Errorf("decode provider settings: %w", err)
		}
	}
	if settings == nil {
		return nil, errors.New("provider settings must be an object")
	}
	var addition struct {
		Hooks map[string][]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(fragment, &addition); err != nil {
		return nil, fmt.Errorf("decode Mesh hook fragment: %w", err)
	}
	hooks := make(map[string][]json.RawMessage)
	if raw, found := settings["hooks"]; found {
		if err := json.Unmarshal(raw, &hooks); err != nil {
			return nil, fmt.Errorf("decode provider hook groups: %w", err)
		}
	}
	if hooks == nil {
		return nil, errors.New("provider hooks must be an object")
	}
	for event, entries := range addition.Hooks {
		merged, err := mergeAgentHookEvent(hooks[event], entries, remove)
		if err != nil {
			return nil, err
		}
		if len(merged) == 0 {
			delete(hooks, event)
			continue
		}
		hooks[event] = merged
	}
	encoded, err := json.Marshal(hooks)
	if err != nil {
		return nil, fmt.Errorf("encode provider hooks: %w", err)
	}
	settings["hooks"] = encoded
	if len(hooks) == 0 {
		delete(settings, "hooks")
	}
	result, err := json.MarshalIndent(settings, "", "  ")
	return append(result, '\n'), err
}

func mergeAgentHookEvent(current, additions []json.RawMessage, remove bool) ([]json.RawMessage, error) {
	commands, err := agentHookCommands(additions)
	if err != nil {
		return nil, err
	}
	var result []json.RawMessage
	for _, entry := range current {
		filtered, err := removeAgentHookCommands(entry, commands)
		if err != nil {
			return nil, err
		}
		if filtered != nil {
			result = append(result, filtered)
		}
	}
	if !remove {
		result = append(result, additions...)
	}
	return result, nil
}

func agentHookCommands(entries []json.RawMessage) ([]string, error) {
	var result []string
	for _, entry := range entries {
		commands, err := agentHookGroupCommands(entry)
		if err != nil {
			return nil, err
		}
		result = append(result, commands...)
	}
	return result, nil
}

func agentHookGroupCommands(entry json.RawMessage) ([]string, error) {
	var group struct {
		Hooks []struct {
			Type    string `json:"type"`
			Command string `json:"command"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(entry, &group); err != nil {
		return nil, fmt.Errorf("decode hook command: %w", err)
	}
	var commands []string
	for _, hook := range group.Hooks {
		if hook.Type == "command" {
			commands = append(commands, hook.Command)
		}
	}
	return commands, nil
}

func removeAgentHookCommands(entry json.RawMessage, commands []string) (json.RawMessage, error) {
	var group map[string]json.RawMessage
	if err := json.Unmarshal(entry, &group); err != nil {
		return nil, fmt.Errorf("decode existing hook group: %w", err)
	}
	var hooks []json.RawMessage
	if err := json.Unmarshal(group["hooks"], &hooks); err != nil {
		return nil, fmt.Errorf("decode existing hooks: %w", err)
	}
	kept := make([]json.RawMessage, 0, len(hooks))
	for _, raw := range hooks {
		var hook struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(raw, &hook); err != nil {
			return nil, fmt.Errorf("decode existing hook: %w", err)
		}
		if !slices.Contains(commands, hook.Command) {
			kept = append(kept, raw)
		}
	}
	if len(kept) == 0 {
		return nil, nil
	}
	group["hooks"], _ = json.Marshal(kept)
	return json.Marshal(group)
}

func writeAgentSettings(path string, data []byte) error {
	path, err := agentSettingsWriteTarget(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create provider settings directory: %w", err)
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".mesh-agent-hooks-*")
	if err != nil {
		return fmt.Errorf("create provider settings update: %w", err)
	}
	defer os.Remove(file.Name()) //nolint:errcheck // remove unpublished temporary settings
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write provider settings: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close provider settings: %w", err)
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return fmt.Errorf("publish provider settings: %w", err)
	}
	return nil
}

func agentSettingsWriteTarget(path string) (string, error) {
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return path, nil
	}
	if err != nil {
		return "", fmt.Errorf("inspect provider settings: %w", err)
	}
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve provider settings target: %w", err)
	}
	return target, nil
}
