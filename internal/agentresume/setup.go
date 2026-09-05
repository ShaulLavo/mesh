package agentresume

import (
	"encoding/json"
	"fmt"
	"strings"
)

func StableHookCommand(provider Provider, meshExecutable string) (string, error) {
	if err := ValidateProvider(provider); err != nil {
		return "", err
	}
	if !absoluteField(meshExecutable) {
		return "", fmt.Errorf("agent recovery: hook executable must be an absolute bounded path")
	}
	return "'" + strings.ReplaceAll(meshExecutable, "'", "'\\''") + "' agent-hook " + string(provider), nil
}

func StableHookFragment(provider Provider, meshExecutable string) ([]byte, error) {
	command, err := StableHookCommand(provider, meshExecutable)
	if err != nil {
		return nil, err
	}
	type handler struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	}
	type group struct {
		Hooks []handler `json:"hooks"`
	}
	fragment := struct {
		Hooks map[string][]group `json:"hooks"`
	}{Hooks: map[string][]group{}}
	for _, name := range []string{"SessionStart", "SessionEnd"} {
		fragment.Hooks[name] = []group{{Hooks: []handler{{Type: "command", Command: command, Timeout: 2}}}}
	}
	return json.MarshalIndent(fragment, "", "  ")
}
