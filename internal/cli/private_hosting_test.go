package cli

import (
	"slices"
	"strings"
	"testing"
)

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

func TestServeTrustOriginCanonicalizesAndClearsOnReservice(t *testing.T) {
	host := setupCommandTestHost(t)
	stdout, _, err := executeCommand(t, Dependencies{DialControl: host.dial}, "serve", "pc", "3301", "--at", "/relay", "--private-host", "relay", "--trust-origin", "HTTPS://bun-demo.sprockt.dev:443", "--trust-origin", "https://other.example/")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "trusted origins: https://bun-demo.sprockt.dev, https://other.example") {
		t.Fatalf("serve output = %q", stdout)
	}
	host.mu.Lock()
	if len(host.services) != 1 || !strings.EqualFold(host.services[0].PrivateHost, "relay.mesh.test") || !slices.Equal(host.services[0].TrustedOrigins, []string{"https://bun-demo.sprockt.dev", "https://other.example"}) {
		got := host.services
		host.mu.Unlock()
		t.Fatalf("published service = %#v", got)
	}
	host.mu.Unlock()

	if _, _, err := executeCommand(t, Dependencies{DialControl: host.dial}, "serve", "pc", "3302", "--at", "/relay"); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.services) != 1 || len(host.services[0].TrustedOrigins) != 0 || host.services[0].PrivateHost != "relay.mesh.test" {
		t.Fatalf("reservice did not preserve private host and clear origins: %#v", host.services)
	}
}
