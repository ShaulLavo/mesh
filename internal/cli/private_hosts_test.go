package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestServePrivateHostReachesOriginAndCatalog(t *testing.T) {
	host := setupCommandTestHost(t)
	stdout, _, err := executeCommand(t, Dependencies{DialControl: host.dial}, "serve", "pc", "3301", "--at", "/platform", "--private-host", "Fregat", "--isolate")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "https://fregat.mesh.test/") {
		t.Fatalf("serve output = %q", stdout)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.services) != 1 || host.services[0].PrivateHost != "fregat.mesh.test" || host.services[0].Name != "platform" {
		t.Fatalf("services = %#v", host.services)
	}
	rows := []ServiceCatalogRow{{Host: HostRecord{}, PrivateName: "pc.mesh.mesh.test", Service: host.services[0]}}
	var table bytes.Buffer
	if err := writeServiceTable(&table, rows, nil); err != nil {
		t.Fatal(err)
	}
	if serviceRoute(rows[0].Service) != "/" || rows[0].URL() != "https://fregat.mesh.test/" || strings.Contains(table.String(), "pc.mesh.mesh.test/platform") || strings.Count(table.String(), "https://fregat.mesh.test/") != 1 {
		t.Fatalf("catalog = %s", table.String())
	}
}

func TestServeRetainsPrivateHostUnlessExplicitlyCleared(t *testing.T) {
	host := setupCommandTestHost(t)
	for _, args := range [][]string{
		{"serve", "pc", "3301", "--at", "/platform", "--private-host", "fregat"},
		{"serve", "pc", "3302", "--at", "/platform"},
		{"serve", "pc", "3303", "--at", "/platform", "--private-host="},
	} {
		if _, _, err := executeCommand(t, Dependencies{DialControl: host.dial}, args...); err != nil {
			t.Fatal(err)
		}
		want := "fregat.mesh.test"
		if args[2] == "3303" {
			want = ""
		}
		host.mu.Lock()
		got := host.services[0].PrivateHost
		host.mu.Unlock()
		if got != want {
			t.Fatalf("%v: private host %q, want %q", args, got, want)
		}
	}
}
