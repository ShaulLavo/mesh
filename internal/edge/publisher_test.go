package edge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/serve"
	"github.com/shaul/mesh/internal/transport"
)

func TestPublisherRetriesExactPendingAndSupersedesChangedDesiredState(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name            string
		second          []serve.Service
		wantSequences   []uint64
		wantFinalRoutes []Route
	}{
		{
			name:            "same desired retries exact sequence",
			second:          []serve.Service{{Name: "app", PublicName: "app.shaulavo.dev"}},
			wantSequences:   []uint64{1, 1, 1, 2},
			wantFinalRoutes: []Route{{PublicName: "app.shaulavo.dev", ServiceName: "app"}},
		},
		{
			name:            "changed desired compensates at higher sequence",
			second:          []serve.Service{{Name: "new", PublicName: "new.shaulavo.dev"}},
			wantSequences:   []uint64{1, 1, 2},
			wantFinalRoutes: []Route{{PublicName: "new.shaulavo.dev", ServiceName: "new"}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := &publisherMemoryOutbox{}
			var mu sync.Mutex
			var sequences []uint64
			failures := 2
			publisher := testPublisher(t, now, state, func(request protocol.Control) (protocol.Control, error) {
				switch request.Type {
				case protocol.TypeHostInfo:
					return publisherHostResponse(request, state.targetID), nil
				case protocol.TypeEdgeRegister:
					mu.Lock()
					sequences = append(sequences, request.EdgeSnapshot.Sequence)
					if failures > 0 {
						failures--
						mu.Unlock()
						return protocol.Control{}, io.ErrUnexpectedEOF
					}
					mu.Unlock()
					snapshot := snapshotFromProtocol(*request.EdgeSnapshot)
					digest, err := VerifySnapshot(snapshot, state.targetID, snapshot.OriginID)
					return protocol.Control{Type: protocol.TypeEdgeRegistered, RequestID: request.RequestID, EdgeSequence: snapshot.Sequence, EdgeDigest: digest}, err
				default:
					return protocol.Control{}, fmt.Errorf("unexpected request %q", request.Type)
				}
			})
			first := []serve.Service{{Name: "app", PublicName: "app.shaulavo.dev"}}
			if err := publisher.Converge(context.Background(), first); err == nil {
				t.Fatal("ambiguous registration unexpectedly succeeded")
			}
			if err := publisher.Converge(context.Background(), test.second); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if fmt.Sprint(sequences) != fmt.Sprint(test.wantSequences) {
				t.Fatalf("sent sequences = %v, want %v", sequences, test.wantSequences)
			}
			if !state.record.Acknowledged || !routesEqual(state.record.Snapshot.Routes, test.wantFinalRoutes) {
				t.Fatalf("final outbox = %#v", state.record)
			}
		})
	}
}

func TestPublisherSupersedesExpiredPendingAndClassifiesSafeErrors(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	state := &publisherMemoryOutbox{}
	var responses = []protocol.Control{
		{Type: protocol.TypeError, ErrorCode: protocol.ErrorCodeEdgeRouteCollision, Message: "attacker\r\nsecret 100.64.0.8"},
		{Type: protocol.TypeEdgeRegistered},
	}
	var sequences []uint64
	publisher := testPublisher(t, now, state, func(request protocol.Control) (protocol.Control, error) {
		if request.Type == protocol.TypeHostInfo {
			return publisherHostResponse(request, state.targetID), nil
		}
		snapshot := snapshotFromProtocol(*request.EdgeSnapshot)
		sequences = append(sequences, snapshot.Sequence)
		response := responses[0]
		responses = responses[1:]
		response.RequestID = request.RequestID
		if response.Type == protocol.TypeEdgeRegistered {
			response.EdgeSequence = snapshot.Sequence
			response.EdgeDigest, _ = VerifySnapshot(snapshot, state.targetID, snapshot.OriginID)
		}
		return response, nil
	})
	services := []serve.Service{{Name: "app", PublicName: "app.shaulavo.dev"}}
	if err := publisher.Converge(context.Background(), services); !errors.Is(err, ErrRouteCollision) {
		t.Fatalf("collision error = %v", err)
	}
	if len(sequences) != 1 || state.record.Acknowledged {
		t.Fatalf("collision attempts = %v, outbox = %#v", sequences, state.record)
	}

	// An expired pending attempt is never retried forever. A fresh sequence
	// supersedes it even when desired routes are unchanged.
	state.record.Snapshot.ExpiresAt = now.Add(-time.Second)
	state.record.Snapshot.IssuedAt = now.Add(-6 * time.Minute)
	state.record.Snapshot, _ = SignSnapshot(NewSnapshot(
		state.targetID, state.originID, state.record.Snapshot.Sequence,
		now.Add(-6*time.Minute), now.Add(-time.Minute), state.record.Snapshot.Routes,
	), state.signer, now.Add(-6*time.Minute))
	state.record.Digest, _ = VerifySnapshot(state.record.Snapshot, state.targetID, state.originID)
	if err := publisher.Converge(context.Background(), services); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(sequences) != "[1 2]" || !state.record.Acknowledged {
		t.Fatalf("expired retry sequences = %v, outbox = %#v", sequences, state.record)
	}
}

