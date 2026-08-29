package protocol

import (
	"bytes"
	"io"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	id, err := NewSessionID("7K3D")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w := NewWriter(&buf)

	if err := w.WriteControlMsg(Control{Type: TypeAttach, SessionID: "7K3D", Cols: 140, Rows: 42}); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteData(id, 4096, []byte("hello\x1b[0m")); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteSnapshot(id, []byte("\x1b[2Jpaint")); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteInput(id, []byte{0x03}); err != nil {
		t.Fatal(err)
	}

	r := NewReader(&buf)

	f, err := r.ReadFrame()
	if err != nil || f.Kind != KindControl {
		t.Fatalf("control frame: %v %v", f.Kind, err)
	}
	c, err := DecodeControl(f.Payload)
	if err != nil || c.Type != TypeAttach || c.Cols != 140 || c.Rows != 42 {
		t.Fatalf("decoded control = %+v, %v", c, err)
	}

	f, err = r.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != KindData || f.Seq != 4096 || f.Session.String() != "7K3D" || string(f.Payload) != "hello\x1b[0m" {
		t.Fatalf("data frame = %+v", f)
	}

	f, err = r.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != KindSnapshot || f.Session.String() != "7K3D" || string(f.Payload) != "\x1b[2Jpaint" {
		t.Fatalf("snapshot frame = %+v", f)
	}

	f, err = r.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != KindInput || f.Session.String() != "7K3D" || !bytes.Equal(f.Payload, []byte{0x03}) {
		t.Fatalf("input frame = %+v", f)
	}

	if _, err := r.ReadFrame(); err != io.EOF {
		t.Fatalf("trailing read = %v, want EOF", err)
	}
}

func TestReaderRejectsOversizedAndUnknownFrames(t *testing.T) {
	// Length header claiming more than MaxPayload must be refused before any
	// allocation happens.
	oversized := []byte{byte(KindData), 0xff, 0xff, 0xff, 0xff}
	if _, err := NewReader(bytes.NewReader(oversized)).ReadFrame(); err != ErrPayloadTooLarge {
		t.Fatalf("oversized frame err = %v, want ErrPayloadTooLarge", err)
	}

	unknown := []byte{0x7f, 0, 0, 0, 0}
	if _, err := NewReader(bytes.NewReader(unknown)).ReadFrame(); err == nil {
		t.Fatal("unknown frame kind accepted")
	}

	short := []byte{byte(KindData), 0, 0, 0, 2, 'a', 'b'}
	if _, err := NewReader(bytes.NewReader(short)).ReadFrame(); err == nil {
		t.Fatal("truncated data frame accepted")
	}

	short = []byte{byte(KindSnapshot), 0, 0, 0, 2, 'a', 'b'}
	if _, err := NewReader(bytes.NewReader(short)).ReadFrame(); err == nil {
		t.Fatal("truncated snapshot frame accepted")
	}
}

func TestSessionIDRejectsOverlongNames(t *testing.T) {
	if _, err := NewSessionID("123456789"); err == nil {
		t.Fatal("9 byte session id accepted; ids must never silently truncate")
	}
	if _, err := NewSessionID(""); err == nil {
		t.Fatal("empty session id accepted")
	}
}
