package daemon

import (
	"reflect"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
)

func TestWatchMemoryEventCurrentConfirmsExactSubscriberCatalogs(t *testing.T) {
	now := time.Now()
	broker := newStateBroker(1, func() time.Time { return now })
	broker.observeSessions(nil)
	broker.observeServices(nil)
	sub, initial, err := broker.subscribe(protocol.StateWatch{Topics: []string{protocol.TopicSessions, protocol.TopicServices}})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.unsubscribe(sub)
	now = now.Add(10 * time.Second)
	broker.memoryChanged(nil, now)
	messages := broker.take(sub)
	if len(messages) != 1 {
		t.Fatal(messages)
	}
	message := messages[0]
	if message.Type != protocol.TypeStateEvent || message.StateEvent == nil || message.StateEvent.Kind != "session.memory" || message.StateCurrent == nil {
		t.Fatalf("memory sample lacks event current: %+v", message)
	}
	if message.StateEvent.Seq != initial.Seq+1 || message.StateCurrent.Seq != message.StateEvent.Seq {
		t.Fatalf("event/current lost subscriber sequence: %+v", message)
	}
	expected := map[string]protocol.Observation{
		protocol.TopicSessions: {AgeMillis: 10000},
		protocol.TopicServices: {AgeMillis: 10000},
	}
	if !reflect.DeepEqual(message.StateCurrent.Sections, expected) {
		t.Fatal(message.StateCurrent.Sections)
	}
	current := broker.current(sub)
	if current.Seq != message.StateCurrent.Seq+1 || !reflect.DeepEqual(current.Sections, message.StateCurrent.Sections) {
		t.Fatalf("standalone and event current disagree: %+v %+v", current, message.StateCurrent)
	}
}
