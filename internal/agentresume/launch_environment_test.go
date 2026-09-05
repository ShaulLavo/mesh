package agentresume

import (
	"strings"
	"testing"
)

func TestLaunchRejectsUnrecordedClaudeBackendEnvironment(t *testing.T) {
	for _, setting := range []string{
		"CLAUDE_CODE_USE_BEDROCK=1",
		"CLAUDE_CODE_USE_VERTEX=1",
		"CLAUDE_CODE_USE_FOUNDRY=1",
		"CLAUDE_CODE_USE_MANTLE=1",
		"CLAUDE_CODE_USE_ANTHROPIC_AWS=1",
		"ANTHROPIC_BASE_URL=https://gateway.example.test",
		"ANTHROPIC_BEDROCK_BASE_URL=https://gateway.example.test",
		"ANTHROPIC_BEDROCK_MANTLE_BASE_URL=https://gateway.example.test",
		"ANTHROPIC_VERTEX_BASE_URL=https://gateway.example.test",
		"ANTHROPIC_FOUNDRY_BASE_URL=https://gateway.example.test",
		"ANTHROPIC_AWS_BASE_URL=https://gateway.example.test",
	} {
		t.Run(setting, func(t *testing.T) {
			launch, err := ParseLaunch(Claude, "/bin/claude", "2.1.261 (Claude Code)", "/project",
				[]string{"HOME=/home/test", setting}, nil)
			if err == nil && Compatibility(launch) == "" {
				t.Fatalf("accepted automatic recovery without saving the provider routing selected by %s", setting)
			}
			if err != nil && strings.Contains(err.Error(), "gateway.example.test") {
				t.Fatalf("recovery diagnostic exposed the routing value: %v", err)
			}
		})
	}
}

func TestLaunchAllowsInactiveAndOtherProviderEnvironment(t *testing.T) {
	for _, provider := range []Provider{Claude, Codex} {
		env := []string{"HOME=/home/test", "CLAUDE_CODE_USE_BEDROCK=0", "CLAUDE_CODE_USE_VERTEX=false", "CLAUDE_CODE_USE_FOUNDRY=FALSE", "ANTHROPIC_BASE_URL="}
		if provider == Codex {
			env = append(env, "CLAUDE_CODE_USE_BEDROCK=1", "ANTHROPIC_BASE_URL=https://unrelated.example.test")
		}
		if _, err := ParseLaunch(provider, "/bin/provider", "version", "/project", env, nil); err != nil {
			t.Fatalf("irrelevant routing disabled %s capture: %v", provider, err)
		}
	}
}
