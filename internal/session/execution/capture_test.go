package execution

import (
	"bytes"
	"context"
	"github.com/shaul/mesh/internal/session"
	"os"
	"testing"
)

func TestCaptureExactIndependentPages(t *testing.T) {
	ctx := context.Background()
	root, b := captureTestBinding(t)
	c, err := OpenCapture(root, b)
	if err != nil {
		t.Fatal(err)
	}
	c.Offer([]byte{'a', 0, 255, 'z'})
	summary, err := c.Finish(ctx, "eof")
	if err != nil {
		t.Fatal(err)
	}
	if !summary.EOF || summary.End != "4" {
		t.Fatalf("summary: %+v", summary)
	}
	reader, err := OpenOutput(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	first, err := reader.Page(ctx, b, "pty", "", 2)
	if err != nil {
		t.Fatal(err)
	}
	second, err := reader.Page(ctx, b, "pty", first.NextCursor, 2)
	if err != nil {
		t.Fatal(err)
	}
	again, err := reader.Page(ctx, b, "pty", "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Bytes, again.Bytes) || !bytes.Equal(append(first.Bytes, second.Bytes...), []byte{'a', 0, 255, 'z'}) {
		t.Fatal("independent raw pages changed")
	}
	if !second.EOF {
		t.Fatal("durable final EOF missing")
	}
}

func captureTestBinding(t *testing.T) (string, Binding) {
	t.Helper()
	root := t.TempDir()
	ref, err := session.NewReservation("7ABC", "boot", session.Execution)
	if err != nil {
		t.Fatal(err)
	}
	b := NewBinding(Key{Host: "host", Class: "local-account", Principal: "account", Opaque: "key"}, Intent{Kind: "pty"}, ref, nil)
	if err := os.MkdirAll(root+"/execution/attempts/"+ref.Instance, 0700); err != nil {
		t.Fatal(err)
	}
	return root, b
}
