package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const codexTrustTestCommand = "'/home/test/.local/bin/mesh' agent-hook codex"

func codexTrustFixture(t *testing.T, config string) (string, []byte, string) {
	t.Helper()
	home := t.TempDir()
	hooksPath := filepath.Join(home, "hooks.json")
	settings := []byte(`{"hooks":{
		"SessionStart":[{"hooks":[{"type":"command","command":"other"}]},{"hooks":[{"type":"command","command":"'/home/test/.local/bin/mesh' agent-hook codex","timeout":2}]}],
		"SessionEnd":[{"hooks":[{"type":"command","command":"'/home/test/.local/bin/mesh' agent-hook codex","timeout":2}]}]}}`)
	configPath := filepath.Join(home, "config.toml")
	if config != "" {
		config = strings.ReplaceAll(config, "HOOKS", hooksPath)
		if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return hooksPath, settings, configPath
}

func TestCodexHookTrustFindsApprovalsAtTheMeshHooksPositions(t *testing.T) {
	approved := `model = "x"
[projects."/work"]
trust_level = "trusted"

[hooks.state]

[hooks.state."HOOKS:session_start:1:0"]
trusted_hash = "sha256:a"

[hooks.state."HOOKS:session_end:0:0"]
trusted_hash = "sha256:b"
`
	for _, tc := range []struct {
		name, config, want string
	}{
		{"approved", approved, "approved in Codex"},
		{"never reviewed", "", "not approved yet"},
		{"only the start hook", strings.Replace(approved, "session_end", "session_other", 1), "not approved yet"},
		{"approval of another hook position", strings.Replace(approved, "session_start:1:0", "session_start:0:0", 1), "not approved yet"},
		{"disabled", approved + "enabled = false\n", "disabled in Codex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hooksPath, settings, configPath := codexTrustFixture(t, tc.config)
			if got := codexHookTrust(hooksPath, settings, codexTrustTestCommand, configPath); !strings.HasPrefix(got, tc.want) {
				t.Fatalf("trust = %q, want prefix %q", got, tc.want)
			}
		})
	}
	hooksPath, settings, configPath := codexTrustFixture(t, approved)
	if got := codexHookTrust(hooksPath, settings, "'/elsewhere/mesh' agent-hook codex", configPath); !strings.HasPrefix(got, "not checked") {
		t.Fatalf("trust without installed Mesh hooks = %q", got)
	}
}
