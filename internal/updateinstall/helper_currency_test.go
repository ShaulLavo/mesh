package updateinstall

import (
	"os"
	"strings"
	"testing"
)

func TestUpgradeHelperPromotesAcrossDatabaseStateVersions(t *testing.T) {
	f := newHelperUpgradeFixture(t, "systemd", "v0.1.170")
	binary, err := os.ReadFile(f.prior.Executable)
	if err != nil {
		t.Fatal(err)
	}
	f.replacePrior(t, []byte(strings.Replace(string(binary), `"stateVersion":7`, `"stateVersion":4`, 1)))
	f.commit(t, "v0.1.171", "")
	got, err := UpgradeHelper(t.Context(), f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got.Digest == f.prior.Digest {
		t.Fatal("helper remained on the old database state version")
	}
}
