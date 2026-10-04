package daemon

import "github.com/shaul/mesh/internal/protocol"

func (b *stateBroker) hostChanged(host protocol.HostInfo) {
	b.mu.Lock()
	defer b.mu.Unlock()
	previous := b.host
	if previous != nil && (host.ID != previous.ID || host.MeshIdentity != previous.MeshIdentity || host.NameRevision < previous.NameRevision) {
		return
	}
	if previous != nil && host.NameRevision == previous.NameRevision {
		if host.MachineName != previous.MachineName {
			return
		}
		b.updateObservationLocked(protocol.TopicHost, sectionObservation{at: b.now()})
		return
	}
	b.host = &host
	b.updateObservationLocked(protocol.TopicHost, sectionObservation{at: b.now()})
	b.publishLocked(protocol.TopicHost, protocol.TopicHost, protocol.StateEvent{
		Kind: "host.changed", Payload: protocol.StatePayload{Host: &host},
	})
}
