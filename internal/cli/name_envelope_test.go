package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

func TestReviewerMixedStateEnvelopeCannotAdmitName(t *testing.T) {
	for _, mode := range []string{"foreign-extra-snapshot", "extra-service-payload"} {
		t.Run(mode, func(t *testing.T) {
			f := namedDestination(t)
			info := f.info()
			view := StateView{Sections: map[string]ObservedSection{}}
			now := time.Now()
			initial := protocol.Control{Type: protocol.TypeStateSnapshot, StateSnapshot: &protocol.StateSnapshot{Seq: 1, Host: &info}}
			if err := applyVerifiedState(t.Context(), f.host, &view, initial, now, 0); err != nil {
				t.Fatal(err)
			}
			next := info
			next.MachineName, next.NameRevision = "renamed", 2
			message := protocol.Control{Type: protocol.TypeStateEvent, StateEvent: &protocol.StateEvent{Seq: 2, Kind: "host.changed", Payload: protocol.StatePayload{Host: &next}}}
			if mode == "foreign-extra-snapshot" {
				foreign := info
				foreign.ID, foreign.MachineName, foreign.NameRevision = "foreign-owner", "forged", 99
				message.StateSnapshot = &protocol.StateSnapshot{Seq: 99, Host: &foreign}
			} else {
				message.StateEvent.Payload.Service = &protocol.ServiceInfo{}
			}
			if err := applyVerifiedState(t.Context(), f.host, &view, message, now.Add(time.Second), 0); err == nil {
				t.Error("accepted mixed state envelope")
			}
			if view.Seq != 1 || view.Name != declaredName(info) {
				t.Error("mixed envelope changed the shown name/sequence")
			}
			path, err := ConfigPath()
			if err != nil {
				t.Fatal(err)
			}
			cached, err := readNameCacheFixtureClaim(filepath.Dir(path), f.host.ID)
			if err != nil {
				t.Fatal(err)
			}
			if cached != declaredName(info) {
				t.Error("mixed envelope changed the cached name")
			}
		})
	}
}

