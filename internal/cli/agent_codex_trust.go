package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// codexHookReview is the one manual setup step Mesh cannot do for Codex, so
// setup and doctor both name it the same way.
const codexHookReview = "open Codex, run /hooks, and approve the Mesh SessionStart and SessionEnd hooks"

// codexTrustEvents maps hooks.json event names to the labels Codex uses in
// its trust keys, "<hooks.json>:<event>:<group>:<handler>".
var codexTrustEvents = map[string]string{"SessionStart": "session_start", "SessionEnd": "session_end"}

// codexHookTrust reports whether Codex has recorded an approval for each Mesh
// hook. It reads the record's presence only: Codex hashes a canonical form of
// the hook that Mesh does not reproduce, so a changed hook still reads as
// approved here until Codex itself asks again.
func codexHookTrust(hooksPath string, settings []byte, command, configPath string) string {
	keys, err := codexMeshHookKeys(hooksPath, settings, command)
	if err != nil || len(keys) == 0 {
		return "not checked (Mesh hooks are not installed)"
	}
	config, err := os.ReadFile(configPath) //nolint:gosec // the user's own Codex configuration
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Sprintf("unknown (read %s: %v)", configPath, err)
	}
	states := codexHookStates(config)
	for _, key := range keys {
		state := states[key]
		if state.disabled {
			return "disabled in Codex; " + codexHookReview
		}
		if !state.trusted {
			return "not approved yet; " + codexHookReview
		}
	}
	return "approved in Codex"
}

func codexMeshHookKeys(hooksPath string, settings []byte, command string) ([]string, error) {
	var config struct {
		Hooks map[string][]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(settings, &config); err != nil {
		return nil, fmt.Errorf("decode Codex hooks: %w", err)
	}
	var keys []string
	for event, label := range codexTrustEvents {
		for group, entry := range config.Hooks[event] {
			commands, err := agentHookGroupCommands(entry)
			if err != nil {
				return nil, err
			}
			for handler, current := range commands {
				if current == command {
					keys = append(keys, fmt.Sprintf("%s:%s:%d:%d", hooksPath, label, group, handler))
				}
			}
		}
	}
	return keys, nil
}

type codexHookState struct{ trusted, disabled bool }

// codexHookStates scans the [hooks.state."KEY"] tables of Codex's
// config.toml. It is a line reader rather than a TOML parser: the tables are
// written by Codex itself in this one flat shape.
func codexHookStates(config []byte) map[string]codexHookState {
	states := make(map[string]codexHookState)
	current := ""
	scanner := bufio.NewScanner(bytes.NewReader(config))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "[") {
			current = ""
			if key, ok := strings.CutPrefix(line, `[hooks.state."`); ok {
				current, _ = strings.CutSuffix(key, `"]`)
			}
			continue
		}
		if current == "" {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		state := states[current]
		switch strings.TrimSpace(name) {
		case "trusted_hash":
			state.trusted = strings.TrimSpace(value) != `""`
		case "enabled":
			state.disabled = strings.TrimSpace(value) == "false"
		}
		states[current] = state
	}
	return states
}

func codexConfigPath(hooksPath string) string {
	return filepath.Join(filepath.Dir(hooksPath), "config.toml")
}
