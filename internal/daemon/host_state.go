package daemon

import "github.com/shaul/mesh/internal/protocol"

func (b *stateBroker) hostChanged(host protocol.HostInfo) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.host != nil && host.NameRevision <= b.host.NameRevision {
		return
	}
	b.host = &host
	b.updateObservationLocked(protocol.TopicHost, sectionObservation{at: b.now()})
	b.publishLocked(protocol.TopicHost, protocol.TopicHost, protocol.StateEvent{
		Kind: "host.changed", Payload: protocol.StatePayload{Host: &host},
	})
}
