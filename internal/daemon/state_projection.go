package daemon

import (
	"context"
	"reflect"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/storage"
)

// Recovery files are read outside reconcile and broker locks. Dirty identities
// coalesce in the committed catalog; a superseded read cannot replace a newer row.
func (b *stateBroker) runProjection(ctx context.Context, l *lifecycle) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.projection:
			b.projectDirty(ctx, l)
		}
	}
}
func (b *stateBroker) projectDirty(ctx context.Context, l *lifecycle) {
	for ctx.Err() == nil && b.hasTopic(protocol.TopicSessions) {
		stored, ok := b.nextProjection()
		if !ok {
			return
		}
		info := sessionInfo(stored)
		l.addRecoveryInfo(&info, true)
		if ctx.Err() != nil {
			return
		}
		b.commitProjection(stored, info)
	}
}
func (b *stateBroker) nextProjection() (storage.Session, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id := range b.dirty {
		delete(b.dirty, id)
		return cloneStoredSession(b.stored[id]), true
	}
	return storage.Session{}, false
}
func (b *stateBroker) commitProjection(stored storage.Session, info protocol.SessionInfo) {
	b.mu.Lock()
	defer b.mu.Unlock()
	current, exists := b.stored[stored.ID]
	if !exists || !sameStoredSession(current, stored) || reflect.DeepEqual(b.sessions[info.ID], info) {
		return
	}
	b.sessions[info.ID] = info
	b.publishLocked(protocol.TopicSessions, "session/"+info.ID, protocol.StateEvent{Kind: "session.changed", Payload: protocol.StatePayload{Session: &info}})
}
