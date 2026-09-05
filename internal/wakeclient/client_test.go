package wakeclient

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/tailnet"
	"github.com/shaul/mesh/internal/wake"
)

var errTargetOffline = errors.New("target offline")

func TestRefreshLearnsSignedPermissionFromPinnedTarget(t *testing.T) {
	client, grant, target := fixtureClient(t)
	target.Endpoint = serveTestPeer(t, func(_ context.Context, request protocol.Control) (protocol.Control, error) {
		return testHostInfo(request, grant.TargetID, "pc", &grant), nil
	})
	ctx := testContext(t)
	if err := client.Refresh(ctx, target); err != nil {
		t.Fatal(err)
	}
	learned, err := client.cache.Get(target.ID)
	if err != nil || learned.TargetID != target.ID || learned.Revision != grant.Revision {
		t.Fatalf("learned permission = %+v, %v", learned, err)
	}
	client.sender = stubSender{send: func(context.Context, wake.Grant) (bool, error) {
		t.Error("already online target was woken")
		return false, nil
	}}
	result, err := client.Wake(ctx, target)
	if err != nil || !result.AlreadyOnline {
		t.Fatalf("Wake = %+v, %v", result, err)
	}
}

func TestRefreshRejectsPermissionBelongingToAnotherTarget(t *testing.T) {
	client, _, target := fixtureClient(t)
	foreign := fixtureGrant(t, true)
	target.Endpoint = serveTestPeer(t, func(_ context.Context, request protocol.Control) (protocol.Control, error) {
		return testHostInfo(request, target.ID, "pc", &foreign), nil
	})
	err := client.Refresh(testContext(t), target)
	if err == nil || !strings.Contains(err.Error(), "another host") {
		t.Fatalf("Refresh = %v", err)
	}
	if _, err := client.cache.Get(target.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("foreign permission was cached under target: %v", err)
	}
}

func TestWakeRejectsForeignPermissionBeforeSending(t *testing.T) {
	client, _, target := rememberedClient(t)
	foreign := fixtureGrant(t, true)
	target.Endpoint = serveTestPeer(t, func(_ context.Context, request protocol.Control) (protocol.Control, error) {
		return testHostInfo(request, target.ID, "pc", &foreign), nil
	})
	var sends atomic.Int32
	client.sender = stubSender{
		probe: func(context.Context, wake.Grant) (wake.State, error) { return wake.Down, nil },
		send: func(context.Context, wake.Grant) (bool, error) {
			sends.Add(1)
			return false, errors.New("test refused unexpected wake")
		},
	}
	_, err := client.Wake(testContext(t), target)
	if err == nil || sends.Load() != 0 {
		t.Fatalf("Wake = %v; sent %d packets after foreign permission rejection", err, sends.Load())
	}
}

func TestWakeStopsWhenReadinessIdentityChanges(t *testing.T) {
	client, _, target := rememberedClient(t)
	var sent atomic.Bool
	client.sender = stubSender{
		probe: func(context.Context, wake.Grant) (wake.State, error) { return wake.Down, nil },
		send:  func(context.Context, wake.Grant) (bool, error) { sent.Store(true); return true, nil },
	}
	client.exchange = func(context.Context, string, string, protocol.Control) (protocol.HostInfo, protocol.Control, error) {
		if !sent.Load() {
			return protocol.HostInfo{}, protocol.Control{}, errTargetOffline
		}
		return protocol.HostInfo{}, protocol.Control{}, ErrIdentityChanged
	}
	ctx, cancel := context.WithTimeout(testContext(t), 300*time.Millisecond)
	defer cancel()
	if _, err := client.Wake(ctx, target); !errors.Is(err, ErrIdentityChanged) {
		t.Fatalf("readiness identity failure = %v, want immediate identity error", err)
	}
}