func TestReviewerMixedPollEnvelopeCannotAdmitName(t *testing.T) {
	f := namedDestination(t)
	info := f.info()
	if err := rememberHostName(t.Context(), f.host, info); err != nil {
		t.Fatal(err)
	}
	view := StateView{Name: declaredName(info), NameVerified: true, Sections: map[string]ObservedSection{}}
	next := info
	next.MachineName, next.NameRevision = "renamed", 2
	foreign := info
	foreign.ID, foreign.MachineName, foreign.NameRevision = "foreign-owner", "forged", 99
	response := protocol.Control{Type: protocol.TypeHostInfoResult, Host: &next, StateEvent: &protocol.StateEvent{Seq: 99, Kind: "host.changed", Payload: protocol.StatePayload{Host: &foreign}}}
	if err := applyVerifiedPoll(t.Context(), f.host, protocol.TopicHost, response, &view, time.Now(), 0); err == nil {
		t.Error("accepted mixed polling envelope")
	}
	if view.Name != declaredName(info) {
		t.Error("mixed polling envelope changed shown name")
	}
	path, err := ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	cached, err := readNameCacheFixtureClaim(filepath.Dir(path), f.host.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cached != declaredName(info) {
		t.Error("mixed polling envelope changed cached name")
	}
}

func TestReviewerRefusalPrecedesCreateAndControl(t *testing.T) {
	f := namedDestination(t)
	if err := SaveHost(f.host); err != nil {
		t.Fatal(err)
	}
	conn, _, err := openVerifiedHostInfo(t.Context(), f.host, dialControlHost)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	hosts, err := LoadHosts()
	if err != nil {
		t.Fatal(err)
	}
	target, err := ResolveArgument("destination", hosts)
	if err != nil || target.Host == nil {
		t.Fatal("unique fixture target failed")
	}
	if _, _, err := f.names.Rename(t.Context(), f.host.ID, "renamed", 1); err != nil {
		t.Fatal(err)
	}
	controls := 0
	dial := func(ctx context.Context, host HostRecord) (transport.Conn, error) {
		conn, err := dialControlHost(ctx, host)
		if err != nil {
			return nil, err
		}
		return &reviewerCountConn{Conn: conn, controls: &controls}, nil
	}
	if _, err := createRemoteSession(t.Context(), *target.Host, dial, []string{"fixture-command"}, 80, 24); err == nil {
		t.Fatal("stale name create succeeded")
	}
	if err := controlRemoteSession(t.Context(), *target.Host, dial, "7K3D", protocol.TypeSignal, "TERM"); err == nil {
		t.Fatal("stale name control succeeded")
	}
	if controls != 0 {
		t.Fatalf("sent %d session-effect controls on refusal", controls)
	}
}

type reviewerCountConn struct {
	transport.Conn
	controls *int
}

func (c *reviewerCountConn) WriteFrame(frame protocol.Frame) error {
	if frame.Kind == protocol.KindControl {
		message, err := protocol.DecodeControl(frame.Payload)
		if err != nil {
			return fmt.Errorf("decode fixture control: %w", err)
		}
		if message.Type != protocol.TypeHostInfo {
			*c.controls++
		}
	}
	if err := c.Conn.WriteFrame(frame); err != nil {
		return fmt.Errorf("write fixture frame: %w", err)
	}
	return nil
}

func TestReviewerMixedHostInfoEnvelopeCannotAdmitName(t *testing.T) {
	f := namedDestination(t)
	original := f.info()
	conn, _, err := openVerifiedHostInfo(t.Context(), f.host, dialControlHost)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if _, _, err := f.names.Rename(t.Context(), f.host.ID, "renamed", 1); err != nil {
		t.Fatal(err)
	}
	dial := func(ctx context.Context, host HostRecord) (transport.Conn, error) {
		conn, err := dialControlHost(ctx, host)
		if err != nil {
			return nil, err
		}
		return &reviewerMixedHostConn{Conn: conn}, nil
	}
	conn, _, err = openVerifiedHostInfo(t.Context(), f.host, dial)
	if err == nil {
		t.Error("accepted mixed authenticated host-info envelope")
	}
	if conn != nil {
		_ = conn.Close()
	}
	path, err := ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	cached, err := readNameCacheFixtureClaim(filepath.Dir(path), f.host.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cached != declaredName(original) {
		t.Error("mixed host-info envelope changed cached name")
	}
}

type reviewerMixedHostConn struct{ transport.Conn }

func (c *reviewerMixedHostConn) ReadFrame() (protocol.Frame, error) {
	frame, err := c.Conn.ReadFrame()
	if err != nil {
		return frame, fmt.Errorf("read fixture frame: %w", err)
	}
	if frame.Kind != protocol.KindControl {
		return frame, nil
	}
	message, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return protocol.Frame{}, fmt.Errorf("decode fixture frame: %w", err)
	}
	if message.Type == protocol.TypeHostInfoResult {
		message.StateEvent = &protocol.StateEvent{Seq: 99, Kind: "host.changed", Payload: protocol.StatePayload{Host: &protocol.HostInfo{ID: "foreign-owner", MachineName: "forged", NameRevision: 99}}}
		frame.Payload, err = message.Encode()
	}
	if err != nil {
		return frame, fmt.Errorf("encode fixture frame: %w", err)
	}
	return frame, nil
}

func TestNamingEnvelopeAcceptsEventAndMatchingCurrent(t *testing.T) {
	f := namedDestination(t)
	info := f.info()
	view := StateView{Sections: map[string]ObservedSection{}}
	now := time.Now()
	initial := protocol.Control{Type: protocol.TypeStateSnapshot, StateSnapshot: &protocol.StateSnapshot{Seq: 1, Host: &info}}
	if err := applyVerifiedState(t.Context(), f.host, &view, initial, now, 0); err != nil {
		t.Fatal(err)
	}
	next := info
	next.MachineName, next.NameRevision = "renamed", 2
	response := protocol.Control{Type: protocol.TypeStateEvent, StateEvent: &protocol.StateEvent{Seq: 2, Kind: "host.changed", Payload: protocol.StatePayload{Host: &next}}, StateCurrent: &protocol.StateCurrent{Seq: 2, Sections: map[string]protocol.Observation{protocol.TopicHost: {}}}}
	if err := applyVerifiedState(t.Context(), f.host, &view, response, now, 0); err != nil {
		t.Fatal(err)
	}
	if view.Seq != 2 || view.Name != declaredName(next) {
		t.Fatalf("legitimate event pairing failed: %+v", view.Name)
	}
}
