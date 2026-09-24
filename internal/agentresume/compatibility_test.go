package agentresume

import "testing"

func TestVerifiedVersionAcceptsTheProbedFloorAndLaterReleasesOfItsLine(t *testing.T) {
	for _, tc := range []struct {
		provider Provider
		reported string
		want     bool
	}{
		{Claude, "2.1.261 (Claude Code)", true},
		{Claude, "2.1.281 (Claude Code)", true},
		{Claude, "2.2.0 (Claude Code)", true},
		{Claude, "2.1.260 (Claude Code)", false},
		{Claude, "3.0.0 (Claude Code)", false},
		{Claude, "2.1.281", false},
		{Claude, "2.1.281-beta (Claude Code)", false},
		{Claude, "2.1.0281 (Claude Code)", false},
		{Codex, "codex-cli 0.153.4", true},
		{Codex, "codex-cli 0.156.1", true},
		{Codex, "codex-cli 0.153.3", false},
		{Codex, "codex-cli 1.0.0", false},
		{Codex, "0.156.1", false},
		{Codex, "2.1.281 (Claude Code)", false},
	} {
		if got := verifiedVersion(tc.provider, tc.reported); got != tc.want {
			t.Errorf("verifiedVersion(%s, %q) = %v, want %v", tc.provider, tc.reported, got, tc.want)
		}
	}
}
