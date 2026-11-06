package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/hostprofile"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/update"
)

type deliveryFixture struct {
	controller *fleetProfileController
	scope      hostprofile.FleetScope
	doc        hostprofile.FleetDocument
	hosts      []update.Host
	state      string
}

func newDeliveryFixture(t *testing.T, count int) deliveryFixture {
	t.Helper()
	state := t.TempDir()
	host, key, err := identity.LoadOrCreate(state)
	if err != nil {
		t.Fatal(err)
	}
	hosts := make([]update.Host, 0, count)
	ids := make([]string, 0, count)
	for i := range count {
		member, _, err := identity.LoadOrCreate(filepath.Join(t.TempDir(), fmt.Sprint(i)))
		if err != nil {
			t.Fatal(err)
		}
		hosts = append(hosts, update.Host{ID: member.ID, Endpoint: "unix:/not-running"})
		ids = append(ids, member.ID)
	}
	membership := filepath.Join(t.TempDir(), "fleet.json")
	writeDeliveryJSON(t, membership, update.Fleet{Version: 1, Name: "fixture", Revision: 1, Members: hosts})
	controller := newFleetProfileController(state, membership, host.ID, update.Client{ID: host.ID, Key: key}, func(string) bool { return true }, nil)
	scope := hostprofile.FleetScope{Members: ids, Trusted: controller.trusted}
	doc, err := hostprofile.SignFleet(hostprofile.Editable{SchemaVersion: 1, Instructions: "notes"}, "fixture", 1, time.Now().UTC(), host.ID, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.store.WriteLocal(t.Context(), 0, doc, scope); err != nil {
		t.Fatal(err)
	}
	controller.jitter = func(d time.Duration) time.Duration { return d }
	return deliveryFixture{controller: controller, scope: scope, doc: doc, hosts: hosts, state: state}
}
func writeDeliveryJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
func deliveryResult(t *testing.T, f deliveryFixture) hostprofile.Outbox {
	t.Helper()
	_, out, err := f.controller.store.Read(f.controller.trusted)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func TestFleetDeliveryExplicitOutcomesAndErrorClassification(t *testing.T) {
	cases := []struct {
		name   string
		result hostprofile.PutResult
		err    error
		state  hostprofile.DeliveryState
		code   string
	}{
		{name: "applied", state: hostprofile.Delivered},
		{name: "unchanged", state: hostprofile.Delivered},
		{name: "conflict", state: hostprofile.Diverged, code: "fleet.revision_conflict"},
		{name: "untrusted", state: hostprofile.Pending, code: "fleet.signer_untrusted"},
		{name: "unknown", result: hostprofile.PutResult{Outcome: "unexpected"}, state: hostprofile.Pending, code: "fleet.delivery_rejected"},
		{name: "authentication", err: transport.ErrAuthentication, state: hostprofile.Pending, code: "host.authentication_failed"},
		{name: "remote", err: &update.RemoteError{Problem: "not trusted, arbitrary remote text"}, state: hostprofile.Pending, code: "fleet.delivery_rejected"},
		{name: "offline", err: errors.New("offline"), state: hostprofile.Pending, code: "host.unreachable"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			f := newDeliveryFixture(t, 1)
			result := test.result
			if test.name == "applied" || test.name == "unchanged" || test.name == "conflict" {
				result = hostprofile.PutResult{Outcome: hostprofile.Acceptance(test.name), Revision: 1, Digest: f.doc.Digest()}
			}
			if test.name == "conflict" {
				result.Digest = fmt.Sprintf("%064d", 0)
			}
			if test.name == "untrusted" {
				result.Outcome = hostprofile.Untrusted
			}
			f.controller.call = func(_ context.Context, _ update.Host, action string, input, output any) error {
				if action != hostprofile.ActionFleetPut || input.(hostprofile.PutRequest).Document.Digest() != f.doc.Digest() {
					t.Error("wrong administrator action")
				}
				*output.(*hostprofile.PutResult) = result
				return test.err
			}
			if err := f.controller.cycle(t.Context()); err != nil {
				t.Fatal(err)
			}
			member := deliveryResult(t, f).Members[0]
			if member.State != test.state {
				t.Fatal(member)
			}
			if test.code != "" && (member.LastError == nil || string(member.LastError.Code) != test.code) {
				t.Fatal(member)
			}
		})
	}
}
func TestFleetDeliveryBackoffRestartAndMembership(t *testing.T) {
	f := newDeliveryFixture(t, 2)
	clock := time.Now()
	f.controller.now = func() time.Time { return clock }
	var counter atomic.Int32
	f.controller.call = func(context.Context, update.Host, string, any, any) error {
		counter.Add(1)
		return errors.New("offline")
	}
	if err := f.controller.cycle(t.Context()); err != nil {
		t.Fatal(err)
	}
	if counter.Load() != 2 {
		t.Fatal(counter.Load())
	}
	first := deliveryResult(t, f)
	clock = clock.Add(29 * time.Second)
	if err := f.controller.cycle(t.Context()); err != nil {
		t.Fatal(err)
	}
	if counter.Load() != 2 {
		t.Fatal("retried before deadline")
	}
	clock = clock.Add(time.Second)
	if err := f.controller.cycle(t.Context()); err != nil {
		t.Fatal(err)
	}
	if counter.Load() != 4 {
		t.Fatal("retry did not occur")
	}
	retry := f.controller.retries[f.hosts[0].ID]
	if retry.delay != time.Minute {
		t.Fatal(retry)
	}
	for range 20 {
		retry.delay = nextFleetDelay(retry.delay)
	}
	if retry.delay != 30*time.Minute {
		t.Fatal(retry)
	}
	restart := newFleetProfileController(f.state, f.controller.membershipPath, f.doc.Author, update.Client{}, f.controller.trusted, nil)
	restart.call = f.controller.call
	restart.jitter = f.controller.jitter
	if err := restart.cycle(t.Context()); err != nil {
		t.Fatal(err)
	}
	if counter.Load() != 6 {
		t.Fatal("restart failed to retry immediately")
	}
	after := deliveryResult(t, f)
	if !first.Members[0].QueuedAt.Equal(after.Members[0].QueuedAt) {
		t.Fatal("restart reset durable age")
	}
	writeDeliveryJSON(t, f.controller.membershipPath, update.Fleet{Version: 1, Name: "fixture", Revision: 2, Members: f.hosts[:1]})
	if err := f.controller.cycle(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(deliveryResult(t, f).Members) != 1 {
		t.Fatal("removed member retained")
	}
}
func TestFleetDeliveryCancellationJoinsAllAttempts(t *testing.T) {
	f := newDeliveryFixture(t, 12)
	entered := make(chan struct{}, 12)
	var active, peak atomic.Int32
	f.controller.call = func(ctx context.Context, _ update.Host, _ string, _, _ any) error {
		n := active.Add(1)
		defer active.Add(-1)
		for {
			previous := peak.Load()
			if previous >= n || peak.CompareAndSwap(previous, n) {
				break
			}
		}
		entered <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); f.controller.Run(ctx) }()
	for range 8 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			cancel()
			<-done
			t.Fatal("delivery workers did not start")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("delivery shutdown did not join")
	}
	if active.Load() != 0 || peak.Load() != 8 {
		t.Fatal(active.Load(), peak.Load())
	}
	for _, member := range deliveryResult(t, f).Members {
		if member.State != hostprofile.Pending || member.LastError != nil {
			t.Fatal("cancellation mutated delivery", member)
		}
	}
}
func TestFleetSyncIsPayloadFreeAndLocal(t *testing.T) {
	f := newDeliveryFixture(t, 1)
	response, handled, err := f.controller.HandleControl(t.Context(), protocol.Control{Type: protocol.TypeFleetProfileSync, RequestID: "sync"})
	if err != nil || !handled || response.Type != protocol.TypeOK {
		t.Fatal(response, handled, err)
	}
	_, _, err = f.controller.HandleControl(t.Context(), protocol.Control{Type: protocol.TypeFleetProfileSync, Context: json.RawMessage(`{"document":{}}`)})
	if err == nil {
		t.Fatal("sync accepted document")
	}
}
func TestFleetSyncRunPokeAndTimer(t *testing.T) {
	f := newDeliveryFixture(t, 1)
	ticks := make(chan time.Time)
	waiting := make(chan struct{}, 4)
	var calls atomic.Int32
	f.controller.after = func(time.Duration) <-chan time.Time { waiting <- struct{}{}; return ticks }
	f.controller.call = func(context.Context, update.Host, string, any, any) error { calls.Add(1); return errors.New("offline") }
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); f.controller.Run(ctx) }()
	defer func() { cancel(); <-done }()
	waitFleetSignal(t, waiting)
	if calls.Load() != 1 {
		t.Fatal(calls.Load())
	}
	f.controller.signal()
	waitFleetSignal(t, waiting)
	if calls.Load() != 2 {
		t.Fatal("poke did not reset in-memory retry")
	}
	ticks <- time.Now()
	waitFleetSignal(t, waiting)
	if calls.Load() != 2 {
		t.Fatal("timer bypassed backoff")
	}
}
func waitFleetSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("delivery loop did not reach wait")
	}
}

