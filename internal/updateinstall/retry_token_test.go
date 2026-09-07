package updateinstall

import (
	"context"
	"testing"
)

func TestExplicitRetryTokenCannotReactivateAfterSecondRollback(t *testing.T) {
	f := newFixture(t)
	f.stage()
	f.manager.failCandidate = true
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := f.engine.Grant(context.Background(), f.request.ID, 1); err != nil {
			t.Fatal(err)
		}
		status, err := f.engine.Run(context.Background())
		if err == nil || status.Phase != RolledBack {
			t.Fatalf("expected rollback, got %s %v", status.Phase, err)
		}
		if attempt == 0 {
			status, err = f.engine.RetryWithToken(context.Background(), f.request.ID, 1, 2)
			if err != nil || status.Phase != Staged {
				t.Fatalf("retry: %s %v", status.Phase, err)
			}
		}
	}
	stops := f.manager.stops
	status, err := f.engine.RetryWithToken(context.Background(), f.request.ID, 1, 2)
	if err != nil || status.Phase != RolledBack || status.RetryToken != 2 {
		t.Fatalf("replayed retry changed receipt: %+v %v", status, err)
	}
	if f.manager.stops != stops {
		t.Fatal("replayed retry restarted daemon")
	}
	f.roundTrip()
}
