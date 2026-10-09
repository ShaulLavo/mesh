package cli

import "testing"

func TestHostingCommandSurfaceIsPrivate(t *testing.T) {
	root := NewCommand(Dependencies{})
	serve, _, err := root.Find([]string{"serve"})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"public", "wake-on-request", "allow-credentials", "yes"} {
		if serve.Flags().Lookup(name) != nil {
			t.Fatalf("serve still exposes %s", name)
		}
	}
	available := map[string]bool{}
	for _, child := range serve.Commands() {
		available[child.Name()] = true
	}
	for _, name := range []string{"ls", "label", "start", "stop"} {
		if !available[name] {
			t.Fatalf("private serve command %s missing", name)
		}
	}
	for _, command := range serve.Commands() {
		if command.Name() == "claim" || command.Name() == "release" {
			t.Fatalf("serve still exposes %s", command.Name())
		}
	}
	daemon, _, err := root.Find([]string{"daemon"})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"edge", "public-edge-target"} {
		if daemon.Flags().Lookup(name) != nil {
			t.Fatalf("daemon still exposes %s", name)
		}
	}
	if daemon.Flags().Lookup("app-registry-config") == nil || daemon.Flags().Lookup("app-registry-target") == nil {
		t.Fatal("private registry config flags missing")
	}
	un, _, err := root.Find([]string{"unserve"})
	if err != nil {
		t.Fatal(err)
	}
	if un.Flags().Lookup("local-edge") != nil {
		t.Fatal("unserve still exposes tunnel release")
	}
}
