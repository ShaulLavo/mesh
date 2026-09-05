package main

import "testing"

func TestNativeAgentCommandsUsePlainTerminalOutput(t *testing.T) {
	for _, name := range []string{"agent", "agent-hook", "agent-resume"} {
		if !plainAgentCommand([]string{name, "claude"}) {
			t.Fatalf("%s would query terminal colors before returning an error", name)
		}
	}
	for _, arguments := range [][]string{nil, {"recover", "7K3D"}, {"daemon"}, {"--window"}} {
		if plainAgentCommand(arguments) {
			t.Fatalf("unrelated command bypasses normal rendering: %v", arguments)
		}
	}
}
