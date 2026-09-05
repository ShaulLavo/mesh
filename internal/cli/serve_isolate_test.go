package cli

import (
	"strings"
	"testing"
)

func TestServeIsolateFlagReachesTheHost(t *testing.T) {
	host := setupCommandTestHost(t)
	stdout, _, err := executeCommand(t, Dependencies{DialControl: host.dial}, "serve", "pc", "3000", "--at", "/app", "--isolate")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "serving https://pc.mesh.shaulavo.dev/app on pc (proxy -> 3000)") {
		t.Fatalf("serve output = %q", stdout)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.services) != 1 || host.services[0].Name != "app" || !host.services[0].Isolate {
		t.Fatalf("host services = %#v, want app with isolate set", host.services)
	}
}
