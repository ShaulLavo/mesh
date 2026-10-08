package main

import "testing"

func TestNamingCommandKeepsNativeSessionsIndependent(t *testing.T) {
	for _, command := range []string{"spawn", "attach", "ls", "session-worker", "version", "update"} {
		if namingCommand([]string{command}) {
			t.Fatalf("native command reads deployment policy: %s", command)
		}
	}
	for _, command := range []string{"daemon", "serve", "unserve", "app", "private-names"} {
		if !namingCommand([]string{command}) {
			t.Fatalf("naming command misses deployment policy: %s", command)
		}
	}
}

func TestNamingCommandHandlesRootFlags(t *testing.T) {
	for _, args := range [][]string{{"--privacy", "serve"}, {"--leave-key", "ctrl+]", "app"}, {"--privacy=true", "daemon"}} {
		if !namingCommand(args) {
			t.Fatalf("missed naming command: %v", args)
		}
	}
	if namingCommand([]string{"--privacy", "spawn", "serve"}) {
		t.Fatal("spawn arguments enabled deployment policy")
	}
}