func TestWakeUsesLocalLANAndWaitsForTargetReadiness(t *testing.T) {
	client, grant, target := rememberedClient(t)
	sent := make(chan struct{})
	ready := make(chan struct{})
	var sends atomic.Int32
	client.sender = stubSender{
		probe: func(context.Context, wake.Grant) (wake.State, error) { return wake.Down, nil },
		send: func(context.Context, wake.Grant) (bool, error) {
			sends.Add(1)
			close(sent)
			return true, nil
		},
	}
	client.exchange = readyAfterSend(target, grant, sent, ready)
	done := startWake(client, testContext(t), target)
	waitSignal(t, sent)
	select {
	case result := <-done:
		t.Fatalf("returned before target readiness: %+v", result)
	default:
	}
	close(ready)
	result := waitWake(t, done)
	if result.err != nil || result.result.Sender != "this machine" || sends.Load() != 1 {
		t.Fatalf("Wake = %+v, sends = %d", result, sends.Load())
	}
}

func TestWakeFallsBackToRemoteLANOverWebSocket(t *testing.T) {
	client, grant, target := rememberedClient(t)
	pi := fixtureGrant(t, true)
	var sends atomic.Int32
	peer := serveTestPeer(t, func(_ context.Context, request protocol.Control) (protocol.Control, error) {
		if request.Type == protocol.TypeHostInfo {
			return testHostInfo(request, pi.TargetID, "pi.home", nil), nil
		}
		if request.Type == protocol.TypeWakeProbe {
			return protocol.Control{Type: protocol.TypeWakeProbed, WakeCanSend: true, WakeState: wake.Down}, nil
		}
		if request.Type != protocol.TypeWakeSend || request.WakeGrant == nil || request.WakeGrant.TargetID != target.ID {
			return protocol.Control{}, errors.New("unexpected remote wake request")
		}
		sends.Add(1)
		return protocol.Control{Type: protocol.TypeWakeSent}, nil
	})
	client.endpoints = staticEndpoints(peer)
	client.exchange = func(ctx context.Context, endpoint, expectedID string, request protocol.Control) (protocol.HostInfo, protocol.Control, error) {
		if endpoint != target.Endpoint {
			return exchange(ctx, endpoint, expectedID, request)
		}
		if expectedID != target.ID || sends.Load() == 0 {
			return protocol.HostInfo{}, protocol.Control{}, errTargetOffline
		}
		return hostWithGrant(grant), protocol.Control{}, nil
	}
	result, err := client.Wake(testContext(t), target)
	if err != nil || result.Sender != "pi.home" || sends.Load() != 1 {
		t.Fatalf("Wake = %+v, %v; sends = %d", result, err, sends.Load())
	}
}

func TestWakeLostAcknowledgementDoesNotSelectSecondSender(t *testing.T) {
	client, grant, target := rememberedClient(t)
	pi := fixtureGrant(t, true)
	other := fixtureGrant(t, true)
	hosts := map[string]protocol.HostInfo{
		"ws://a-pi/mesh":    {ID: pi.TargetID, TailscaleName: "ws://a-pi/mesh"},
		"ws://b-other/mesh": {ID: other.TargetID, TailscaleName: "ws://b-other/mesh"},
	}
	client.endpoints = staticEndpoints("ws://a-pi/mesh", "ws://b-other/mesh")
	sent := make(chan struct{})
	ready := make(chan struct{})
	var sends atomic.Int32
	readiness := readyAfterSend(target, grant, sent, ready)
	client.exchange = func(ctx context.Context, endpoint, expectedID string, request protocol.Control) (protocol.HostInfo, protocol.Control, error) {
		if endpoint == target.Endpoint {
			return readiness(ctx, endpoint, expectedID, request)
		}
		if request.Type == protocol.TypeWakeProbe {
			return hosts[endpoint], protocol.Control{Type: protocol.TypeWakeProbed, WakeCanSend: true, WakeState: wake.Down}, nil
		}
		if endpoint != "ws://a-pi/mesh" || expectedID != pi.TargetID {
			t.Errorf("send was not pinned to first selected peer: %s / %s", endpoint, expectedID)
		}
		sends.Add(1)
		close(sent)
		return protocol.HostInfo{}, protocol.Control{}, errors.New("acknowledgement connection reset")
	}
	done := startWake(client, testContext(t), target)
	waitSignal(t, sent)
	select {
	case result := <-done:
		t.Fatalf("lost acknowledgement returned before target readiness: %+v", result)
	default:
	}
	close(ready)
	result := waitWake(t, done)
	if result.err != nil || sends.Load() != 1 || result.result.Sender != "ws://a-pi/mesh" {
		t.Fatalf("Wake = %+v, sends = %d", result, sends.Load())
	}
}

