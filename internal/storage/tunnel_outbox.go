package storage

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"github.com/shaul/mesh/internal/tunnel"
)

type tunnelAttempt struct {
	mutation  tunnel.Mutation
	canonical []byte
	digest    string
}

// DeliverTunnelMutation serializes one (edge, claimant) stream across processes.
// A crashed delivery leaves the signed attempt for the next process to finish.
func (s *Store) DeliverTunnelMutation(ctx context.Context, targetID string, key ed25519.PrivateKey, action tunnel.Action, publicName string, send func(context.Context, tunnel.Mutation) (tunnel.Ack, error)) (tunnel.Ack, error) {
	intent, err := tunnel.Sign(key, targetID, action, publicName, 1)
	if err != nil {
		return tunnel.Ack{}, err
	}
	if send == nil {
		return tunnel.Ack{}, errors.New("storage: nil tunnel delivery function")
	}
	lock, err := s.lockTunnelStream(ctx, targetID, intent.ClaimantID)
	if err != nil {
		return tunnel.Ack{}, err
	}
	defer lock.Close() //nolint:errcheck // closing also releases the process lock
	for {
		attempt, err := s.prepareTunnelAttempt(ctx, intent, key)
		if err != nil {
			return tunnel.Ack{}, err
		}
		ack, err := s.deliverTunnelAttempt(ctx, attempt, send)
		if err != nil {
			return tunnel.Ack{}, err
		}
		if attempt.mutation.Action != action || attempt.mutation.PublicName != publicName {
			continue
		}
		if ack.Error != "" {
			return ack, errors.New(ack.Error)
		}
		return ack, nil
	}
}

