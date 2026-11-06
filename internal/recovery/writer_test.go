package recovery

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/shaul/mesh/internal/agentresume"
)

func TestWriterRewritesRevertedContentAfterVisibleFailure(t *testing.T) {
	dir := t.TempDir()
	writes := 0
	w := newWriter(func(record Record) error {
		writes++
		if err := Write(dir, record); err != nil {
			return err
		}
		if record.Title == "B" {
			return fmt.Errorf("injected post-publication error: %w", syscall.EIO)
		}
		return nil
	}, nil)
	defer w.Close()
	record := checkpointFixture()
	record.Title = "A"
	if err := awaitCheckpoint(t, w.Submit(record, Key{})); err != nil {
		t.Fatal(err)
	}
	record.Title = "B"
	if err := awaitCheckpoint(t, w.Submit(record, Key{})); !errors.Is(err, syscall.EIO) {
		t.Fatalf("B acknowledgement = %v, want EIO", err)
	}
	saved, err := Read(dir)
	if err != nil || saved.Title != "B" {
		t.Fatalf("visible failed publication = %q, %v", saved.Title, err)
	}
	record.Title = "A"
	if err := awaitCheckpoint(t, w.Submit(record, Key{})); err != nil {
		t.Fatal(err)
	}
	saved, err = Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Title != "A" || writes != 3 {
		t.Fatalf("saved %q after %d writes, want A after 3", saved.Title, writes)
	}
}

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
	firstGate, latestGate := newCheckpointGate(), newCheckpointGate()
	saved := make(chan string, 3)
	w := newWriter(func(record Record) error {
		switch record.Title {
		case "first":
			firstGate.block()
		case "latest":
			latestGate.block()
		}
		saved <- record.Title
		return nil
	}, nil)
	cleanupCheckpointWriter(t, w, firstGate, latestGate)
	record := checkpointFixture()
	record.Title = "first"
	firstKey := CheckpointKey(record, 1)
	first := w.Submit(record, firstKey)
	awaitCheckpointGate(t, firstGate.started)
	record.Title = "stale"
	staleKey := CheckpointKey(record, 2)
	stale := w.Submit(record, staleKey)
	record.Title = "latest"
	latestKey := CheckpointKey(record, 3)
	latest := w.Submit(record, latestKey)
	assertCheckpointPending(t, latest)
	firstGate.open()
	if err := awaitCheckpoint(t, first); err != nil {
		t.Fatal(err)
	}
	awaitCheckpointGate(t, latestGate.started)
	assertCheckpointCoverage(t, w, false, firstKey, staleKey, latestKey, Key{})
	assertCheckpointPending(t, stale)
	assertCheckpointPending(t, latest)
	latestGate.open()
	for _, ack := range []<-chan error{stale, latest} {
		if err := awaitCheckpoint(t, ack); err != nil {
			t.Fatal(err)
		}
	}
	assertCheckpointCoverage(t, w, true, latestKey)
	assertCheckpointCoverage(t, w, false, firstKey, staleKey, Key{})
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
	if err := awaitCheckpoint(t, w.Submit(record, Key{})); !errors.Is(err, failure) {
		t.Fatalf("failure acknowledgement = %v", err)
	}
	if err := awaitCheckpoint(t, w.Submit(record, Key{})); err != nil {
		t.Fatal(err)
	}
	record.CheckpointAt = record.CheckpointAt.Add(2 * time.Second)
	if err := awaitCheckpoint(t, w.Submit(record, Key{})); err != nil {
		t.Fatal(err)
	}
	if writes.Load() != 2 {
		t.Fatalf("writes = %d, expected failure and successful retry only", writes.Load())
	}
}

