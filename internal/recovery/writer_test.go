package recovery

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func awaitCheckpoint(t *testing.T, ack <-chan error) error {
	t.Helper()
	select {
	case err := <-ack:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("checkpoint acknowledgement timed out")
		return nil
	}
}

func TestSlowWriterReplacesPendingSnapshotsAndAcknowledgesLatest(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	saved := make(chan string, 3)
	w := newWriter(func(record Record) error {
		if record.Title == "first" {
			close(started)
			<-release
		}
		saved <- record.Title
		return nil
	}, nil)
	record := checkpointFixture()
	record.Title = "first"
	first := w.Submit(record)
	<-started
	record.Title = "stale"
	stale := w.Submit(record)
	record.Title = "latest"
	latest := w.Submit(record)
	select {
	case <-latest:
		t.Fatal("update acknowledged before disk write")
	default:
	}
	close(release)
	for _, ack := range []<-chan error{first, stale, latest} {
		if err := awaitCheckpoint(t, ack); err != nil {
			t.Fatal(err)
		}
	}
	w.Close()
	if got := <-saved; got != "first" {
		t.Fatalf("first write = %q", got)
	}
	if got := <-saved; got != "latest" {
		t.Fatalf("second write = %q", got)
	}
	if len(saved) != 0 {
		t.Fatal("stale checkpoint was written")
	}
}

func TestWriterSkipsUnchangedDataAndRetriesFailures(t *testing.T) {
	var writes atomic.Int32
	failure := errors.New("disk full")
	w := newWriter(func(Record) error {
		if writes.Add(1) == 1 {
			return failure
		}
		return nil
	}, nil)
	defer w.Close()
	record := checkpointFixture()
	if err := awaitCheckpoint(t, w.Submit(record)); !errors.Is(err, failure) {
		t.Fatalf("failure acknowledgement = %v", err)
	}
	if err := awaitCheckpoint(t, w.Submit(record)); err != nil {
		t.Fatal(err)
	}
	record.CheckpointAt = record.CheckpointAt.Add(2 * time.Second)
	if err := awaitCheckpoint(t, w.Submit(record)); err != nil {
		t.Fatal(err)
	}
	if writes.Load() != 2 {
		t.Fatalf("writes = %d, expected failure and successful retry only", writes.Load())
	}
}
