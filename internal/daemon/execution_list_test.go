package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/access"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	meshserve "github.com/shaul/mesh/internal/serve"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/session/execution"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/worker"
)

func TestExecutionListFramedOwnershipGrantsAndFieldRejection(t *testing.T) {
	// 1. Protocol decoder rejects caller, executionContext, and administration on execution.list.
	for _, field := range []string{
		`"caller":"secret-cred"`,
		`"executionContext":{"contextId":"0123456789abcdef0123456789abcdef"}`,
		`"administration":true`,
	} {
		raw := []byte(fmt.Sprintf(`{"type":"execution.list","requestId":"r1",%s}`, field))
		if _, err := protocol.DecodeControl(raw); err == nil {
			t.Fatalf("DecodeControl accepted unexpected field %s on execution.list", field)
		}
	}

	// 2. Framed daemon test: owner full device, non-owner full device, and UpdateCarrier grant.
	stateDir := t.TempDir()
	hostIdent, hostKey, err := identity.LoadOrCreate(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	hostRec := storage.Host{ID: storage.HostID(hostIdent.ID), MeshIdentity: hostIdent.ID}
	sessionsDir := filepath.Join(stateDir, "s")
	ownerPub, ownerKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherPub, otherKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	updaterPub, updaterKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ownerID := base64.RawURLEncoding.EncodeToString(ownerPub)
	otherID := base64.RawURLEncoding.EncodeToString(otherPub)
	updaterID := base64.RawURLEncoding.EncodeToString(updaterPub)

	var workers contextWorkerCounter
	lc := mustLifecycle(t, lifecycleConfig{
		Catalog:     &scopedCatalog{},
		Connector:   &workers,
		Host:        hostRec,
		SessionsDir: sessionsDir,
	})
	journal, err := lc.executionJournal()
	if err != nil {
		t.Fatal(err)
	}
	seedReceipt := func(class, principal, id, key string, instNum int) {
		k := execution.Key{Host: hostIdent.ID, Class: class, Principal: principal, Opaque: key}
		intent := execution.Intent{Kind: execution.KindPipe, Command: []string{"/bin/true"}, Cwd: "/tmp", Stdin: execution.StdinClosed}
		ref := session.Reservation{ID: id, Instance: fmt.Sprintf("%032x", instNum), BootID: worker.BootID(), Purpose: execution.PurposeForKind(execution.KindPipe)}
		if _, _, err := journal.Reserve(t.Context(), k, intent, func() (execution.Binding, error) {
			return execution.NewBinding(k, intent, ref, nil), nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	seedReceipt("device", ownerID, "7K3A", "own-1", 1)
	seedReceipt("device", ownerID, "7K3B", "own-2", 2)
	seedReceipt("device", otherID, "8B3C", "oth-1", 3)
	seedReceipt("local-account", "local-account", "9C3D", "loc-1", 4)

	bind := func(id string) transport.Authorization {
		switch id {
		case ownerID, otherID:
			return transport.Authorization{Class: access.FullDevice, Current: func() bool { return true }}
		case updaterID:
			return transport.Authorization{
				Class:   access.UpdateCarrier,
				Current: func() bool { return true },
				Admit: func(frame protocol.Frame) bool {
					if frame.Kind != protocol.KindControl {
						return false
					}
					req, decErr := protocol.DecodeControl(frame.Payload)
					return decErr == nil && req.Type == update.ControlType
				},
			}
		default:
			return transport.Authorization{}
		}
	}
	auth := &transport.Authentication{
		Key: hostKey,
		Authorize: func(id string) bool {
			grant := bind(id)
			return grant.Current != nil && grant.Current()
		},
		Bind: bind,
	}
	registry, err := meshserve.NewRegistry(nil)
	if err != nil {
		t.Fatal(err)
	}
	serviceController, err := newServiceController(t.Context(), t.TempDir(), &scopedServiceStore{}, registry)
	if err != nil {
		t.Fatal(err)
	}
	var owners contextControlCounter
	srv, err := newClientServer(lc, &workers, serviceController, &owners)
	if err != nil {
		t.Fatal(err)
	}
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = transport.ServeWithOptions(w, r, transport.ServeOptions{Auth: auth}, func(ctx context.Context, conn transport.Conn) error {
			return srv.Handle(ctx, conn)
		})
	}))
	defer httpSrv.Close()
	wsURL := httpSrv.URL

	exchange := func(key ed25519.PrivateKey, admission transport.Admission, ctrl protocol.Control) (protocol.Control, error) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		conn, err := transport.DialOnce(ctx, wsURL, transport.DialOptions{
			Auth: &transport.Authentication{
				Key:              key,
				ExpectedIdentity: hostIdent.ID,
				Admission:        admission,
			},
		})
		if err != nil {
			return protocol.Control{}, fmt.Errorf("dial test server: %w", err)
		}
		defer func() { _ = conn.Close() }()
		payload, err := ctrl.Encode()
		if err != nil {
			return protocol.Control{}, fmt.Errorf("encode control: %w", err)
		}
		if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload}); err != nil {
			return protocol.Control{}, fmt.Errorf("write control frame: %w", err)
		}
		frame, err := conn.ReadFrame()
		if err != nil {
			return protocol.Control{}, fmt.Errorf("read control frame: %w", err)
		}
		decoded, err := protocol.DecodeControl(frame.Payload)
		if err != nil {
			return protocol.Control{}, fmt.Errorf("decode control: %w", err)
		}
		return decoded, nil
	}

	// Owner full device sees its 2 receipts across 2 pages (limit=1) with non-zero CreatedAt.
	p1, err := exchange(ownerKey, transport.FullAdmission, protocol.Control{
		Type: protocol.TypeExecutionList, RequestID: "own-p1", ExecutionListAll: true, ExecutionListLimit: 1,
	})
	if err != nil || p1.Type != protocol.TypeExecutionListed || len(p1.ExecutionReceipts) != 1 || p1.ExecutionNextPage == nil {
		t.Fatalf("owner page 1: resp=%+v err=%v", p1, err)
	}
	if p1.ExecutionReceipts[0].CreatedAt == nil || p1.ExecutionReceipts[0].CreatedAt.IsZero() {
		t.Fatalf("expected non-zero CreatedAt on listed receipt: %+v", p1.ExecutionReceipts[0])
	}
	p2, err := exchange(ownerKey, transport.FullAdmission, protocol.Control{
		Type: protocol.TypeExecutionList, RequestID: "own-p2", ExecutionListAll: true, ExecutionListLimit: 1, Cursor: *p1.ExecutionNextPage,
	})
	if err != nil || p2.Type != protocol.TypeExecutionListed || len(p2.ExecutionReceipts) != 1 || p2.ExecutionNextPage != nil {
		t.Fatalf("owner page 2: resp=%+v err=%v", p2, err)
	}
	ids := []string{p1.ExecutionReceipts[0].SessionID, p2.ExecutionReceipts[0].SessionID}
	if !((ids[0] == "7K3A" && ids[1] == "7K3B") || (ids[0] == "7K3B" && ids[1] == "7K3A")) {
		t.Fatalf("owner listed unexpected ids: %v", ids)
	}

	// Non-owner full device sees ONLY its own receipt (8B3C) and cannot reuse owner's cursor.
	otherResp, err := exchange(otherKey, transport.FullAdmission, protocol.Control{
		Type: protocol.TypeExecutionList, RequestID: "oth-p1", ExecutionListAll: true,
	})
	if err != nil || otherResp.Type != protocol.TypeExecutionListed || len(otherResp.ExecutionReceipts) != 1 || otherResp.ExecutionReceipts[0].SessionID != "8B3C" {
		t.Fatalf("other full device saw wrong receipts: resp=%+v err=%v", otherResp, err)
	}
	stolenCursor, err := exchange(otherKey, transport.FullAdmission, protocol.Control{
		Type: protocol.TypeExecutionList, RequestID: "oth-stolen", ExecutionListAll: true, Cursor: *p1.ExecutionNextPage,
	})
	if err != nil || stolenCursor.Type != protocol.TypeError || stolenCursor.ErrorCode != "usage.invalid" {
		t.Fatalf("expected usage.invalid for cross-owner cursor, got resp=%+v err=%v", stolenCursor, err)
	}

	// UpdateCarrier grant is denied execution.list at the framed boundary.
	updResp, updErr := exchange(updaterKey, transport.SignedUpdateAdmission, protocol.Control{
		Type: protocol.TypeExecutionList, RequestID: "upd-deny", ExecutionListAll: true,
	})
	if updErr == nil && updResp.Type == protocol.TypeExecutionListed {
		raw, _ := json.Marshal(updResp)
		t.Fatalf("UpdateCarrier grant should not receive execution.listed: %s", raw)
	}
}