func (s *Store) prepareTunnelAttempt(ctx context.Context, intent tunnel.Mutation, key ed25519.PrivateKey) (tunnelAttempt, error) {
	tx, err := s.beginTunnelWrite(ctx)
	if err != nil {
		return tunnelAttempt{}, err
	}
	defer tx.Rollback() //nolint:errcheck // commit decides the transaction outcome
	previous, sequence, err := loadTunnelAttempt(ctx, tx, intent.TargetID, intent.ClaimantID)
	if err != nil {
		return tunnelAttempt{}, err
	}
	if previous != nil {
		return *previous, nil
	}
	if sequence == math.MaxInt64 {
		return tunnelAttempt{}, errors.New("storage: tunnel mutation sequence exhausted")
	}
	mutation, err := tunnel.Sign(key, intent.TargetID, intent.Action, intent.PublicName, sequence+1)
	if err != nil {
		return tunnelAttempt{}, err
	}
	canonical, err := tunnel.Canonical(mutation)
	if err != nil {
		return tunnelAttempt{}, err
	}
	digest, err := tunnel.Verify(mutation, intent.TargetID)
	if err != nil {
		return tunnelAttempt{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO tunnel_outbox
        (target_id, claimant_id, sequence, action, public_name, canonical, digest, signature)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(target_id, claimant_id) DO UPDATE SET sequence = excluded.sequence,
        action = excluded.action, public_name = excluded.public_name, canonical = excluded.canonical,
        digest = excluded.digest, signature = excluded.signature WHERE tunnel_outbox.canonical IS NULL`,
		mutation.TargetID, mutation.ClaimantID, mutation.Sequence, mutation.Action, mutation.PublicName, canonical, digest, mutation.Signature)
	if err != nil {
		return tunnelAttempt{}, fmt.Errorf("storage: persist signed tunnel attempt: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return tunnelAttempt{}, fmt.Errorf("storage: commit signed tunnel attempt: %w", err)
	}
	return tunnelAttempt{mutation: mutation, canonical: canonical, digest: digest}, nil
}

func loadTunnelAttempt(ctx context.Context, tx *sql.Tx, targetID, claimantID string) (*tunnelAttempt, uint64, error) {
	var sequence uint64
	var action, publicName, digest sql.NullString
	var canonical, signature []byte
	err := tx.QueryRowContext(ctx, `SELECT sequence, action, public_name, canonical, digest, signature
        FROM tunnel_outbox WHERE target_id = ? AND claimant_id = ?`, targetID, claimantID).
		Scan(&sequence, &action, &publicName, &canonical, &digest, &signature)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("storage: load tunnel attempt: %w", err)
	}
	if canonical == nil {
		return nil, sequence, nil
	}
	mutation := tunnel.Mutation{Action: tunnel.Action(action.String), TargetID: targetID, ClaimantID: claimantID, Sequence: sequence, PublicName: publicName.String, Signature: signature}
	expected, err := tunnel.Canonical(mutation)
	if err != nil {
		return nil, 0, fmt.Errorf("storage: decode tunnel attempt: %w", err)
	}
	verified, err := tunnel.Verify(mutation, targetID)
	if err != nil {
		return nil, 0, fmt.Errorf("storage: verify tunnel attempt: %w", err)
	}
	if !bytes.Equal(canonical, expected) || verified != digest.String {
		return nil, 0, errors.New("storage: stored tunnel attempt differs from its signed transcript")
	}
	return &tunnelAttempt{mutation: mutation, canonical: canonical, digest: verified}, sequence, nil
}

func (s *Store) deliverTunnelAttempt(ctx context.Context, attempt tunnelAttempt, send func(context.Context, tunnel.Mutation) (tunnel.Ack, error)) (tunnel.Ack, error) {
	ack, err := send(ctx, attempt.mutation)
	if err != nil {
		return tunnel.Ack{}, err
	}
	if ack.Sequence != attempt.mutation.Sequence || ack.Digest != attempt.digest {
		return tunnel.Ack{}, errors.New("storage: tunnel acknowledgement does not match pending sequence and digest")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE tunnel_outbox SET action = NULL, public_name = NULL,
        canonical = NULL, digest = NULL, signature = NULL
        WHERE target_id = ? AND claimant_id = ? AND sequence = ? AND digest = ?`,
		attempt.mutation.TargetID, attempt.mutation.ClaimantID, ack.Sequence, ack.Digest)
	if err != nil {
		return tunnel.Ack{}, fmt.Errorf("storage: acknowledge tunnel attempt: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return tunnel.Ack{}, fmt.Errorf("storage: inspect tunnel acknowledgement: %w", err)
	}
	if changed != 1 {
		return tunnel.Ack{}, errors.New("storage: tunnel acknowledgement no longer matches pending attempt")
	}
	return ack, nil
}

func (s *Store) lockTunnelStream(ctx context.Context, targetID, claimantID string) (*os.File, error) {
	id := sha256.Sum256([]byte(targetID + "\x00" + claimantID))
	path := filepath.Join(filepath.Dir(s.path), fmt.Sprintf(".tunnel-stream-%x.lock", id))
	file, err := openTunnelLockFile(path)
	if err != nil {
		return nil, fmt.Errorf("storage: open tunnel stream lock: %w", err)
	}
	if err := validateTunnelLock(file, path); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := acquireTunnelFileLock(ctx, file); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("storage: lock tunnel stream: %w", err)
	}
	return file, nil
}

func validateTunnelLock(file *os.File, path string) error {
	opened, err := file.Stat()
	if err != nil {
		return fmt.Errorf("storage: inspect opened tunnel lock: %w", err)
	}
	named, err := os.Lstat(path) //nolint:gosec // fixed digest basename in the local state directory, never a public hostname
	if err != nil {
		return fmt.Errorf("storage: inspect tunnel lock path: %w", err)
	}
	if !opened.Mode().IsRegular() || !named.Mode().IsRegular() || !os.SameFile(opened, named) || opened.Mode().Perm() != 0o600 {
		return errors.New("storage: tunnel stream lock must be a regular 0600 file")
	}
	return nil
}
