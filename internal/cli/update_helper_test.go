package cli

import (
	"testing"

	"github.com/shaul/mesh/internal/updateinstall"
)

func TestHelperUpgradeReusesPersistedDaemonServiceDomain(t *testing.T) {
	settings := updateinstall.Settings{Service: updateinstall.ServiceSpec{Kind: "launchd", Domain: "user/501"}}
	kind, domain := helperUpgradeService(settings)
	if kind != "launchd" || domain != "user/501" {
		t.Fatalf("helper upgrade service = %s %s, want persisted launchd user domain", kind, domain)
	}
}
