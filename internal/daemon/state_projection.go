package daemon

import (
	"context"
	"reflect"
	"time"

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
		projection, ok := b.nextProjection()
		if !ok {
			return
		}
		info := sessionInfo(projection.stored)
		l.addRecoveryInfo(&info, true)
		if ctx.Err() != nil {
			return
		}
		b.commitProjection(projection, info)
	}
}

type sessionProjection struct {
	stored   storage.Session
	revision uint64
}

func (b *stateBroker) nextProjection() (sessionProjection, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id := range b.dirty {
		delete(b.dirty, id)
		return sessionProjection{stored: cloneStoredSession(b.stored[id]), revision: b.revisions[id]}, true
	}
	return sessionProjection{}, false
}
func (b *stateBroker) commitProjection(projection sessionProjection, info protocol.SessionInfo) {
	b.mu.Lock()
	defer b.mu.Unlock()
	stored := projection.stored
	current, exists := b.stored[stored.ID]
	if !exists || b.revisions[stored.ID] != projection.revision || !sameStoredSession(current, stored) {
		return
	}
	previous := b.sessions[info.ID]
	b.sessions[info.ID] = info
	if sameRecognition(previous, info) {
		return
	}
	b.publishLocked(protocol.TopicSessions, "session/"+info.ID, protocol.StateEvent{Kind: "session.changed", Payload: protocol.StatePayload{Session: &info}})
}

// Checkpoint time can advance without changing the recognition a viewer shows.
func sameRecognition(a, b protocol.SessionInfo) bool {
	if a.Recovery != nil {
		record := *a.Recovery
		record.CheckpointAt = time.Time{}
		a.Recovery = &record
	}
	if b.Recovery != nil {
		record := *b.Recovery
		record.CheckpointAt = time.Time{}
		b.Recovery = &record
	}
	return reflect.DeepEqual(a, b)
}
