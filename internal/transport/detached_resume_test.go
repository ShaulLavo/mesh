package transport

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/shaul/mesh/internal/protocol"
)

func TestDetachedAttachReconnectKeepsClaimModeAndNesting(t *testing.T) {
	t.Parallel()
	old, next := newTestLink(false), newTestLink(false)
	ref := connectionRef{link: old, generation: 1}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn := &reconnectingConn{
		ctx: ctx, cancel: cancel,
		dial:    func(context.Context, string, websocket.DialOptions, KeepAlive) (linkConn, error) { return next, nil },
		backoff: Backoff{Initial: time.Millisecond, Max: time.Millisecond},
		current: ref, generation: 1, resumes: make(map[string]resumeState),
	}
	defer conn.Close() //nolint:errcheck // test resource cleanup
	containing := []protocol.SessionIdentity{{HostID: "host-a", SessionID: "AAAA"}}
	request := protocol.Control{Type: protocol.TypeAttachDetached, SessionID: "BBBB", ContainingSessions: containing}
	payload, err := request.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	attached, err := (protocol.Control{Type: protocol.TypeAttached, SessionID: "BBBB", Seq: 21, NestingSupported: true}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, drop, err := conn.observeFrame(ref, protocol.Frame{Kind: protocol.KindControl, Payload: attached}); err != nil || drop {
		t.Fatalf("attached frame: drop = %v, error = %v", drop, err)
	}
	sid, _ := protocol.NewSessionID("BBBB")
	if _, drop, err := conn.observeFrame(ref, protocol.Frame{Kind: protocol.KindData, Session: sid, Seq: 21, Payload: []byte("abc")}); err != nil || drop {
		t.Fatalf("data frame: drop = %v, error = %v", drop, err)
	}
	nesting, err := (protocol.Control{
		Type: protocol.TypeNesting, SessionID: "BBBB", NestingSupported: true,
		Nested: []protocol.SessionIdentity{{HostID: "host-c", SessionID: "CCCC"}},
	}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	frame := protocol.Frame{Kind: protocol.KindControl, Payload: nesting}
	got, drop, err := conn.observeFrame(ref, frame)
	if err != nil || drop {
		t.Fatalf("nesting frame: drop = %v, error = %v", drop, err)
	}
	assertFrameEqual(t, got, frame)
	resumedRef, err := conn.connection(ref)
	if err != nil {
		t.Fatal(err)
	}
	writes := next.snapshot()
	if len(writes) != 1 {
		t.Fatalf("reconnect sent %d frames, want one conditional resume", len(writes))
	}
	resume, err := protocol.DecodeControl(writes[0].Payload)
	if err != nil {
		t.Fatal(err)
	}
	if resume.Type != protocol.TypeAttachDetached || resume.LastSeq == nil || *resume.LastSeq != 24 || !slices.Equal(resume.ContainingSessions, containing) {
		t.Fatalf("resume changed claim mode, position, or containment: %#v", resume)
	}
	refusal, err := (protocol.Control{Type: protocol.TypeError, SessionID: "BBBB", Reason: protocol.ReasonAttached}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, drop, err := conn.observeFrame(resumedRef, protocol.Frame{Kind: protocol.KindControl, Payload: refusal}); err != nil || drop {
		t.Fatalf("refusal frame: drop = %v, error = %v", drop, err)
	}
	frames, err := conn.resumeFrames()
	if err != nil || len(frames) != 0 {
		t.Fatalf("resume frames after another window claimed session = %#v, %v", frames, err)
	}
}

func TestRefusedDetachedAttachIsNotResumed(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{protocol.ReasonAttached, ""} {
		t.Run(reason, func(t *testing.T) {
			link := newTestLink(false)
			ref := connectionRef{link: link, generation: 1}
			conn := &reconnectingConn{current: ref, generation: 1, resumes: make(map[string]resumeState)}
			request, err := (protocol.Control{Type: protocol.TypeAttachDetached, SessionID: "BBBB"}).Encode()
			if err != nil {
				t.Fatal(err)
			}
			if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: request}); err != nil {
				t.Fatal(err)
			}
			response, err := (protocol.Control{Type: protocol.TypeError, SessionID: "BBBB", Reason: reason}).Encode()
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := conn.observeFrame(ref, protocol.Frame{Kind: protocol.KindControl, Payload: response}); err != nil {
				t.Fatal(err)
			}
			frames, err := conn.resumeFrames()
			if err != nil || len(frames) != 0 {
				t.Fatalf("refused claim resume frames = %#v, %v", frames, err)
			}
		})
	}
}
