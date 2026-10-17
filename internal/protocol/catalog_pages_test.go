package protocol

import (
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestCatalogPagesPreserveListsAndSnapshots(t *testing.T) {
	rows := make([]SessionInfo, 168)
	for i := range rows {
		rows[i] = SessionInfo{ID: "TEST", Command: []string{"sh", "-c", strings.Repeat("x", 64<<10)}}
	}
	for _, message := range []Control{
		{Type: TypeListed, RequestID: "list", Sessions: rows},
		{Type: TypeStateSnapshot, RequestID: "watch", StateSnapshot: &StateSnapshot{Seq: 9, Sessions: rows}},
		{Type: TypeListed, RequestID: "empty"},
	} {
		t.Run(message.RequestID, func(t *testing.T) { assertCatalogPages(t, message) })
	}
}

func assertCatalogPages(t *testing.T, message Control) {
	t.Helper()
	frames, err := ControlFrames(message)
	if err != nil {
		t.Fatal(err)
	}
	for i, frame := range frames {
		if len(frame.Payload) > MaxPayload {
			t.Fatalf("page %d has %d bytes", i, len(frame.Payload))
		}
		page, err := DecodeControl(frame.Payload)
		if err != nil || page.More != (i < len(frames)-1) {
			t.Fatal("page boundary or final marker is invalid", err)
		}
	}
	if message.RequestID != "empty" && len(frames) < 3 {
		t.Fatal("fixture did not cross multiple frame boundaries")
	}
	got, err := ReadControl(frameSequence(frames), func(Frame) error { return nil })
	if err != nil || !reflect.DeepEqual(got, message) {
		t.Fatal("complete catalog changed during transfer", err)
	}
}

func TestCatalogPageFrameBoundary(t *testing.T) {
	message := Control{Type: TypeListed, RequestID: "boundary", Sessions: []SessionInfo{{ID: "EDGE", Command: []string{""}}}}
	payload, err := message.Encode()
	if err != nil {
		t.Fatal(err)
	}
	message.Sessions[0].Command[0] = strings.Repeat("x", MaxPayload-len(payload))
	frames, err := ControlFrames(message)
	if err != nil || len(frames) != 1 {
		t.Fatal("single entry at the exact frame boundary rejected", err)
	}
	message.Sessions[0].Command[0] += "x"
	if _, err := ControlFrames(message); !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("oversized session returned %v", err)
	}
}

func TestIncompleteCatalogNeverReturnsPartialRows(t *testing.T) {
	first := Control{Type: TypeListed, RequestID: "list", More: true, Sessions: []SessionInfo{{ID: "ONE"}}}
	for _, last := range []Control{
		{},
		{Type: TypeListed, RequestID: "wrong", Sessions: []SessionInfo{{ID: "TWO"}}},
		{Type: TypeListed, RequestID: "list"},
		{Type: TypeError, RequestID: "list", Message: "catalog failed"},
	} {
		frames := []Frame{testControlFrame(t, first)}
		if last.Type != "" {
			frames = append(frames, testControlFrame(t, last))
		}
		got, err := ReadControl(frameSequence(frames), func(Frame) error { return nil })
		if err == nil && got.Type != TypeError || len(got.Sessions) != 0 {
			t.Fatal("incomplete or mismatched transfer published partial catalog")
		}
	}
}

func TestCatalogCreditsBindRequestPageAndSnapshot(t *testing.T) {
	catalog := Control{Type: TypeStateSnapshot, RequestID: "watch", StateSnapshot: &StateSnapshot{Seq: 42}}
	for _, credit := range []Control{
		{Type: TypeCatalogContinue, RequestID: "watch", Page: 1, Seq: 42},
		{Type: TypeCatalogContinue, RequestID: "other", Page: 1, Seq: 42},
		{Type: TypeCatalogContinue, RequestID: "watch", Page: 2, Seq: 42},
		{Type: TypeCatalogContinue, RequestID: "watch", Page: 1, Seq: 41},
		{Type: TypeHostInfo, RequestID: "watch", Page: 1, Seq: 42},
	} {
		err := AwaitCatalogContinuation(frameSequence([]Frame{testControlFrame(t, credit)}), catalog, 1)
		valid := credit.Type == TypeCatalogContinue && credit.RequestID == "watch" && credit.Page == 1 && credit.Seq == 42
		if (err == nil) != valid {
			t.Fatal("unexpected continuation credit accepted", credit, err)
		}
	}
}

func testControlFrame(t *testing.T, message Control) Frame {
	t.Helper()
	payload, err := message.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return Frame{Kind: KindControl, Payload: payload}
}

func frameSequence(frames []Frame) func() (Frame, error) {
	return func() (Frame, error) {
		if len(frames) == 0 {
			return Frame{}, io.EOF
		}
		frame := frames[0]
		frames = frames[1:]
		return frame, nil
	}
}