func TestObserveNeverSendsAndRecoverRequiresConfirmedDown(t *testing.T) {
	client, _, target := rememberedClient(t)
	pi := fixtureGrant(t, true)
	client.endpoints = staticEndpoints("ws://pi/mesh")
	var state atomic.Value
	state.Store(wake.Unknown)
	var sends atomic.Int32
	client.sender = stubSender{send: func(context.Context, wake.Grant) (bool, error) {
		sends.Add(1)
		return true, nil
	}}
	client.exchange = func(_ context.Context, _, _ string, request protocol.Control) (protocol.HostInfo, protocol.Control, error) {
		if request.Type != protocol.TypeWakeProbe {
			sends.Add(1)
			return protocol.HostInfo{}, protocol.Control{}, errors.New("observation attempted wake or readiness")
		}
		return protocol.HostInfo{ID: pi.TargetID}, protocol.Control{Type: protocol.TypeWakeProbed, WakeCanSend: true, WakeState: state.Load().(wake.State)}, nil
	}
	ctx := testContext(t)
	for _, observed := range []wake.State{wake.Down, wake.Unknown, wake.Reachable} {
		state.Store(observed)
		assertObservation(t, client, ctx, target, observed)
	}
	for _, observed := range []wake.State{wake.Unknown, wake.Reachable} {
		state.Store(observed)
		if err := client.Recover(ctx, target); !errors.Is(err, ErrObservationUnavailable) {
			t.Fatalf("inconclusive recovery = %v", err)
		}
	}
	if sends.Load() != 0 {
		t.Fatalf("observation/recovery sent %d wake requests", sends.Load())
	}
}

func TestRecoverDoesNotWakeWhenClientNetworkIsAbsent(t *testing.T) {
	client, _, target := rememberedClient(t)
	client.endpoints = staticEndpoints("ws://pi/mesh")
	var localProbes, sends atomic.Int32
	client.sender = stubSender{
		probe: func(context.Context, wake.Grant) (wake.State, error) { localProbes.Add(1); return wake.Down, nil },
		send:  func(context.Context, wake.Grant) (bool, error) { sends.Add(1); return true, nil },
	}
	client.exchange = func(context.Context, string, string, protocol.Control) (protocol.HostInfo, protocol.Control, error) {
		return protocol.HostInfo{}, protocol.Control{}, errors.New("network unreachable")
	}
	if err := client.Recover(testContext(t), target); err == nil {
		t.Fatal("network failure was not reported")
	}
	if localProbes.Load() != 0 || sends.Load() != 0 {
		t.Fatalf("used client as its own witness: probes = %d, sends = %d", localProbes.Load(), sends.Load())
	}
}

func TestObserveExcludesLocalDaemonAsWitness(t *testing.T) {
	client, _, target := rememberedClient(t)
	self := fixtureGrant(t, true)
	client.selfID = self.TargetID
	client.endpoints = staticEndpoints("ws://self/mesh")
	client.exchange = func(context.Context, string, string, protocol.Control) (protocol.HostInfo, protocol.Control, error) {
		return protocol.HostInfo{ID: self.TargetID}, protocol.Control{Type: protocol.TypeWakeProbed, WakeCanSend: true, WakeState: wake.Down}, nil
	}
	if observation, err := client.Observe(testContext(t), target); err == nil || observation.State != wake.Unknown {
		t.Fatalf("own daemon supplied independent witness: %+v, %v", observation, err)
	}
}

