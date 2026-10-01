package daemon

import (
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/storage"
	"testing"
	"time"
)

func TestWatchSnapshotHandoffCoalescingOwnershipAndOverflow(t *testing.T) {
	now := time.Now()
	b := newStateBroker(1, func() time.Time { return now })
	b.observeSessions(nil)
	b.sessionsChanged(SessionDiff{Added: []storage.Session{{ID: "one", Command: []string{"old"}}}})
	sub, initial, err := b.subscribe(protocol.StateWatch{Topics: []string{"sessions"}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.unsubscribe(sub)
	if len(initial.Sessions) != 1 {
		t.Fatal(initial)
	}
	initial.Sessions[0].Command[0] = "mutated"
	if _, _, err := b.subscribe(protocol.StateWatch{Topics: []string{"services"}}); err == nil {
		t.Fatal("subscriber cap ignored")
	}
	row := storage.Session{ID: "one", Command: []string{"new"}}
	b.sessionsChanged(SessionDiff{Changed: []storage.Session{row}})
	row.Command[0] = "caller mutation"
	b.sessionsChanged(SessionDiff{Changed: []storage.Session{{ID: "one", Command: []string{"latest"}}}})
	messages := b.take(sub)
	if len(messages) != 1 || messages[0].StateEvent.Payload.Session.Command[0] != "latest" {
		t.Fatal(messages)
	}
	for i := 0; i < stateQueueCapacity+1; i++ {
		b.mu.Lock()
		b.enqueueLocked(sub, string(rune(i)), protocol.StateEvent{Kind: "session.removed", Payload: protocol.StatePayload{SessionID: "one"}})
		b.mu.Unlock()
	}
	messages = b.take(sub)
	if len(messages) != 2 || messages[0].Type != protocol.TypeStateResync || messages[1].Type != protocol.TypeStateSnapshot || len(sub.pending) != 0 {
		t.Fatal(messages)
	}
}
func TestWatchFreshnessUnchangedFiveMinutesAndFailure(t *testing.T) {
	now := time.Now()
	b := newStateBroker(2, func() time.Time { return now })
	b.observeSessions(nil)
	sub, _, _ := b.subscribe(protocol.StateWatch{Topics: []string{"sessions"}})
	defer b.unsubscribe(sub)
	for range 300 {
		now = now.Add(time.Second)
		b.observeSessions(nil)
		if len(b.take(sub)) != 0 {
			t.Fatal("unchanged emitted event")
		}
	}
	current := b.current(sub)
	if current.Sections["sessions"].AgeMillis != 0 || current.Sections["sessions"].Failing {
		t.Fatal(current)
	}
	now = now.Add(time.Second)
	b.observeSessions(errWatchLimit)
	for range 5 {
		now = now.Add(10 * time.Second)
		current = b.current(sub)
		if !current.Sections["sessions"].Failing {
			t.Fatal("confirmation concealed failure")
		}
	}
}
