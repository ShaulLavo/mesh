package edge

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/apps"
	"github.com/shaul/mesh/internal/protocol"
)

func TestAppRegistryExchangePinsEdgeBeforeSendingSignedOperation(t *testing.T) {
	now := time.Now().UTC()
	state := &publisherMemoryOutbox{}
	exchanges := 0
	publisher := testPublisher(t, now, state, func(request protocol.Control) (protocol.Control, error) {
		if request.Type == protocol.TypeHostInfo {
			return publisherHostResponse(request, "unrelated-edge"), nil
		}
		exchanges++
		return protocol.Control{}, nil
	})
	signed, err := apps.Sign("mesh-app/request/v1", state.targetID, 1, apps.Request{Action: "list"}, publisher.signer, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.AppRegistryExchange(context.Background(), signed); err == nil || exchanges != 0 {
		t.Fatalf("wrong edge received app operation: err=%v exchanges=%d", err, exchanges)
	}
}

func TestAppRegistryExchangeRejectsAnotherOwnerBeforeTransport(t *testing.T) {
	now := time.Now().UTC()
	state := &publisherMemoryOutbox{}
	calls := 0
	publisher := testPublisher(t, now, state, func(request protocol.Control) (protocol.Control, error) { calls++; return protocol.Control{}, nil })
	_, otherKey := testIdentity(t)
	signed, err := apps.Sign("mesh-app/request/v1", state.targetID, 1, apps.Request{Action: "list"}, otherKey, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.AppRegistryExchange(context.Background(), signed); err == nil || calls != 0 {
		t.Fatalf("foreign operation reached transport: err=%v calls=%d", err, calls)
	}
}

func TestAppRegistryExchangeVerifiesSignedResponseIdentityAndSequence(t *testing.T) {
	now := time.Now().UTC()
	edgeID, edgeKey := testIdentity(t)
	state := &publisherMemoryOutbox{}
	publisher := testPublisher(t, now, state, func(request protocol.Control) (protocol.Control, error) {
		if request.Type == protocol.TypeHostInfo {
			return publisherHostResponse(request, edgeID), nil
		}
		response, err := apps.Sign("mesh-app/response/v1", state.originID, 2, map[string]string{"requestId": "different"}, edgeKey, now)
		if err != nil {
			return protocol.Control{}, err
		}
		data, err := json.Marshal(response)
		return protocol.Control{Type: protocol.TypeAppRegistry, RequestID: request.RequestID, App: data}, err
	})
	publisher.target.Identity = edgeID
	signed, err := apps.Sign("mesh-app/request/v1", edgeID, 1, apps.Request{Action: "list"}, publisher.signer, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.AppRegistryExchange(context.Background(), signed); err == nil {
		t.Fatal("mismatched response sequence accepted")
	}
}