func TestObserveFindsConclusiveWitnessAfterUnknownPeer(t *testing.T) {
	client, _, target := rememberedClient(t)
	first, second := fixtureGrant(t, true), fixtureGrant(t, true)
	client.endpoints = staticEndpoints("ws://a/mesh", "ws://b/mesh")
	client.exchange = func(_ context.Context, endpoint, _ string, _ protocol.Control) (protocol.HostInfo, protocol.Control, error) {
		if endpoint == "ws://a/mesh" {
			return protocol.HostInfo{ID: first.TargetID}, protocol.Control{Type: protocol.TypeWakeProbed, WakeCanSend: true, WakeState: wake.Unknown}, nil
		}
		return protocol.HostInfo{ID: second.TargetID}, protocol.Control{Type: protocol.TypeWakeProbed, WakeCanSend: true, WakeState: wake.Down}, nil
	}
	observation, err := client.Observe(testContext(t), target)
	if err != nil || observation.State != wake.Down {
		t.Fatalf("observation = %+v, %v", observation, err)
	}
}

func TestRecoverWakesAfterIndependentLANWitnessConfirmsDown(t *testing.T) {
	client, grant, target := rememberedClient(t)
	pi := fixtureGrant(t, true)
	client.endpoints = staticEndpoints("ws://pi/mesh")
	var witnessProbes, sends atomic.Int32
	client.sender = stubSender{
		probe: func(context.Context, wake.Grant) (wake.State, error) { return wake.Down, nil },
		send:  func(context.Context, wake.Grant) (bool, error) { sends.Add(1); return true, nil },
	}
	client.exchange = func(_ context.Context, endpoint, _ string, request protocol.Control) (protocol.HostInfo, protocol.Control, error) {
		if request.Type == protocol.TypeWakeProbe {
			witnessProbes.Add(1)
			return protocol.HostInfo{ID: pi.TargetID}, protocol.Control{Type: protocol.TypeWakeProbed, WakeCanSend: true, WakeState: wake.Down}, nil
		}
		if endpoint != target.Endpoint || sends.Load() == 0 {
			return protocol.HostInfo{}, protocol.Control{}, errTargetOffline
		}
		return hostWithGrant(grant), protocol.Control{}, nil
	}
	if err := client.Recover(testContext(t), target); err != nil {
		t.Fatal(err)
	}
	if witnessProbes.Load() != 1 || sends.Load() != 1 {
		t.Fatalf("Recover witness probes = %d, sends = %d", witnessProbes.Load(), sends.Load())
	}
}

func TestWakeReportsNoSenderOnTargetsLAN(t *testing.T) {
	client, _, target := rememberedClient(t)
	pi := fixtureGrant(t, true)
	client.endpoints = staticEndpoints("ws://pi/mesh")
	client.exchange = func(_ context.Context, endpoint, _ string, request protocol.Control) (protocol.HostInfo, protocol.Control, error) {
		if endpoint == target.Endpoint {
			return protocol.HostInfo{}, protocol.Control{}, errTargetOffline
		}
		if request.Type != protocol.TypeWakeProbe {
			t.Error("sent to peer on another LAN")
		}
		return protocol.HostInfo{ID: pi.TargetID}, protocol.Control{Type: protocol.TypeWakeProbed, WakeCanSend: false, WakeState: wake.Unknown}, nil
	}
	_, err := client.Wake(testContext(t), target)
	if err == nil || !strings.Contains(err.Error(), "LAN") {
		t.Fatalf("Wake = %v", err)
	}
}

