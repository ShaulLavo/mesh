//go:build linux || darwin

package worker

import (
	"slices"
	"strings"
	"testing"
)

func TestAgentProcessTestEnvDoesNotInheritCallerSettings(t *testing.T) {
	for _, name := range []string{"ANTHROPIC_BASE_URL", "OPENAI_API_KEY", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "HTTP_PROXY", "https_proxy", "MESH_SESSION_ID", "UNRELATED_CALLER_SETTING"} {
		t.Setenv(name, "fixture-caller-value")
	}
	dir := t.TempDir()
	env := agentProcessTestEnv("native", dir)
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if !slices.Contains([]string{"PATH", "HOME", "TMPDIR", "TERM", "LANG", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS", "MESH_TEST_AGENT_ROLE", "MESH_TEST_AGENT_DIR"}, name) {
			t.Errorf("agent test process inherited %s", name)
		}
	}
	if !slices.Contains(env, "HOME="+dir) {
		t.Error("agent test process home is not isolated")
	}
}