func TestFleetSignedCarrierTrustAndAuthenticatedSender(t *testing.T) {
	f := newDeliveryFixture(t, 1)
	t.Setenv("MESH_UPDATE_CACHE_DIR", t.TempDir())
	destination, key, err := identity.LoadPrivate(f.state)
	if err != nil {
		t.Fatal(err)
	}
	relay, relayKey, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	author, authorKey, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := update.Trust(f.state, relay.ID, false); err != nil {
		t.Fatal(err)
	}
	writeDeliveryJSON(t, f.controller.membershipPath, update.Fleet{Version: 1, Name: "fixture", Revision: 2, Members: []update.Host{{ID: relay.ID, Endpoint: "unix:/not-running"}}})
	updates, err := newUpdateController(f.state, destination.ID, key)
	if err != nil {
		t.Fatal(err)
	}
	f.controller.trusted = updates.authority.IsAdministrator
	updates.fleetProfile = f.controller
	auth, err := controlAuthentication(f.state)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = transport.ServeWithOptions(w, r, transport.ServeOptions{Auth: auth}, func(ctx context.Context, conn transport.Conn) error {
			return fleetCarrierFixture(ctx, conn, updates, f.controller)
		})
	}))
	defer server.Close()
	target := update.Host{ID: destination.ID, Endpoint: "ws" + strings.TrimPrefix(server.URL, "http") + "/mesh"}
	client := update.Client{ID: relay.ID, Key: relayKey}
	doc, err := hostprofile.SignFleet(hostprofile.Editable{SchemaVersion: 1, Instructions: "relayed"}, "fixture", 2, time.Now().UTC(), author.ID, authorKey)
	if err != nil {
		t.Fatal(err)
	}
	var result hostprofile.PutResult
	if err := client.Call(t.Context(), target, hostprofile.ActionFleetPut, hostprofile.PutRequest{Document: doc}, &result); err != nil {
		t.Fatal(err)
	}
	if result.Outcome != hostprofile.Untrusted {
		t.Fatal(result)
	}
	if err := update.Trust(f.state, author.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := client.Call(t.Context(), target, hostprofile.ActionFleetPut, hostprofile.PutRequest{Document: doc}, &result); err != nil {
		t.Fatal(err)
	}
	if result.Outcome != hostprofile.Applied {
		t.Fatal(result)
	}
	out := deliveryResult(t, f)
	if len(out.Members) != 1 || out.Members[0].HostID != relay.ID || out.Members[0].State != hostprofile.Delivered {
		t.Fatal("sender was not the authenticated relay", out)
	}
	doc.Signature = "bad"
	if err := client.Call(t.Context(), target, hostprofile.ActionFleetPut, hostprofile.PutRequest{Document: doc}, &result); err == nil {
		t.Fatal("carrier admitted invalid document signature")
	}
	// Full-device authority still cannot use the local-only signal over a network connection.
	if err := identity.ApproveDevice(f.state, relay.ID); err != nil {
		t.Fatal(err)
	}
	conn, err := transport.DialOnce(t.Context(), target.Endpoint, transport.DialOptions{Auth: &transport.Authentication{Key: relayKey, ExpectedIdentity: destination.ID}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	payload, err := (protocol.Control{Type: protocol.TypeFleetProfileSync, RequestID: "remote-sync"}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	response, err := protocol.ReadControl(conn.ReadFrame, conn.WriteFrame)
	if err != nil || response.Type != protocol.TypeError {
		t.Fatal(response, err)
	}
}
func fleetCarrierFixture(ctx context.Context, conn transport.Conn, updates *updateController, fleet *fleetProfileController) error {
	for {
		request, err := protocol.ReadControl(conn.ReadFrame, conn.WriteFrame)
		if err != nil {
			return fmt.Errorf("read fleet fixture control: %w", err)
		}
		response, handled, err := fleet.HandleControl(ctx, request)
		if !handled {
			response, _, err = updates.HandleControl(ctx, request)
		}
		if err != nil {
			response = protocol.Control{Type: protocol.TypeError, RequestID: request.RequestID, Message: err.Error()}
		}
		data, err := response.Encode()
		if err != nil {
			return fmt.Errorf("encode fleet fixture control: %w", err)
		}
		if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: data}); err != nil {
			return fmt.Errorf("write fleet fixture control: %w", err)
		}
	}
}

func TestFleetLateNetworkCurrentCannotReplaceNewOwnerWrite(t *testing.T) {
	f := newDeliveryFixture(t, 1)
	host, key, err := identity.LoadPrivate(f.state)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	remote, err := hostprofile.SignFleet(hostprofile.Editable{SchemaVersion: 1, Instructions: "late remote"}, "fixture", 3, time.Now().UTC(), host.ID, key)
	if err != nil {
		t.Fatal(err)
	}
	f.controller.call = func(ctx context.Context, _ update.Host, _ string, _, output any) error {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		*output.(*hostprofile.PutResult) = hostprofile.PutResult{Outcome: hostprofile.Older, Revision: 3, Digest: remote.Digest(), Current: &remote}
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- f.controller.cycle(t.Context()) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		<-done
		t.Fatal("network attempt did not start")
	}
	next, err := hostprofile.SignFleet(hostprofile.Editable{SchemaVersion: 1, Instructions: "new owner"}, "fixture", 2, time.Now().UTC(), host.ID, key)
	if err != nil {
		close(release)
		<-done
		t.Fatal(err)
	}
	result, writeErr := f.controller.store.WriteLocal(t.Context(), 1, next, f.scope)
	close(release)
	deliveryErr := <-done
	if writeErr != nil || deliveryErr != nil || !result.Complete() {
		t.Fatal(result, writeErr, deliveryErr)
	}
	snapshot, out, err := f.controller.store.Read(f.controller.trusted)
	if err != nil || snapshot.Value.Digest() != next.Digest() || out.Members[0].State != hostprofile.Pending {
		t.Fatal(snapshot, out, err)
	}
}
