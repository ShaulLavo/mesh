package cli

import (
	"testing"

	"github.com/shaul/mesh/internal/agentresume"
)

func TestAgentDoctorRequiresCurrentProviderStartupCommand(t *testing.T) {
	for _, settings := range []string{
		`{"note":"agent-hook"}`,
		`{"hooks":{"SessionEnd":[{"hooks":[{"type":"command","command":"'/work/bin/mesh' agent-hook claude"}]}]}}`,
		`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"'/work/bin/mesh' agent-hook codex"}]}]}}`,
		`{"hooks":{"SessionStart":[{"hooks":[{"type":"prompt","command":"'/work/bin/mesh' agent-hook claude"}]}]}}`,
		`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"'/old/mesh' agent-hook claude"}]}]}}`,
	} {
		if got := agentHookSetupStatus([]byte(settings), nil, agentresume.Claude, "/work/bin/mesh"); got != "Mesh hook missing" {
			t.Errorf("doctor accepted unrelated configuration: %s = %s", settings, got)
		}
	}
	installed, err := agentresume.StableHookFragment(agentresume.Claude, "/work/bin/mesh")
	if err != nil {
		t.Fatal(err)
	}
	if got := agentHookSetupStatus(installed, nil, agentresume.Claude, "/work/bin/mesh"); got != "configured; delivery unverified" {
		t.Fatalf("current startup hook = %s", got)
	}
}
