package storage

import (
	"context"
	"database/sql"
	"fmt"

	dbsqlc "github.com/shaul/mesh/internal/storage/sqlc"
)

// HostChanges is one reconcile pass's durable delta. A nil Host leaves liveness
// untouched; Sessions contains only added or changed records.
type HostChanges struct {
	Host     *Host
	Sessions []Session
	Retired  []SessionID
}

// ApplyHostChanges commits a delta atomically, without rewriting other sessions.
func (s *Store) ApplyHostChanges(ctx context.Context, hostID HostID, changes HostChanges) error {
	if err := validateHostID(hostID); err != nil {
		return err
	}
	if changes.Host == nil && len(changes.Sessions) == 0 && len(changes.Retired) == 0 {
		return nil
	}
	hostValues, err := changedHostParams(hostID, changes.Host)
	if err != nil {
		return err
	}
	params, err := sessionChangeParams(hostID, changes.Sessions)
	if err != nil {
		return err
	}
	retired, err := retiredSessionIDs(changes.Retired)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("storage: reconcile host %s: begin transaction: %w", hostID, err)
	}
	defer tx.Rollback() //nolint:errcheck // commit decides the transaction outcome
	queries := s.queries.WithTx(tx)
	if changes.Host != nil {
		if _, err := queries.UpsertHost(ctx, *hostValues); err != nil {
			return fmt.Errorf("storage: reconcile host %s: upsert host: %w", hostID, err)
		}
	}
	if err := applySessionChanges(ctx, queries, hostID, params, retired); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: reconcile host %s: commit: %w", hostID, err)
	}
	return nil
}

func changedHostParams(hostID HostID, host *Host) (*dbsqlc.UpsertHostParams, error) {
	if host == nil {
		return nil, nil
	}
	if host.ID != hostID {
		return nil, fmt.Errorf("storage: reconcile host %s: host belongs to %s", hostID, host.ID)
	}
	values, err := hostParams(*host)
	if err != nil {
		return nil, err
	}
	return &values, nil
}

func sessionChangeParams(hostID HostID, sessions []Session) ([]dbsqlc.UpsertSessionParams, error) {
	params := make([]dbsqlc.UpsertSessionParams, 0, len(sessions))
	seen := make(map[SessionID]struct{}, len(sessions))
	for _, session := range sessions {
		if session.HostID != hostID {
			return nil, fmt.Errorf("storage: reconcile host %s: session %s belongs to host %s", hostID, session.ID, session.HostID)
		}
		if _, ok := seen[session.ID]; ok {
			return nil, fmt.Errorf("storage: reconcile host %s: duplicate session %s", hostID, session.ID)
		}
		seen[session.ID] = struct{}{}
		values, err := sessionParams(session)
		if err != nil {
			return nil, fmt.Errorf("storage: reconcile host %s: %w", hostID, err)
		}
		params = append(params, values)
	}
	return params, nil
}

func retiredSessionIDs(ids []SessionID) ([]string, error) {
	retired := make([]string, 0, len(ids))
	for _, id := range ids {
		if err := validateSessionID(id); err != nil {
			return nil, err
		}
		retired = append(retired, string(id))
	}
	return retired, nil
}

func applySessionChanges(ctx context.Context, queries *dbsqlc.Queries, hostID HostID, params []dbsqlc.UpsertSessionParams, retired []string) error {
	for _, values := range params {
		if _, err := queries.UpsertSession(ctx, values); err != nil {
			return fmt.Errorf("storage: reconcile host %s: upsert session %s: %w", hostID, values.ID, err)
		}
	}
	if len(retired) > 0 {
		if _, err := queries.DeleteRetiredHostSessions(ctx, dbsqlc.DeleteRetiredHostSessionsParams{HostID: string(hostID), Retired: retired}); err != nil {
			return fmt.Errorf("storage: reconcile host %s: retire sessions: %w", hostID, err)
		}
	}
	return nil
}