func TestWriterCoversOnlyTheNewestDurableSubmission(t *testing.T) {
	firstGate, secondGate := newCheckpointGate(), newCheckpointGate()
	failure := errors.New("disk full")
	var fail atomic.Bool
	fail.Store(true)
	w := newWriter(func(record Record) error {
		if record.Title == "third" && fail.Load() {
			return failure
		}
		switch record.Title {
		case "first":
			firstGate.block()
		case "second":
			secondGate.block()
		}
		return nil
	}, nil)
	cleanupCheckpointWriter(t, w, firstGate, secondGate)
	record := checkpointFixture()
	record.Title = "first"
	firstKey := CheckpointKey(record, 1)
	assertCheckpointCoverage(t, w, false, firstKey, Key{})
	first := w.Submit(record, firstKey)
	awaitCheckpointGate(t, firstGate.started)
	assertCheckpointCoverage(t, w, false, firstKey, Key{})
	record.Title = "second"
	secondKey := CheckpointKey(record, 2)
	second := w.Submit(record, secondKey)
	assertCheckpointCoverage(t, w, false, firstKey, secondKey, Key{})
	firstGate.open()
	if err := awaitCheckpoint(t, first); err != nil {
		t.Fatal(err)
	}
	awaitCheckpointGate(t, secondGate.started)
	assertCheckpointPending(t, second)
	assertCheckpointCoverage(t, w, false, firstKey, secondKey, Key{})
	secondGate.open()
	if err := awaitCheckpoint(t, second); err != nil {
		t.Fatal(err)
	}
	assertCheckpointCoverage(t, w, true, secondKey)
	assertCheckpointCoverage(t, w, false, firstKey, Key{})
	record.Title = "third"
	thirdKey := CheckpointKey(record, 3)
	if err := awaitCheckpoint(t, w.Submit(record, thirdKey)); !errors.Is(err, failure) {
		t.Fatalf("failed acknowledgement = %v, want %v", err, failure)
	}
	assertCheckpointCoverage(t, w, false, secondKey, thirdKey, Key{})
	fail.Store(false)
	if err := awaitCheckpoint(t, w.Submit(record, thirdKey)); err != nil {
		t.Fatal(err)
	}
	assertCheckpointCoverage(t, w, true, thirdKey)
	w.Close()
	assertCheckpointCoverage(t, w, true, thirdKey)
	fourthKey := CheckpointKey(record, 4)
	if err := awaitCheckpoint(t, w.Submit(record, fourthKey)); err == nil {
		t.Fatal("closed writer accepted a submission")
	}
	assertCheckpointCoverage(t, w, false, thirdKey, fourthKey, Key{})
}

func TestWriterCoversDeduplicatedSubmissionBeforeAcknowledgement(t *testing.T) {
	var writes atomic.Int32
	w := newWriter(func(Record) error {
		writes.Add(1)
		return nil
	}, nil)
	t.Cleanup(w.Close)
	record := checkpointFixture()
	firstKey := CheckpointKey(record, 1)
	if err := awaitCheckpoint(t, w.Submit(record, firstKey)); err != nil {
		t.Fatal(err)
	}
	assertCheckpointCoverage(t, w, true, firstKey)
	record.CheckpointAt = record.CheckpointAt.Add(time.Second)
	secondKey := CheckpointKey(record, 2)
	for range 2 {
		if err := awaitCheckpoint(t, w.Submit(record, secondKey)); err != nil {
			t.Fatal(err)
		}
		assertCheckpointCoverage(t, w, true, secondKey)
	}
	assertCheckpointCoverage(t, w, false, firstKey, Key{})
	if writes.Load() != 1 {
		t.Fatalf("deduplicated submissions wrote %d times, want 1", writes.Load())
	}
	if err := awaitCheckpoint(t, w.Submit(record, Key{})); err != nil {
		t.Fatal(err)
	}
	assertCheckpointCoverage(t, w, false, firstKey, secondKey, Key{})
}