func TestExplicitWakeCanUseLANWithUnknownTargetState(t *testing.T) {
	client, grant, target := rememberedClient(t)
	var sent atomic.Bool
	client.sender = stubSender{
		probe: func(context.Context, wake.Grant) (wake.State, error) { return wake.Unknown, nil },
		send:  func(context.Context, wake.Grant) (bool, error) { sent.Store(true); return true, nil },
	}
	client.exchange = func(context.Context, string, string, protocol.Control) (protocol.HostInfo, protocol.Control, error) {
		if !sent.Load() {
			return protocol.HostInfo{}, protocol.Control{}, errTargetOffline
		}
		return hostWithGrant(grant), protocol.Control{}, nil
	}
	if _, err := client.Wake(testContext(t), target); err != nil || !sent.Load() {
		t.Fatalf("explicit Wake = %v, sent = %v", err, sent.Load())
	}
}

func TestConcurrentWakeSharesSendAndIsolatesCancellation(t *testing.T) {
	client, grant, target := rememberedClient(t)
	sent := make(chan struct{})
	ready := make(chan struct{})
	var sends atomic.Int32
	client.sender = stubSender{
		probe: func(context.Context, wake.Grant) (wake.State, error) { return wake.Down, nil },
		send:  func(context.Context, wake.Grant) (bool, error) { sends.Add(1); close(sent); return true, nil },
	}
	client.exchange = readyAfterSend(target, grant, sent, ready)
	firstCtx, cancelFirst := context.WithCancel(testContext(t))
	defer cancelFirst()
	first := startWake(client, firstCtx, target)
	waitSignal(t, sent)
	second := startWake(client, testContext(t), target)
	waitForWaiters(t, client, target.ID, 2)
	cancelFirst()
	if result := waitWake(t, first); !errors.Is(result.err, context.Canceled) {
		t.Fatalf("first caller = %+v", result)
	}
	close(ready)
	result := waitWake(t, second)
	if result.err != nil || result.result.Sender != "this machine" || sends.Load() != 1 {
		t.Fatalf("second caller = %+v, sends = %d", result, sends.Load())
	}
}

func TestLastCancelledWaiterStopsReadinessWork(t *testing.T) {
	client, _, target := rememberedClient(t)
	var sent atomic.Bool
	checking := make(chan struct{})
	stopped := make(chan struct{})
	client.sender = stubSender{
		probe: func(context.Context, wake.Grant) (wake.State, error) { return wake.Down, nil },
		send:  func(context.Context, wake.Grant) (bool, error) { sent.Store(true); return true, nil },
	}
	client.exchange = func(ctx context.Context, _, _ string, _ protocol.Control) (protocol.HostInfo, protocol.Control, error) {
		if !sent.Load() {
			return protocol.HostInfo{}, protocol.Control{}, errTargetOffline
		}
		close(checking)
		<-ctx.Done()
		close(stopped)
		return protocol.HostInfo{}, protocol.Control{}, ctx.Err()
	}
	ctx, cancel := context.WithCancel(testContext(t))
	defer cancel()
	done := startWake(client, ctx, target)
	waitSignal(t, checking)
	cancel()
	if result := waitWake(t, done); !errors.Is(result.err, context.Canceled) {
		t.Fatalf("cancelled caller = %+v", result)
	}
	waitSignal(t, stopped)
	waitForWaiters(t, client, target.ID, 0)
}

func TestWakeRejectsDisabledTargetPermission(t *testing.T) {
	client, _, _ := fixtureClient(t)
	grant := fixtureGrant(t, false)
	target := Target{ID: grant.TargetID, Name: "pc", Endpoint: "ws://pc/mesh"}
	if err := client.Remember(grant); err != nil {
		t.Fatal(err)
	}
	client.exchange = func(context.Context, string, string, protocol.Control) (protocol.HostInfo, protocol.Control, error) {
		return protocol.HostInfo{}, protocol.Control{}, errTargetOffline
	}
	if _, err := client.Wake(testContext(t), target); !errors.Is(err, wake.ErrDisabled) {
		t.Fatalf("Wake = %v", err)
	}
}

type stubSender struct {
	probe func(context.Context, wake.Grant) (wake.State, error)
	send  func(context.Context, wake.Grant) (bool, error)
}

func (s stubSender) Probe(ctx context.Context, grant wake.Grant) (wake.State, error) {
	if s.probe == nil {
		return wake.Unknown, wake.ErrNoLAN
	}
	return s.probe(ctx, grant)
}

