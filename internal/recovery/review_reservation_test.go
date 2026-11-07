package recovery

import (
	"testing"

	"github.com/shaul/mesh/internal/session"
)

func TestExecutionReviewReservedRecoveryAcrossBoot(t *testing.T) {
	cfg, runtime := transactionFixture(t)
	cfg.BootID = "first-boot"
	runtime.boot = cfg.BootID
	ref, err := runtime.Reserve(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	source := runtime.sources["7K3D"]
	launch, cwd, err := chooseLaunch(source, fallbackRecord(cfg.HostID, source), Request{SessionID: source.ID})
	if err != nil {
		t.Fatal(err)
	}
	launch.ID, launch.Reservation = ref.ID, ref
	saved := intent{Version: 1, HostID: cfg.HostID, SourceID: source.ID, Phase: "reserved", Launch: launch, OriginalCwd: cwd}
	cfg.BootID, runtime.boot = "second-boot", "second-boot"
	got, err := renewRecoveryIntent(t.Context(), cfg, runtime.root+"/7K3D", saved)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("phase=%s reservation boot=%s current boot=%s purpose=%s", got.Phase, got.Launch.Reservation.BootID, cfg.BootID, got.Launch.Reservation.Purpose)
	if got.Launch.Reservation.Purpose != session.Recovery || got.Launch.Reservation.BootID != cfg.BootID {
		t.Fatal("reserved recovery keeps old-boot reference and the real launcher refuses it")
	}
}