func TestWriterOverflowRejectionInvalidatesCoverageUntilRetry(t *testing.T) {
	gate := newCheckpointGate()
	var writes atomic.Int32
	w := newWriter(func(Record) error {
		if writes.Add(1) == 1 {
			gate.block()
		}
		return nil
	}, nil)
	cleanupCheckpointWriter(t, w, gate)
	record := checkpointFixture()
	firstKey := CheckpointKey(record, 1)
	first := w.Submit(record, firstKey)
	awaitCheckpointGate(t, gate.started)
	record.Title = "pending"
	pendingKey := CheckpointKey(record, 2)
	waiters := make([]<-chan error, 256)
	for i := range waiters {
		waiters[i] = w.Submit(record, pendingKey)
	}
	record.Title = "rejected"
	rejectedKey := CheckpointKey(record, 3)
	if err := awaitCheckpoint(t, w.Submit(record, rejectedKey)); err == nil {
		t.Fatal("full writer accepted a submission")
	}
	assertCheckpointCoverage(t, w, false, firstKey, pendingKey, rejectedKey, Key{})
	gate.open()
	if err := awaitCheckpoint(t, first); err != nil {
		t.Fatal(err)
	}
	for _, ack := range waiters {
		if err := awaitCheckpoint(t, ack); err != nil {
			t.Fatal(err)
		}
	}
	assertCheckpointCoverage(t, w, false, firstKey, pendingKey, rejectedKey, Key{})
	if writes.Load() != 2 {
		t.Fatalf("drain wrote %d times, want only 2 accepted batches", writes.Load())
	}
	if err := awaitCheckpoint(t, w.Submit(record, rejectedKey)); err != nil {
		t.Fatal(err)
	}
	assertCheckpointCoverage(t, w, true, rejectedKey)
	if writes.Load() != 3 {
		t.Fatalf("retry wrote %d times, want 3", writes.Load())
	}
}

func TestWriterCloseDrainsAcceptedBatches(t *testing.T) {
	synctest.Test(t, testWriterCloseDrainsAcceptedBatches)
}

func testWriterCloseDrainsAcceptedBatches(t *testing.T) {
	firstGate, lastGate := newCheckpointGate(), newCheckpointGate()
	written := make(chan string, 2)
	w := newWriter(func(record Record) error {
		if record.Title == "first" {
			firstGate.block()
		} else {
			lastGate.block()
		}
		written <- record.Title
		return nil
	}, nil)
	cleanupCheckpointWriter(t, w, firstGate, lastGate)
	record := checkpointFixture()
	record.Title = "first"
	first := w.Submit(record, CheckpointKey(record, 1))
	awaitCheckpointGate(t, firstGate.started)
	record.Title = "last"
	lastKey := CheckpointKey(record, 2)
	last := w.Submit(record, lastKey)
	closed := make(chan struct{})
	go func() {
		w.Close()
		close(closed)
	}()
	synctest.Wait()
	assertCheckpointPending(t, first)
	assertCheckpointNotClosed(t, closed)
	firstGate.open()
	if err := awaitCheckpoint(t, first); err != nil {
		t.Fatal(err)
	}
	awaitCheckpointGate(t, lastGate.started)
	synctest.Wait()
	assertCheckpointPending(t, last)
	assertCheckpointNotClosed(t, closed)
	lastGate.open()
	awaitCheckpointGate(t, closed)
	if err := awaitCheckpoint(t, last); err != nil {
		t.Fatal(err)
	}
	assertCheckpointCoverage(t, w, true, lastKey)
	if first, last := <-written, <-written; first != "first" || last != "last" {
		t.Fatalf("drained records = %q, %q", first, last)
	}
}

func TestWriterEncodingFailureInvalidatesCoverageAndDigest(t *testing.T) {
	var writes, reports atomic.Int32
	w := newWriter(func(Record) error {
		writes.Add(1)
		return nil
	}, func(error) { reports.Add(1) })
	t.Cleanup(w.Close)
	record := checkpointFixture()
	key := CheckpointKey(record, 1)
	if err := awaitCheckpoint(t, w.Submit(record, key)); err != nil {
		t.Fatal(err)
	}
	invalid := record
	invalid.LastOutputAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := awaitCheckpoint(t, w.Submit(invalid, Key{})); err == nil {
		t.Fatal("invalid time was acknowledged")
	}
	assertCheckpointCoverage(t, w, false, key, Key{})
	if err := awaitCheckpoint(t, w.Submit(record, key)); err != nil {
		t.Fatal(err)
	}
	assertCheckpointCoverage(t, w, true, key)
	if writes.Load() != 2 || reports.Load() != 1 {
		t.Fatalf("writes = %d, reports = %d, want 2 and 1", writes.Load(), reports.Load())
	}
}

