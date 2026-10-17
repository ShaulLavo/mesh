package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/hostmetrics"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

func TestStateCatalogKeepsProgressAcrossPageDeadlines(t *testing.T) {
	client, peer := catalogPipe(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	message := progressCatalog()
	done := make(chan struct{})
	var writeErr error
	go func() {
		defer close(done)
		writeErr = writeState(ctx, peer, message)
	}()
	t.Cleanup(func() { _ = client.Close(); _ = peer.Close(); <-done })
	started := time.Now()
	credits := 0
	write := func(frame protocol.Frame) error {
		credit := decodeServerControl(t, frame)
		credits++
		t.Logf("credit %d observed at %s; reader waits 3s", credit.Page, time.Since(started).Round(time.Millisecond))
		time.Sleep(3 * time.Second)
		if err := client.WriteFrame(frame); err != nil {
			return fmt.Errorf("delayed catalog credit: %w", err)
		}
		return nil
	}
	response, err := protocol.ReadControl(client.ReadFrame, write)
	if err != nil {
		t.Fatal("active paged reader was disconnected", err)
	}
	if credits != 2 || len(response.StateSnapshot.Sessions) != 3 {
		t.Fatal("catalog did not preserve all pages")
	}
	<-done
	if writeErr != nil {
		t.Fatal("writer failed after complete transfer", writeErr)
	}
	t.Logf("all pages complete at %s", time.Since(started).Round(time.Millisecond))
}

func TestStateCatalogCreditWaitTimesOutAndReleasesSubscriber(t *testing.T) {
	watch := startProgressWatch(t)
	started := time.Now()
	first, err := watch.client.ReadFrame()
	if err != nil || !decodeServerControl(t, first).More {
		t.Fatal("initial page unavailable", err)
	}
	awaitProgressWatch(t, watch)
	if !errors.Is(watch.err, context.DeadlineExceeded) {
		t.Fatal("missing credit did not observe the page deadline", watch.err)
	}
	assertProgressSubscriberReleased(t, watch.broker)
	t.Logf("stalled credit released subscriber after %s", time.Since(started).Round(time.Millisecond))
}

func TestStateCatalogParentCancellationReleasesSubscriber(t *testing.T) {
	watch := startProgressWatch(t)
	first, err := watch.client.ReadFrame()
	if err != nil || !decodeServerControl(t, first).More {
		t.Fatal("initial page unavailable", err)
	}
	watch.cancel()
	awaitProgressWatch(t, watch)
	if !errors.Is(watch.err, context.Canceled) {
		t.Fatal("parent cancellation did not interrupt the page", watch.err)
	}
	assertProgressSubscriberReleased(t, watch.broker)
}

type progressWatch struct {
	client transport.Conn
	broker *stateBroker
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

func startProgressWatch(t *testing.T) *progressWatch {
	t.Helper()
	client, peer := catalogPipe(t)
	broker := newStateBroker(1, time.Now)
	for _, row := range progressCatalog().StateSnapshot.Sessions {
		broker.sessions[row.ID] = row
	}
	server := &clientServer{state: broker, metrics: hostmetrics.New()}
	ctx, cancel := context.WithCancel(t.Context())
	watch := &progressWatch{client: client, broker: broker, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(watch.done)
		watch.err = server.serveState(ctx, peer, protocol.Control{Type: protocol.TypeStateWatch, RequestID: "progress", Watch: &protocol.StateWatch{Topics: []string{protocol.TopicSessions}}})
	}()
	t.Cleanup(func() { cancel(); _ = client.Close(); _ = peer.Close(); <-watch.done })
	return watch
}

func assertProgressSubscriberReleased(t *testing.T, broker *stateBroker) {
	t.Helper()
	sub, _, err := broker.subscribe(protocol.StateWatch{Topics: []string{protocol.TopicServices}})
	if err != nil {
		t.Fatal("page reader retained its subscriber", err)
	}
	broker.unsubscribe(sub)
}

func awaitProgressWatch(t *testing.T, watch *progressWatch) {
	t.Helper()
	select {
	case <-watch.done:
	case <-time.After(7 * time.Second):
		t.Fatal("page credit wait did not release its reader")
	}
}

func progressCatalog() protocol.Control {
	rows := make([]protocol.SessionInfo, 3)
	for i := range rows {
		rows[i] = protocol.SessionInfo{ID: fmt.Sprintf("P%03d", i), Command: []string{strings.Repeat("x", 3<<20)}}
	}
	return protocol.Control{Type: protocol.TypeStateSnapshot, RequestID: "progress", StateSnapshot: &protocol.StateSnapshot{Seq: 1, Sessions: rows}}
}
