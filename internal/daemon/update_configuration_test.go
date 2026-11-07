package daemon

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/update"
)

func TestConfigurationObservationRequiresActualLocalSignedIdentity(t *testing.T) {
	state := t.TempDir()
	host, key, err := identity.LoadOrCreate(state)
	if err != nil {
		t.Fatal(err)
	}
	controller, err := newUpdateController(state, host.ID, key)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = transport.ServeWithOptions(w, r, transport.ServeOptions{Auth: &transport.Authentication{Key: key, Authorize: func(id string) bool { return id == host.ID }}}, func(ctx context.Context, conn transport.Conn) error {
			return configurationControlFrames(ctx, conn, controller)
		})
	}))
	t.Cleanup(server.Close)
	client := update.Client{ID: host.ID, Key: key}
	target := update.Host{ID: host.ID, Endpoint: "ws" + strings.TrimPrefix(server.URL, "http")}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	err = client.Call(ctx, target, "configuration.observe", update.ConfigurationObservation{}, nil)
	var remote *update.RemoteError
	if !errors.As(err, &remote) || remote.Code != "configuration.denied" {
		t.Fatal("remote self identity posted consumer success", err)
	}
	if len(controller.configurationObservers.snapshot(time.Now())) != 0 {
		t.Fatal("remote observer acquired a lease")
	}
	observation := configurationObservationFixture(t, host.ID)
	configurationLocalObservation(t, controller, host.ID, key, observation)
	snapshots := controller.configurationObservers.snapshot(time.Now())
	if len(snapshots) != 1 || snapshots[0].Generation != observation.Generation {
		t.Fatal(snapshots)
	}
}
func configurationControlFrames(ctx context.Context, conn transport.Conn, controller *updateController) error {
	for range 2 {
		frame, err := conn.ReadFrame()
		if err != nil {
			return fmt.Errorf("read configuration fixture: %w", err)
		}
		control, err := protocol.DecodeControl(frame.Payload)
		if err != nil {
			return fmt.Errorf("decode configuration fixture: %w", err)
		}
		response, _, err := controller.HandleControl(ctx, control)
		if err != nil {
			return err
		}
		payload, err := response.Encode()
		if err != nil {
			return fmt.Errorf("encode configuration fixture: %w", err)
		}
		if err = conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload}); err != nil {
			return fmt.Errorf("write configuration fixture: %w", err)
		}
	}
	return nil
}
func configurationObservationFixture(t *testing.T, id string) update.ConfigurationObservation {
	t.Helper()
	instance, err := update.NewAutomaticCommandID()
	if err != nil {
		t.Fatal(err)
	}
	observed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return update.ConfigurationObservation{InstanceID: instance, HostID: id, Generation: 1, State: "disabled", Digest: (update.FeedValue{Present: true}).Digest(), HTTPAt: observed, PublishedAt: observed, UpstreamAt: &observed}
}
func configurationLocalObservation(t *testing.T, controller *updateController, id string, key ed25519.PrivateKey, observation update.ConfigurationObservation) {
	t.Helper()
	result := configurationLocalObservationResult(t, controller, id, key, observation)
	if result.Problem != "" {
		t.Fatalf("local observer refused: %s", result.Problem)
	}
}
func configurationLocalObservationResult(t *testing.T, controller *updateController, id string, key ed25519.PrivateKey, observation update.ConfigurationObservation) update.Message {
	t.Helper()
	request := update.Message{Actor: id, Target: id, Action: "challenge", Nonce: observation.InstanceID}
	request.Sign(key)
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	response, _, err := controller.HandleControl(t.Context(), protocol.Control{Type: update.ControlType, Update: data})
	if err != nil {
		t.Fatal(err)
	}
	var challenge update.Message
	if err = json.Unmarshal(response.Update, &challenge); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(challenge.Data, &request.Nonce); err != nil {
		t.Fatal(err)
	}
	request.Action = "configuration.observe"
	request.Data, err = json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	request.Sign(key)
	data, err = json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	response, _, err = controller.HandleControl(t.Context(), protocol.Control{Type: update.ControlType, Update: data})
	if err != nil {
		t.Fatal(err)
	}
	var result update.Message
	if err = json.Unmarshal(response.Update, &result); err != nil {
		t.Fatal(err)
	}
	return result
}
func TestConfigurationObserverLeaseAndDaemonRestartRequireActualRenewal(t *testing.T) {
	state := t.TempDir()
	host, key, err := identity.LoadOrCreate(state)
	if err != nil {
		t.Fatal(err)
	}
	first, err := newUpdateController(state, host.ID, key)
	if err != nil {
		t.Fatal(err)
	}
	observation := configurationObservationFixture(t, host.ID)
	configurationLocalObservation(t, first, host.ID, key, observation)
	initial := first.configurationObservers.snapshot(time.Now())[0]
	configurationLocalObservation(t, first, host.ID, key, observation)
	renewed := first.configurationObservers.snapshot(time.Now())[0]
	if renewed.HTTPAt != initial.HTTPAt || renewed.PublishedAt != initial.PublishedAt || !renewed.UpstreamAt.Equal(*initial.UpstreamAt) {
		t.Fatal("renewal manufactured fresh HTTP or quota evidence")
	}
	if len(first.configurationObservers.snapshot(renewed.RenewedAt.Add(update.ConfigurationObserverLease))) != 0 {
		t.Fatal("expired observer stayed live")
	}
	second, err := newUpdateController(state, host.ID, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.configurationObservers.snapshot(time.Now())) != 0 {
		t.Fatal("daemon restart retained a fabricated observer lease")
	}
	configurationLocalObservation(t, second, host.ID, key, observation)
	if len(second.configurationObservers.snapshot(time.Now())) != 1 {
		t.Fatal("actual renewal did not restore lease")
	}
}