type checkpointGate struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newCheckpointGate() *checkpointGate {
	return &checkpointGate{started: make(chan struct{}), release: make(chan struct{})}
}

func (g *checkpointGate) block() {
	close(g.started)
	<-g.release
}

func (g *checkpointGate) open() {
	g.once.Do(func() { close(g.release) })
}

func cleanupCheckpointWriter(t *testing.T, w *Writer, gates ...*checkpointGate) {
	t.Helper()
	t.Cleanup(func() {
		for _, gate := range gates {
			gate.open()
		}
		w.Close()
	})
}

func awaitCheckpointGate(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal("checkpoint gate timed out")
	}
}

func assertCheckpointCoverage(t *testing.T, w *Writer, covered bool, keys ...Key) {
	t.Helper()
	for _, key := range keys {
		if got := w.Covers(key); got != covered {
			t.Fatalf("Covers(%x) = %v, want %v", key, got, covered)
		}
	}
}

func assertCheckpointPending(t *testing.T, ack <-chan error) {
	t.Helper()
	select {
	case err := <-ack:
		t.Fatalf("checkpoint acknowledged before release: %v", err)
	default:
	}
}

func assertCheckpointNotClosed(t *testing.T, closed <-chan struct{}) {
	t.Helper()
	select {
	case <-closed:
		t.Fatal("Close returned before accepted writes finished")
	default:
	}
}

type checkpointKeyCase struct {
	name     string
	change   func(*Record)
	revision uint64
	equal    bool
}

func TestCheckpointKeyCoversInputsExceptCheckpointTimeAndLines(t *testing.T) {
	cases := []checkpointKeyCase{
		{name: "shell", change: func(r *Record) { r.Shell = "/bin/zsh" }},
		{name: "directory", change: func(r *Record) { r.ShellDirectory = "/elsewhere" }},
		{name: "directory source", change: func(r *Record) { r.DirectorySource = DirectoryObserved }},
		{name: "title", change: func(r *Record) { r.Title = "another title" }},
		{name: "last output", change: func(r *Record) { r.LastOutputAt = r.LastOutputAt.Add(time.Second) }},
		{name: "command", change: func(r *Record) { r.Command[0] = "/bin/zsh" }},
		{name: "restart argv", change: func(r *Record) { r.Restart.Argv[0] = "/bin/echo" }},
		{name: "restart cwd", change: func(r *Record) { r.Restart.Cwd = "/elsewhere" }},
		{name: "agent lifecycle", change: func(r *Record) { r.Agent.Lifecycle = agentresume.Closed }},
		{name: "agent options", change: func(r *Record) { r.Agent.Options[0] = "--different" }},
		{name: "agent registered time", change: func(r *Record) { r.Agent.RegisteredAt = r.Agent.RegisteredAt.Add(time.Second) }},
		{name: "agent receipt", change: func(r *Record) { r.AgentResume.ConversationID = "another-conversation" }},
		{name: "remote session", change: func(r *Record) { r.Remote.SessionID = "8K3D" }},
		{name: "version", change: func(r *Record) { r.Version++ }},
		{name: "owner host", change: func(r *Record) { r.HostID = "another-host" }},
		{name: "owner session", change: func(r *Record) { r.SessionID = "8K3D" }},
		{name: "revision", revision: 2},
		{name: "checkpoint time", change: func(r *Record) { r.CheckpointAt = r.CheckpointAt.Add(time.Second) }, equal: true},
		{name: "lines", change: func(r *Record) { r.Lines[0] = "prepared text" }, equal: true},
		{name: "equal pointer contents", equal: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) { checkCheckpointKeyCase(t, test) })
	}
}

func checkCheckpointKeyCase(t *testing.T, test checkpointKeyCase) {
	t.Helper()
	base := checkpointKeyFixture()
	key := CheckpointKey(base, 1)
	if key == (Key{}) {
		t.Fatal("valid metadata produced a zero key")
	}
	changed := cloneCheckpointKeyFixture(base)
	if test.change != nil {
		test.change(&changed)
	}
	revision := uint64(1)
	if test.revision != 0 {
		revision = test.revision
	}
	if got := CheckpointKey(changed, revision); (got == key) != test.equal {
		t.Fatalf("key equal = %v, want %v", got == key, test.equal)
	}
	if CheckpointKey(base, 1) != key {
		t.Fatal("key computation or fixture mutation changed the original inputs")
	}
}

