package daemon

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"
)

func TestNetworkOwnerRateExemptionRequiresResolvedOwner(t *testing.T) {
	if networkOwnerRateExemption(nil) != nil {
		t.Fatal("disabled owner access installed an exemption")
	}
	address := netip.MustParseAddr("100.64.0.2")
	for _, test := range []struct {
		name   string
		owners []string
		err    error
		want   bool
	}{
		{name: "owner", owners: []string{"owner"}, want: true},
		{name: "visitor"},
		{name: "discovery failure", owners: []string{"owner"}, err: errors.New("unavailable")},
	} {
		exempt := networkOwnerRateExemption(func(ctx context.Context, got netip.Addr) ([]string, error) {
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > 2*time.Second || got != address {
				t.Fatalf("unbounded discovery or wrong client address: deadline %v, address %v", deadline, got)
			}
			return test.owners, test.err
		})
		if got := exempt(context.Background(), address); got != test.want {
			t.Errorf("%s: exempt %v, want %v", test.name, got, test.want)
		}
	}
}
