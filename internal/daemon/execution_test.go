package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/session/execution"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/worker"
)

func TestAcceptedReceiptSurvivesErrorConversion(t *testing.T) {
	for _, acceptance := range []string{"durable", "visible_unconfirmed"} {
		receipt := &protocol.ExecutionReceipt{HostID: "host", SessionID: "7K3D", Instance: "0123456789abcdef0123456789abcdef", Acceptance: acceptance}
		err := &executionError{receipt: receipt, err: errors.New("publication barrier failed")}
		reply := clientRequestError(protocol.Control{RequestID: "r"}, err)
		encoded, encodeErr := reply.Encode()
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		decoded, decodeErr := protocol.DecodeControl(encoded)
		if decodeErr != nil {
			t.Fatal(decodeErr)
		}
		if decoded.Type != protocol.TypeError || decoded.ExecutionReceipt == nil || decoded.ExecutionReceipt.Acceptance != acceptance || decoded.ErrorCode != "state.unavailable" {
			t.Fatalf("accepted receipt lost: %+v", decoded)
		}
	}
}
func TestExecutionCreateKeepsAcceptedBindingWhenCatalogFails(t *testing.T) {
	catalog := &lifecycleTestCatalog{reconcileErr: errors.New("catalog unavailable")}
	l, err := newLifecycle(lifecycleConfig{Catalog: catalog, Connector: lifecycleConnectorFunc(func(context.Context, protocol.SessionID) (transport.Conn, error) { return nil, errors.New("unused") }), Host: storage.Host{ID: "host", MeshIdentity: "host"}, SessionsDir: filepath.Join(t.TempDir(), "s"), Env: []string{"PATH=/bin"}, Launch: func(cfg worker.LaunchConfig) (worker.Launched, error) {
		return worker.Launched{Meta: worker.Meta{Reservation: cfg.Reservation, ID: cfg.Reservation.ID}}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.closeExecution)
	ctx := context.WithValue(t.Context(), localClientKey{}, true)
	request := protocol.Control{Type: protocol.TypeExecutionCreate, RequestID: "r", RequestKey: "job", ExecutionKind: "pty", Command: []string{"/bin/true"}, Cwd: "/work"}
	response, handled, err := l.HandleControl(ctx, request)
	if !handled || err == nil || response.ExecutionReceipt == nil {
		t.Fatalf("acceptance not reported: %+v %v", response, err)
	}
	if response.ExecutionReceipt.Acceptance != "durable" || response.ExecutionReceipt.LaunchOutcome != "started" {
		t.Fatal(response.ExecutionReceipt)
	}
	converted := clientRequestError(request, err)
	if converted.ExecutionReceipt.Instance != response.ExecutionReceipt.Instance {
		t.Fatal("binding discarded")
	}
}
func TestExecutionProtectionPrecedesForgottenAndLabelHistory(t *testing.T) {
	root := t.TempDir()
	meta := catalogTestMeta("7K3D", worker.StateExited, "boot-a")
	meta.Label = "agent request instance"
	meta.CreatedAt = time.Now().Add(-40 * 24 * time.Hour)
	ended := meta.CreatedAt.Add(time.Second)
	code := 0
	meta.ExitedAt = &ended
	meta.ExitCode = &code
	ref, err := session.NewReservation(meta.ID, "boot-a", session.Execution)
	if err != nil {
		t.Fatal(err)
	}
	meta.Reservation = &ref
	writeCatalogMeta(t, root, meta.ID, meta)
	if err := os.WriteFile(filepath.Join(root, meta.ID, ".reservation"), []byte("damaged exact reference"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, meta.ID, ".forgotten"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	store := &catalogStoreStub{}
	catalog := newCatalogForTest(t, root, store, probeFunc(func(context.Context, string) error { return nil }), func() string { return "boot-a" })
	if err := catalog.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(store.retired) != 0 {
		t.Fatal("forgotten marker bypassed execution protection")
	}
	if _, eligible := catalog.eligibleLabelledExit(storage.SessionID(meta.ID), meta.Label); eligible {
		t.Fatal("agent entered service history pruning")
	}
	if err := removeRetiredSession(filepath.Join(root, meta.ID)); !errors.Is(err, execution.ErrProtected) {
		t.Fatalf("cleanup ignored protected origin: %v", err)
	}
	if err := os.Remove(filepath.Join(root, meta.ID, ".reservation")); err != nil {
		t.Fatal(err)
	}
	if err := removeRetiredSession(filepath.Join(root, meta.ID)); !errors.Is(err, execution.ErrProtected) {
		t.Fatalf("missing static reference bypassed known execution metadata: %v", err)
	}
}