func TestCheckpointKeyReturnsZeroOnEncodingFailure(t *testing.T) {
	record := checkpointKeyFixture()
	record.LastOutputAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	if key := CheckpointKey(record, 1); key != (Key{}) {
		t.Fatalf("encoding failure key = %x, want zero", key)
	}
}

func checkpointKeyFixture() Record {
	record := checkpointFixture()
	record.Title = "inherited title"
	record.LastOutputAt = record.CheckpointAt
	record.Restart = &Command{Argv: []string{"/bin/bash", "-l"}, Cwd: "/project"}
	record.Remote = &Target{HostID: "remote-host", SessionID: "7K3D"}
	record.Agent = &agentresume.Recipe{
		Version: agentresume.Version,
		Launch: agentresume.Launch{Provider: agentresume.Codex, Executable: "/bin/codex", ProviderVersion: "test",
			Directory: "/project", DataRoot: "/agent", Options: []string{"--model", "test"}},
		ConversationID: "conversation", InvocationToken: "invocation", RegisteredAt: record.CheckpointAt,
		Lifecycle: agentresume.Active,
	}
	record.AgentResume = &agentresume.Receipt{SourceSessionID: "7K3D", Provider: agentresume.Codex,
		ConversationID: "conversation", InvocationToken: "invocation", VerifiedAt: record.CheckpointAt}
	return record
}

func cloneCheckpointKeyFixture(record Record) Record {
	record.Command = slices.Clone(record.Command)
	record.Lines = slices.Clone(record.Lines)
	restart := *record.Restart
	restart.Argv = slices.Clone(restart.Argv)
	record.Restart = &restart
	remote := *record.Remote
	record.Remote = &remote
	agent := *record.Agent
	agent.Options = slices.Clone(agent.Options)
	record.Agent = &agent
	receipt := *record.AgentResume
	record.AgentResume = &receipt
	return record
}

func TestWriterCoalescedFailureAcknowledgesReplacedWaiters(t *testing.T) {
	firstGate, latestGate := newCheckpointGate(), newCheckpointGate()
	failure := errors.New("latest write failed")
	var fail atomic.Bool
	fail.Store(true)
	var writes, reports atomic.Int32
	w := newWriter(func(record Record) error {
		writes.Add(1)
		if record.Title == "first" {
			firstGate.block()
		}
		if record.Title == "latest" && fail.Load() {
			latestGate.block()
			return failure
		}
		return nil
	}, func(error) { reports.Add(1) })
	cleanupCheckpointWriter(t, w, firstGate, latestGate)
	record := checkpointFixture()
	record.Title = "first"
	first := w.Submit(record, CheckpointKey(record, 1))
	awaitCheckpointGate(t, firstGate.started)
	record.Title = "stale"
	staleKey := CheckpointKey(record, 2)
	stale := w.Submit(record, staleKey)
	record.Title = "latest"
	latestKey := CheckpointKey(record, 3)
	latest := w.Submit(record, latestKey)
	firstGate.open()
	if err := awaitCheckpoint(t, first); err != nil {
		t.Fatal(err)
	}
	awaitCheckpointGate(t, latestGate.started)
	assertCheckpointPending(t, stale)
	assertCheckpointPending(t, latest)
	latestGate.open()
	for _, ack := range []<-chan error{stale, latest} {
		if err := awaitCheckpoint(t, ack); !errors.Is(err, failure) {
			t.Fatalf("coalesced acknowledgement = %v, want %v", err, failure)
		}
	}
	assertCheckpointCoverage(t, w, false, staleKey, latestKey, Key{})
	if writes.Load() != 2 || reports.Load() != 1 {
		t.Fatalf("writes = %d, reports = %d, want 2 and 1", writes.Load(), reports.Load())
	}
	fail.Store(false)
	if err := awaitCheckpoint(t, w.Submit(record, latestKey)); err != nil {
		t.Fatal(err)
	}
	assertCheckpointCoverage(t, w, true, latestKey)
}
