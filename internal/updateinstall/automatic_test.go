package updateinstall

import (
	"context"
	"errors"
	"testing"
)

func TestAutomaticJournalRequiresAuthorityEvenWhenOriginOmittedFromGrant(t *testing.T) {
	f := newFixture(t)
	f.request.Automatic = &AutomaticReference{PolicyID: "policy-1", CoordinatorID: "coordinator", Membership: f.request.Manifest.Digest(), Epoch: 1, ConsentRevision: 1}
	if _, err := f.engine.Stage(context.Background(), f.request); err == nil {
		t.Fatal("automatic stage accepted without authority")
	}
	allowed := true
	f.engine = f.engine.WithAcceptance(func(_ context.Context, _ Request, _ AcceptanceKind, commit func() error) error {
		if !allowed {
			return errors.New("consent revoked")
		}
		return commit()
	})
	staged, err := f.engine.Stage(context.Background(), f.request)
	if err != nil {
		t.Fatal(err)
	}
	allowed = false
	if _, err := f.engine.Grant(context.Background(), f.request.ID, f.request.Generation); err == nil {
		t.Fatal("omitting origin bypassed journal's automatic grant authority")
	}
	if _, err := f.engine.Stage(context.Background(), f.request); err == nil {
		t.Fatal("equal stage bypassed revoked consent")
	}
	current, err := f.engine.Read()
	if err != nil || current.Phase != staged.Phase {
		t.Fatalf("revoked authority changed journal: %+v %v", current, err)
	}
	allowed = true
	if _, err := f.engine.Grant(context.Background(), f.request.ID, f.request.Generation); err != nil {
		t.Fatal(err)
	}
	allowed = false
	if _, err := f.engine.Grant(context.Background(), f.request.ID, f.request.Generation); err == nil {
		t.Fatal("granted replay bypassed current consent")
	}
	f.roundTrip()
}
