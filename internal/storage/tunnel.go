package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/shaul/mesh/internal/edge"
	"github.com/shaul/mesh/internal/tunnel"
)

// beginTunnelWrite takes SQLite's write reservation before inspecting shared
// hostname or sequence state. The empty update changes no durable rows.
func (s *Store) beginTunnelWrite(ctx context.Context) (*sql.Tx, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("storage: begin tunnel transaction: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE tunnel_highwater SET sequence = sequence WHERE 0"); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("storage: reserve tunnel write transaction: %w", err)
	}
	return tx, nil
}

func (s *Store) TunnelVersion(ctx context.Context, claimantID string) (tunnel.Ack, error) {
	var ack tunnel.Ack
	err := s.db.QueryRowContext(ctx, "SELECT sequence, digest FROM tunnel_highwater WHERE claimant_id = ?", claimantID).Scan(&ack.Sequence, &ack.Digest)
	if errors.Is(err, sql.ErrNoRows) {
		return tunnel.Ack{}, tunnel.ErrNotFound
	}
	if err != nil {
		return tunnel.Ack{}, fmt.Errorf("storage: read tunnel sequence: %w", err)
	}
	return ack, nil
}

func (s *Store) TunnelClaim(ctx context.Context, publicName string) (tunnel.Claim, error) {
	claim := tunnel.Claim{PublicName: publicName}
	err := s.db.QueryRowContext(ctx, "SELECT claimant_id FROM tunnel_claims WHERE public_name = ?", publicName).Scan(&claim.ClaimantID)
	if errors.Is(err, sql.ErrNoRows) {
		return tunnel.Claim{}, tunnel.ErrNotFound
	}
	if err != nil {
		return tunnel.Claim{}, fmt.Errorf("storage: read tunnel claim: %w", err)
	}
	return claim, nil
}

// ApplyTunnelMutation commits the reservation and one replay tombstone together.
// Authorization and active-forward checks belong to the edge's mutation gate.
func (s *Store) ApplyTunnelMutation(ctx context.Context, mutation tunnel.Mutation, digest string) error {
	verified, err := tunnel.Verify(mutation, mutation.TargetID)
	if err != nil {
		return err
	}
	if verified != digest {
		return errors.New("storage: tunnel digest does not match signed mutation")
	}
	tx, err := s.beginTunnelWrite(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // commit decides the transaction outcome
	idempotent, exists, err := inspectTunnelVersion(ctx, tx, mutation, digest)
	if err != nil || idempotent {
		return err
	}
	if err := checkTunnelMutation(ctx, tx, mutation, exists); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO tunnel_highwater (claimant_id, sequence, digest) VALUES (?, ?, ?)
        ON CONFLICT(claimant_id) DO UPDATE SET sequence = excluded.sequence, digest = excluded.digest`, mutation.ClaimantID, mutation.Sequence, digest)
	if err != nil {
		return fmt.Errorf("storage: save tunnel replay tombstone: %w", err)
	}
	if mutation.Action == tunnel.Create {
		_, err = tx.ExecContext(ctx, "INSERT INTO tunnel_claims (public_name, claimant_id) VALUES (?, ?) ON CONFLICT(public_name) DO NOTHING", mutation.PublicName, mutation.ClaimantID)
	} else {
		_, err = tx.ExecContext(ctx, "DELETE FROM tunnel_claims WHERE public_name = ? AND claimant_id = ?", mutation.PublicName, mutation.ClaimantID)
	}
	if err != nil {
		return fmt.Errorf("storage: mutate tunnel claim: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: commit tunnel mutation: %w", err)
	}
	return nil
}

func inspectTunnelVersion(ctx context.Context, tx *sql.Tx, mutation tunnel.Mutation, digest string) (idempotent, exists bool, err error) {
	var current tunnel.Ack
	err = tx.QueryRowContext(ctx, "SELECT sequence, digest FROM tunnel_highwater WHERE claimant_id = ?", mutation.ClaimantID).Scan(&current.Sequence, &current.Digest)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("storage: inspect tunnel replay tombstone: %w", err)
	}
	if mutation.Sequence < current.Sequence {
		return false, true, tunnel.ErrStaleSequence
	}
	if mutation.Sequence == current.Sequence && digest != current.Digest {
		return false, true, tunnel.ErrSequenceConflict
	}
	return mutation.Sequence == current.Sequence, true, nil
}

func checkTunnelMutation(ctx context.Context, tx *sql.Tx, mutation tunnel.Mutation, versionExists bool) error {
	var owner string
	err := tx.QueryRowContext(ctx, "SELECT claimant_id FROM tunnel_claims WHERE public_name = ?", mutation.PublicName).Scan(&owner)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("storage: inspect tunnel hostname: %w", err)
	}
	if owner != "" && owner != mutation.ClaimantID {
		return tunnel.ErrCollision
	}
	if mutation.Action == tunnel.Release && !versionExists {
		return tunnel.ErrNotFound
	}
	if mutation.Action == tunnel.Release || owner != "" {
		return nil
	}
	var routes, held, claimants, combined int
	err = tx.QueryRowContext(ctx, `SELECT
        (SELECT count(*) FROM edge_routes WHERE public_name = ?),
        (SELECT count(*) FROM tunnel_claims WHERE claimant_id = ?),
        (SELECT count(*) FROM tunnel_highwater),
        (SELECT count(*) FROM tunnel_claims) + (SELECT count(*) FROM edge_routes)`, mutation.PublicName, mutation.ClaimantID).Scan(&routes, &held, &claimants, &combined)
	if err != nil {
		return fmt.Errorf("storage: inspect tunnel capacity: %w", err)
	}
	if routes != 0 {
		return tunnel.ErrCollision
	}
	if held >= tunnel.MaximumClaimsPerKey || combined >= edge.MaximumTotalRoutes || !versionExists && claimants >= tunnel.MaximumClaimants {
		return tunnel.ErrCapacity
	}
	return nil
}

// DeleteTunnelClaim is edge-local recovery. Replay history remains untouched.
func (s *Store) DeleteTunnelClaim(ctx context.Context, publicName string) error {
	if err := tunnel.ValidateHostname(publicName); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, "DELETE FROM tunnel_claims WHERE public_name = ?", publicName)
	if err != nil {
		return fmt.Errorf("storage: recover tunnel claim: %w", err)
	}
	return nil
}

func checkEdgeTunnelCollisions(ctx context.Context, tx *sql.Tx, snapshot edge.Snapshot) error {
	var combined int
	err := tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM tunnel_claims) +
        (SELECT count(*) FROM edge_routes WHERE origin_id != ?)`, snapshot.OriginID).Scan(&combined)
	if err != nil {
		return fmt.Errorf("storage: inspect combined edge capacity: %w", err)
	}
	if combined+len(snapshot.Routes) > edge.MaximumTotalRoutes {
		return tunnel.ErrCapacity
	}
	for _, route := range snapshot.Routes {
		var exists bool
		err := tx.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM tunnel_claims WHERE public_name = ?)", route.PublicName).Scan(&exists)
		if err != nil {
			return fmt.Errorf("storage: inspect tunnel collision: %w", err)
		}
		if exists {
			return edge.ErrRouteCollision
		}
	}
	return nil
}
