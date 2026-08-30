package edge

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
)

func TestSnapshotSignatureBindsCompleteCanonicalState(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	edgeID, _ := testIdentity(t)
	originID, originKey := testIdentity(t)
	snapshot := NewSnapshot(edgeID, originID, 7, now, now.Add(5*time.Minute), []Route{
		{PublicName: "web.shaulavo.dev", ServiceName: "docs", WakeOnRequest: true},
		{PublicName: "api.shaulavo.dev", ServiceName: "v1"},
	})
	if snapshot.Routes[0].PublicName != "api.shaulavo.dev" {
		t.Fatalf("routes are not canonical: %#v", snapshot.Routes)
	}
	signed, err := SignSnapshot(snapshot, originKey, now)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := VerifySnapshot(signed, edgeID, originID)
	if err != nil || len(digest) != 64 {
		t.Fatalf("verify digest = %q, error = %v", digest, err)
	}

	mutations := map[string]func(*Snapshot){
		"target":    func(candidate *Snapshot) { candidate.TargetID, _ = testIdentity(t) },
		"origin":    func(candidate *Snapshot) { candidate.OriginID, _ = testIdentity(t) },
		"sequence":  func(candidate *Snapshot) { candidate.Sequence++ },
		"issued":    func(candidate *Snapshot) { candidate.IssuedAt = candidate.IssuedAt.Add(time.Second) },
		"expires":   func(candidate *Snapshot) { candidate.ExpiresAt = candidate.ExpiresAt.Add(time.Second) },
		"public":    func(candidate *Snapshot) { candidate.Routes[0].PublicName = "new.shaulavo.dev" },
		"service":   func(candidate *Snapshot) { candidate.Routes[0].ServiceName = "v2" },
		"wake":      func(candidate *Snapshot) { candidate.Routes[0].WakeOnRequest = true },
		"signature": func(candidate *Snapshot) { candidate.Signature[0] ^= 1 },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			candidate := cloneSnapshot(signed)
			mutate(&candidate)
			if _, err := VerifySnapshot(candidate, candidate.TargetID, candidate.OriginID); err == nil {
				t.Fatal("tampered snapshot verified")
			}
		})
	}
}

func TestSnapshotRejectsNoncanonicalAndUnboundedState(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	edgeID, _ := testIdentity(t)
	originID, key := testIdentity(t)
	valid := NewSnapshot(edgeID, originID, 1, now, now.Add(5*time.Minute), []Route{{PublicName: "app.shaulavo.dev", ServiceName: "app"}})

	cases := map[string]func(*Snapshot){
		"zero sequence":  func(snapshot *Snapshot) { snapshot.Sequence = 0 },
		"large sequence": func(snapshot *Snapshot) { snapshot.Sequence = math.MaxInt64 + 1 },
		"future": func(snapshot *Snapshot) {
			snapshot.IssuedAt = now.Add(3 * time.Minute)
			snapshot.ExpiresAt = now.Add(8 * time.Minute)
		},
		"expired": func(snapshot *Snapshot) {
			snapshot.IssuedAt = now.Add(-6 * time.Minute)
			snapshot.ExpiresAt = now.Add(-time.Minute)
		},
		"long TTL":       func(snapshot *Snapshot) { snapshot.ExpiresAt = now.Add(16 * time.Minute) },
		"apex":           func(snapshot *Snapshot) { snapshot.Routes[0].PublicName = "shaulavo.dev" },
		"nested":         func(snapshot *Snapshot) { snapshot.Routes[0].PublicName = "a.b.shaulavo.dev" },
		"private name":   func(snapshot *Snapshot) { snapshot.Routes[0].PublicName = "mesh.shaulavo.dev" },
		"terminal":       func(snapshot *Snapshot) { snapshot.Routes[0].ServiceName = "mesh" },
		"terminal child": func(snapshot *Snapshot) { snapshot.Routes[0].ServiceName = "mesh/control" },
		"unsorted": func(snapshot *Snapshot) {
			snapshot.Routes = []Route{{PublicName: "z.shaulavo.dev", ServiceName: "z"}, {PublicName: "a.shaulavo.dev", ServiceName: "a"}}
		},
		"duplicate": func(snapshot *Snapshot) { snapshot.Routes = append(snapshot.Routes, snapshot.Routes[0]) },
		"large name": func(snapshot *Snapshot) {
			snapshot.Routes[0].ServiceName = strings.Repeat("a", maximumServiceNameBytes+1)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			candidate := cloneSnapshot(valid)
			mutate(&candidate)
			if _, err := SignSnapshot(candidate, key, now); err == nil {
				t.Fatal("invalid snapshot signed")
			}
		})
	}

	tooMany := cloneSnapshot(valid)
	tooMany.Routes = make([]Route, MaximumRoutes+1)
	for index := range tooMany.Routes {
		tooMany.Routes[index] = Route{PublicName: "app.shaulavo.dev", ServiceName: strings.Repeat("a", index/26) + string(rune('a'+index%26))}
	}
	if _, err := SignSnapshot(tooMany, key, now); err == nil {
		t.Fatal("too many routes signed")
	}
}

func TestMaximalRegistrationAndListPageFitOneBoundedFrame(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	edgeID, _ := testIdentity(t)
	originID, key := testIdentity(t)
	routes := make([]Route, MaximumRoutes)
	for index := range routes {
		routes[index] = Route{
			PublicName:    strings.Repeat("a", 63) + ".shaulavo.dev",
			ServiceName:   fmt.Sprintf("%03d%s", index, strings.Repeat("a", maximumServiceNameBytes-3)),
			WakeOnRequest: index%2 == 0,
		}
	}
	signed, err := SignSnapshot(NewSnapshot(edgeID, originID, 1, now, now.Add(5*time.Minute), routes), key, now)
	if err != nil {
		t.Fatal(err)
	}
	registration := snapshotToProtocol(signed)
	assertControlFitsFrame(t, protocol.Control{Type: protocol.TypeEdgeRegister, RequestID: "max-registration", EdgeSnapshot: &registration})

	page := make([]protocol.EdgeRouteInfo, maximumListLimit)
	for index := range page {
		page[index] = protocol.EdgeRouteInfo{
			PublicName: routes[index].PublicName, ServiceName: routes[index].ServiceName,
			DisplayAlias: strings.Repeat("d", 128), LastSeenAt: now, Online: true,
		}
	}
	assertControlFitsFrame(t, protocol.Control{Type: protocol.TypeEdgeListed, RequestID: "max-list-page", EdgeRoutes: page, EdgeNextCursor: encodeListCursor(routes[99].PublicName, "/"+routes[99].ServiceName)})
}

func assertControlFitsFrame(t *testing.T, control protocol.Control) {
	t.Helper()
	payload, err := control.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) > protocol.MaxPayload {
		t.Fatalf("control payload = %d, exceeds %d", len(payload), protocol.MaxPayload)
	}
	var framed bytes.Buffer
	if err := protocol.NewWriter(&framed).WriteControlMsg(control); err != nil {
		t.Fatal(err)
	}
}

func testIdentity(t *testing.T) (string, ed25519.PrivateKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(publicKey), privateKey
}

func cloneSnapshot(snapshot Snapshot) Snapshot {
	snapshot.Routes = append([]Route(nil), snapshot.Routes...)
	snapshot.Signature = append([]byte(nil), snapshot.Signature...)
	return snapshot
}
