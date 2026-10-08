package cli

import (
	"strings"
	"testing"
)

func TestServePrivateHostReachesOriginAndCatalog(t *testing.T) {
	host := setupCommandTestHost(t)
	stdout, _, err := executeCommand(t, Dependencies{DialControl: host.dial}, "serve", "pc", "3301", "--at", "/platform", "--private-host", "fregat", "--isolate")
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
	rows := serviceAliasRows([]ServiceCatalogRow{{Host: HostRecord{}, PrivateName: "pc.mesh.mesh.test", Service: host.services[0]}})
	if len(rows) != 2 || serviceRoute(rows[0].Service) != "/" || rows[0].URL() != "https://fregat.mesh.test/" || rows[1].URL() != "https://pc.mesh.mesh.test/platform" {
		t.Fatalf("catalog aliases = %#v", rows)
	}
}