func TestRoutesFromServicesBoundsOnlyPublicRoutes(t *testing.T) {
	services := make([]serve.Service, MaximumRoutes+20)
	for index := range services {
		services[index] = serve.Service{Name: fmt.Sprintf("private-%03d", index)}
	}
	services = append(services, serve.Service{Name: "app", PublicName: "app.shaulavo.dev"})
	routes, err := routesFromServices(services)
	if err != nil || len(routes) != 1 || routes[0].ServiceName != "app" {
		t.Fatalf("public routes = %#v, error = %v", routes, err)
	}
}

func TestPublisherPublishesDataPlaneTrustOnlyAfterHostIdentityPin(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	targetID, _ := testIdentity(t)
	originID, signer := testIdentity(t)
	state := &publisherMemoryOutbox{targetID: targetID, originID: originID, signer: signer}
	var pinned []netip.Addr
	respond := func(request protocol.Control) (protocol.Control, error) {
		switch request.Type {
		case protocol.TypeHostInfo:
			return publisherHostResponse(request, targetID), nil
		case protocol.TypeEdgeRegister:
			snapshot := snapshotFromProtocol(*request.EdgeSnapshot)
			digest, err := VerifySnapshot(snapshot, targetID, originID)
			return protocol.Control{
				Type: protocol.TypeEdgeRegistered, RequestID: request.RequestID,
				EdgeSequence: snapshot.Sequence, EdgeDigest: digest,
			}, err
		default:
			return protocol.Control{}, fmt.Errorf("unexpected request %q", request.Type)
		}
	}
	publisher, err := NewPublisher(PublisherConfig{
		Signer: signer,
		Target: TargetConfig{Identity: targetID, TailscaleName: "edge.example.ts.net", ControlPort: 7337, WebSocketPath: "/mesh"},
		State:  state,
		Resolve: func(context.Context, TargetConfig) (netip.AddrPort, error) {
			return netip.MustParseAddrPort("100.64.0.2:7337"), nil
		},
		Dial: func(context.Context, string) (transport.Conn, error) {
			return &publisherTestConn{respond: respond}, nil
		},
		Now: func() time.Time { return now }, RequestTimeout: time.Second,
		OnPinned: func(address netip.Addr) { pinned = append(pinned, address) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.Converge(context.Background(), []serve.Service{{Name: "app", PublicName: "app.shaulavo.dev"}}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(pinned) != "[100.64.0.2]" {
		t.Fatalf("published trust addresses = %v, want the identity-pinned endpoint", pinned)
	}

	pinned = nil
	state = &publisherMemoryOutbox{targetID: targetID, originID: originID, signer: signer}
	publisher, err = NewPublisher(PublisherConfig{
		Signer: signer,
		Target: TargetConfig{Identity: targetID, TailscaleName: "edge.example.ts.net", ControlPort: 7337, WebSocketPath: "/mesh"},
		State:  state,
		Resolve: func(context.Context, TargetConfig) (netip.AddrPort, error) {
			return netip.MustParseAddrPort("100.64.0.9:7337"), nil
		},
		Dial: func(context.Context, string) (transport.Conn, error) {
			return &publisherTestConn{respond: func(request protocol.Control) (protocol.Control, error) {
				return publisherHostResponse(request, originID), nil
			}}, nil
		},
		Now: func() time.Time { return now }, RequestTimeout: time.Second,
		OnPinned: func(address netip.Addr) { pinned = append(pinned, address) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.Converge(context.Background(), []serve.Service{{Name: "app", PublicName: "app.shaulavo.dev"}}); err == nil {
		t.Fatal("publisher accepted the wrong host identity")
	}
	if len(pinned) != 0 {
		t.Fatalf("unverified address was trusted: %v", pinned)
	}
}

func TestPublisherListPagePinsSignsAndValidatesCanonicalPage(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	state := &publisherMemoryOutbox{}
	cursor := encodeListCursor("app.shaulavo.dev", "/a")
	next := encodeListCursor("app.shaulavo.dev", "/c")
	publisher := testPublisher(t, now, state, func(request protocol.Control) (protocol.Control, error) {
		switch request.Type {
		case protocol.TypeHostInfo:
			return publisherHostResponse(request, state.targetID), nil
		case protocol.TypeEdgeList:
			if request.EdgeListProof == nil {
				t.Fatal("edge.list omitted its signed proof")
			}
			proof := listProofFromProtocol(*request.EdgeListProof)
			if _, err := verifyListProof(proof, request.RequestID, cursor, 2, state.targetID, now); err != nil {
				t.Fatalf("list proof: %v", err)
			}
			return protocol.Control{
				Type: protocol.TypeEdgeListed, RequestID: request.RequestID, EdgeNextCursor: next,
				EdgeRoutes: []protocol.EdgeRouteInfo{
					listedRoute("app.shaulavo.dev", "b", now), listedRoute("app.shaulavo.dev", "c", now),
				},
			}, nil
		default:
			return protocol.Control{}, fmt.Errorf("unexpected request %q", request.Type)
		}
	})
	routes, gotNext, err := publisher.ListPage(context.Background(), cursor, 2)
	if err != nil || len(routes) != 2 || gotNext != next {
		t.Fatalf("ListPage = %#v, %q, %v", routes, gotNext, err)
	}
}

func TestPublisherListPageRejectsMalformedPinnedResponses(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	cursor := encodeListCursor("app.shaulavo.dev", "/b")
	cases := map[string]protocol.Control{
		"before cursor": {
			Type: protocol.TypeEdgeListed, EdgeRoutes: []protocol.EdgeRouteInfo{listedRoute("app.shaulavo.dev", "a", now)},
		},
		"unordered": {
			Type: protocol.TypeEdgeListed, EdgeRoutes: []protocol.EdgeRouteInfo{
				listedRoute("app.shaulavo.dev", "d", now), listedRoute("app.shaulavo.dev", "c", now),
			},
		},
		"duplicate": {
			Type: protocol.TypeEdgeListed, EdgeRoutes: []protocol.EdgeRouteInfo{
				listedRoute("app.shaulavo.dev", "c", now), listedRoute("app.shaulavo.dev", "c", now),
			},
		},
		"wrong next cursor": {
			Type: protocol.TypeEdgeListed, EdgeRoutes: []protocol.EdgeRouteInfo{listedRoute("app.shaulavo.dev", "c", now)},
			EdgeNextCursor: encodeListCursor("app.shaulavo.dev", "/d"),
		},
		"remote error is secret": {Type: protocol.TypeError, Message: "ATTACKER\r\n100.64.0.9"},
	}
	for name, response := range cases {
		t.Run(name, func(t *testing.T) {
			state := &publisherMemoryOutbox{}
			publisher := testPublisher(t, now, state, func(request protocol.Control) (protocol.Control, error) {
				if request.Type == protocol.TypeHostInfo {
					return publisherHostResponse(request, state.targetID), nil
				}
				response.RequestID = request.RequestID
				return response, nil
			})
			_, _, err := publisher.ListPage(context.Background(), cursor, 2)
			if err == nil {
				t.Fatal("malformed edge.list response accepted")
			}
			if strings.Contains(err.Error(), "ATTACKER") || strings.Contains(err.Error(), "100.64") {
				t.Fatalf("remote response leaked through error: %v", err)
			}
		})
	}
	state := &publisherMemoryOutbox{}
	publisher := testPublisher(t, now, state, func(protocol.Control) (protocol.Control, error) {
		t.Fatal("network used for malformed input cursor")
		return protocol.Control{}, nil
	})
	if _, _, err := publisher.ListPage(context.Background(), "not*base64", 2); err == nil {
		t.Fatal("malformed input cursor accepted")
	}
}

func listedRoute(publicName, serviceName string, lastSeen time.Time) protocol.EdgeRouteInfo {
	return protocol.EdgeRouteInfo{
		PublicName: publicName, ServiceName: serviceName, DisplayAlias: "Desktop", LastSeenAt: lastSeen, Online: true,
	}
}

func testPublisher(t *testing.T, now time.Time, state *publisherMemoryOutbox, respond func(protocol.Control) (protocol.Control, error)) *Publisher {
	t.Helper()
	targetID, _ := testIdentity(t)
	originID, signer := testIdentity(t)
	state.targetID, state.originID, state.signer = targetID, originID, signer
	publisher, err := NewPublisher(PublisherConfig{
		Signer: signer,
		Target: TargetConfig{Identity: targetID, TailscaleName: "edge.example.ts.net", ControlPort: 7337, WebSocketPath: "/mesh"},
		State:  state,
		Resolve: func(context.Context, TargetConfig) (netip.AddrPort, error) {
			return netip.MustParseAddrPort("100.64.0.2:7337"), nil
		},
		Dial: func(context.Context, string) (transport.Conn, error) {
			return &publisherTestConn{respond: respond}, nil
		},
		Now: func() time.Time { return now }, RequestTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return publisher
}

func publisherHostResponse(request protocol.Control, identity string) protocol.Control {
	return protocol.Control{
		Type: protocol.TypeHostInfoResult, RequestID: request.RequestID,
		Host: &protocol.HostInfo{ID: identity, MeshIdentity: identity},
	}
}

type publisherMemoryOutbox struct {
	mu       sync.Mutex
	targetID string
	originID string
	signer   []byte
	record   OutboxRecord
	have     bool
}

func (s *publisherMemoryOutbox) LoadEdgeOutbox(context.Context, string) (OutboxRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.have {
		return OutboxRecord{}, ErrSnapshotNotFound
	}
	result := s.record
	result.Snapshot = cloneSnapshot(result.Snapshot)
	return result, nil
}

func (s *publisherMemoryOutbox) SaveEdgeOutbox(_ context.Context, record OutboxRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.have && record.Snapshot.Sequence <= s.record.Snapshot.Sequence {
		return errors.New("publisher test: sequence did not increase")
	}
	s.record = record
	s.record.Snapshot = cloneSnapshot(record.Snapshot)
	s.have = true
	return nil
}

func (s *publisherMemoryOutbox) AcknowledgeEdgeOutbox(_ context.Context, targetID string, sequence uint64, digest string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.have || targetID != s.targetID || sequence != s.record.Snapshot.Sequence || digest != s.record.Digest {
		return errors.New("publisher test: acknowledgement mismatch")
	}
	s.record.Acknowledged = true
	return nil
}

type publisherTestConn struct {
	mu       sync.Mutex
	respond  func(protocol.Control) (protocol.Control, error)
	response protocol.Frame
	readErr  error
	closed   bool
}

func (c *publisherTestConn) WriteFrame(frame protocol.Frame) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return io.ErrClosedPipe
	}
	request, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return err
	}
	response, err := c.respond(request)
	if err != nil {
		c.readErr = err
		return nil
	}
	payload, err := response.Encode()
	if err != nil {
		return err
	}
	c.response = protocol.Frame{Kind: protocol.KindControl, Payload: payload}
	return nil
}

func (c *publisherTestConn) ReadFrame() (protocol.Frame, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return protocol.Frame{}, io.ErrClosedPipe
	}
	if c.readErr != nil {
		err := c.readErr
		c.readErr = nil
		return protocol.Frame{}, err
	}
	frame := c.response
	c.response = protocol.Frame{}
	return frame, nil
}

func (c *publisherTestConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}