func (s stubSender) Send(ctx context.Context, grant wake.Grant) (bool, error) {
	if s.send == nil {
		return false, errors.New("unexpected local wake send")
	}
	return s.send(ctx, grant)
}

func fixtureGrant(t *testing.T, enabled bool) wake.Grant {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := wake.NewAuthorityWithOptions(t.TempDir(), key, wake.AuthorityOptions{
		Discover: func(context.Context) (wake.NIC, error) {
			return wake.NIC{MAC: "02:00:00:00:00:02", Address: "192.168.50.20", Prefix: "192.168.50.0/24", GatewayMAC: "02:00:00:00:00:01"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	grant, err := authority.SetAllowed(context.Background(), enabled)
	if err != nil {
		t.Fatal(err)
	}
	return grant
}

func fixtureClient(t *testing.T) (*Client, wake.Grant, Target) {
	t.Helper()
	client, err := New(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	client.peers = func(context.Context) ([]tailnet.Peer, error) { return nil, nil }
	client.sender = stubSender{}
	grant := fixtureGrant(t, true)
	return client, grant, Target{ID: grant.TargetID, Name: "pc", Endpoint: "ws://pc.test:7337/mesh"}
}

func rememberedClient(t *testing.T) (*Client, wake.Grant, Target) {
	t.Helper()
	client, grant, target := fixtureClient(t)
	if err := client.Remember(grant); err != nil {
		t.Fatal(err)
	}
	return client, grant, target
}

func hostWithGrant(grant wake.Grant) protocol.HostInfo {
	return protocol.HostInfo{ID: grant.TargetID, MeshIdentity: grant.TargetID, Wake: &grant}
}

func staticEndpoints(endpoints ...string) func(context.Context) ([]string, error) {
	return func(context.Context) ([]string, error) { return endpoints, nil }
}

func readyAfterSend(target Target, grant wake.Grant, sent, ready <-chan struct{}) func(context.Context, string, string, protocol.Control) (protocol.HostInfo, protocol.Control, error) {
	return func(ctx context.Context, endpoint, expectedID string, _ protocol.Control) (protocol.HostInfo, protocol.Control, error) {
		if endpoint != target.Endpoint || expectedID != target.ID {
			return protocol.HostInfo{}, protocol.Control{}, errors.New("readiness target was not pinned")
		}
		select {
		case <-sent:
		default:
			return protocol.HostInfo{}, protocol.Control{}, errTargetOffline
		}
		select {
		case <-ready:
			return hostWithGrant(grant), protocol.Control{}, nil
		case <-ctx.Done():
			return protocol.HostInfo{}, protocol.Control{}, ctx.Err()
		}
	}
}

type wakeAnswer struct {
	result Result
	err    error
}

func startWake(client *Client, ctx context.Context, target Target) <-chan wakeAnswer {
	done := make(chan wakeAnswer, 1)
	go func() {
		result, err := client.Wake(ctx, target)
		done <- wakeAnswer{result: result, err: err}
	}()
	return done
}

func waitWake(t *testing.T, done <-chan wakeAnswer) wakeAnswer {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(3 * time.Second):
		t.Fatal("wake call did not finish")
		return wakeAnswer{}
	}
}

func waitSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("wake stage did not complete")
	}
}

func waitForWaiters(t *testing.T, client *Client, id string, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		client.mu.Lock()
		count := waiterCount(client.flights[id])
		client.mu.Unlock()
		if count == want {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("wake operation did not have %d waiters", want)
}

func waiterCount(f *flight) int {
	if f == nil {
		return 0
	}
	return f.waiters
}

func assertObservation(t *testing.T, client *Client, ctx context.Context, target Target, want wake.State) {
	t.Helper()
	observation, err := client.Observe(ctx, target)
	if err != nil || observation.State != want {
		t.Fatalf("Observe = %+v, %v; want %s", observation, err, want)
	}
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	return ctx
}
