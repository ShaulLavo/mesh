package storage

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/shaul/mesh/internal/serve"
)

const maximumAppStateBytes = 16 << 20
const saveAppStateSQL = `INSERT INTO private_app_state (key, data) VALUES (?, ?)
        ON CONFLICT(key) DO UPDATE SET data = excluded.data`

// LoadAppState returns nil for an absent role state. App domain code owns the JSON schema.
func (s *Store) LoadAppState(ctx context.Context, key string) ([]byte, error) {
	if err := validateAppStateKey(key); err != nil {
		return nil, err
	}
	var data []byte
	err := s.db.QueryRowContext(ctx, "SELECT data FROM private_app_state WHERE key = ?", key).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("storage: load app state: %w", err)
	}
	return data, nil
}

func (s *Store) SaveAppState(ctx context.Context, key string, data []byte) error {
	if err := validateAppState(key, data); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, saveAppStateSQL, key, data)
	if err != nil {
		return fmt.Errorf("storage: save app state: %w", err)
	}
	return nil
}

func validateAppState(key string, data []byte) error {
	if err := validateAppStateKey(key); err != nil {
		return err
	}
	if len(data) == 0 || len(data) > maximumAppStateBytes || !json.Valid(data) {
		return errors.New("storage: app state must be valid JSON within 16 MiB")
	}
	return nil
}

func validateAppStateKey(key string) error {
	if len(key) < 1 || len(key) > 128 {
		return errors.New("storage: app state key must contain 1..128 bytes")
	}
	return nil
}

// ReserveAppNames takes a write reservation before inspecting app ownership.
// Active owner retries converge; retired names can never become active again.
func (s *Store) ReserveAppNames(ctx context.Context, hostnames []string, ownerID string) error {
	if err := validateAppNames(hostnames, ownerID); err != nil {
		return err
	}
	tx, err := s.beginAppWrite(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // commit decides the transaction outcome
	for _, name := range hostnames {
		if err := reserveAppName(ctx, tx, name, ownerID); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: commit app names: %w", err)
	}
	return nil
}

func validateAppNames(names []string, owner string) error {
	if len(names) == 0 || len(names) > 8 {
		return errors.New("storage: app name count is outside 1..8")
	}
	for _, name := range names {
		if err := validateAppNameOwner(name, owner); err != nil {
			return err
		}
	}
	return nil
}

// ReserveAppNamesAndState commits every deployment alias and app state together.
func (s *Store) ReserveAppNamesAndState(ctx context.Context, hostnames []string, ownerID, key string, data []byte) error {
	if err := validateAppNames(hostnames, ownerID); err != nil {
		return err
	}
	if err := validateAppState(key, data); err != nil {
		return err
	}
	tx, err := s.beginAppWrite(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // commit decides the transaction outcome
	for _, hostname := range hostnames {
		if err := reserveAppName(ctx, tx, hostname, ownerID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, saveAppStateSQL, key, data); err != nil {
		return fmt.Errorf("storage: save allocated app state: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: commit allocated app: %w", err)
	}
	return nil
}

func reserveAppName(ctx context.Context, tx *sql.Tx, hostname, ownerID string) error {
	var currentOwner string
	var active bool
	err := tx.QueryRowContext(ctx, "SELECT owner_id, active FROM app_names WHERE hostname = ?", hostname).Scan(&currentOwner, &active)
	if err == nil {
		return existingAppName(currentOwner, ownerID, active)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("storage: inspect app name: %w", err)
	}
	if err := checkAppNameCapacity(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO app_names (hostname, owner_id, active) VALUES (?, ?, 1)", hostname, ownerID); err != nil {
		return fmt.Errorf("storage: reserve app name: %w", err)
	}
	return nil
}

func existingAppName(currentOwner, ownerID string, active bool) error {
	if currentOwner != ownerID || !active {
		return ErrAppNameCollision
	}
	return nil
}

func checkAppNameCapacity(ctx context.Context, tx *sql.Tx) error {
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM app_names WHERE active = 1").Scan(&count); err != nil {
		return fmt.Errorf("storage: inspect app capacity: %w", err)
	}
	if count >= 8192 {
		return ErrAppNameCapacity
	}
	return nil
}

func (s *Store) AppNameExists(ctx context.Context, hostname string) (bool, error) {
	if err := serve.ValidateDeploymentHost(hostname); err != nil {
		return false, fmt.Errorf("storage: validate app hostname: %w", err)
	}
	var exists bool
	err := s.db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM app_names WHERE hostname = ?)", hostname).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("storage: inspect app name: %w", err)
	}
	return exists, nil
}

func (s *Store) SetAppNameInactive(ctx context.Context, hostname, ownerID string) error {
	if err := validateAppNameOwner(hostname, ownerID); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, "UPDATE app_names SET active = 0 WHERE hostname = ? AND owner_id = ?", hostname, ownerID)
	if err != nil {
		return fmt.Errorf("storage: retire app name: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("storage: inspect retired app name: %w", err)
	}
	if changed == 0 {
		return ErrAppNameCollision
	}
	return nil
}

func validateAppNameOwner(hostname, ownerID string) error {
	if err := serve.ValidateDeploymentHost(hostname); err != nil {
		return fmt.Errorf("storage: validate app hostname: %w", err)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(ownerID)
	if err != nil || len(decoded) != 32 || base64.RawURLEncoding.EncodeToString(decoded) != ownerID {
		return errors.New("storage: app owner must be a canonical Ed25519 identity")
	}
	return nil
}

var ErrAppNameCollision = errors.New("storage: app hostname is already reserved")
var ErrAppNameCapacity = errors.New("storage: app hostname capacity reached")

// Acquire the write reservation before reading ownership so concurrent creators have one winner.
func (s *Store) beginAppWrite(ctx context.Context) (*sql.Tx, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("storage: begin app transaction: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE app_names SET active = active WHERE 0"); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("storage: reserve app write transaction: %w", err)
	}
	return tx, nil
}
