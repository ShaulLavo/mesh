package mcpnative

import (
	"context"
	"testing"
)

type leaseRecorder struct {
	stubControls
	leases []LeaseServiceRequest
}

func (r *leaseRecorder) LeaseService(_ context.Context, req LeaseServiceRequest) (CallOutcome, error) {
	r.leases = append(r.leases, req)
	return CallOutcome{Envelope: okEnvelope(leaseOperation, ServiceLeaseData{LeaseID: req.LeaseID})}, nil
}

// Review 13: a lease is created only under a caller-chosen key, so a repeat
// after a lost reply names the same lease and the tool can be idempotent.
func TestLeaseNeedsACallerKey(t *testing.T) {
	controls := &leaseRecorder{}
	if _, err := callTool(t, controls, "mesh_service_lease", `{"host":"pc","service":"repo","ttl_seconds":60}`); !isParamError(err) {
		t.Fatalf("a lease without a key: %v", err)
	}
	if _, err := callTool(t, controls, "mesh_service_lease", `{"host":"pc","service":"repo","lease_id":"bad key","ttl_seconds":60}`); !isParamError(err) {
		t.Fatalf("a malformed key: %v", err)
	}
	for range 2 {
		if _, err := callTool(t, controls, "mesh_service_lease", `{"host":"pc","service":"repo","lease_id":"build-1","ttl_seconds":60}`); err != nil {
			t.Fatal(err)
		}
	}
	if len(controls.leases) != 2 || controls.leases[0].LeaseID != "build-1" || controls.leases[1].LeaseID != "build-1" {
		t.Fatalf("leases %+v", controls.leases)
	}
	spec, _ := DefaultRegistry().Lookup("mesh_service_lease")
	if spec.Annotations.IdempotentHint == nil || !*spec.Annotations.IdempotentHint {
		t.Fatal("a keyed lease should be advertised as idempotent")
	}
}
