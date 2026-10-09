package storage

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/shaul/mesh/internal/edge"
	"github.com/shaul/mesh/internal/tunnel"
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

// ReserveAppNames shares the write reservation with services and tunnels.
// Active owner retries converge; retired names can never become active again.
func (s *Store) ReserveAppNames(ctx context.Context, publicNames []string, ownerID string) error {
	if err := validateAppNames(publicNames, ownerID); err != nil {
		return err
	}
	tx, err := s.beginTunnelWrite(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // commit decides the transaction outcome
	for _, name := range publicNames {
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
func (s *Store) ReserveAppNamesAndState(ctx context.Context, publicNames []string, ownerID, key string, data []byte) error {
	if err := validateAppNames(publicNames, ownerID); err != nil {
		return err
	}
	if err := validateAppState(key, data); err != nil {
		return err
	}
	tx, err := s.beginTunnelWrite(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // commit decides the transaction outcome
	for _, publicName := range publicNames {
		if err := reserveAppName(ctx, tx, publicName, ownerID); err != nil {
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

func reserveAppName(ctx context.Context, tx *sql.Tx, publicName, ownerID string) error {
	var currentOwner string
	var active bool
	err := tx.QueryRowContext(ctx, "SELECT owner_id, active FROM app_names WHERE public_name = ?", publicName).Scan(&currentOwner, &active)
	if err == nil {
		return existingAppName(currentOwner, ownerID, active)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("storage: inspect app name: %w", err)
	}
	if err := checkNewAppName(ctx, tx, publicName); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO app_names (public_name, owner_id, active) VALUES (?, ?, 1)", publicName, ownerID); err != nil {
		return fmt.Errorf("storage: reserve app name: %w", err)
	}
	return nil
}

func existingAppName(currentOwner, ownerID string, active bool) error {
	if currentOwner != ownerID || !active {
		return tunnel.ErrCollision
	}
	return nil
}

func checkNewAppName(ctx context.Context, tx *sql.Tx, publicName string) error {
	var collision bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM edge_routes WHERE public_name = ?)
        OR EXISTS (SELECT 1 FROM tunnel_claims WHERE public_name = ?)`, publicName, publicName).Scan(&collision)
	if err != nil {
		return fmt.Errorf("storage: inspect app name collision: %w", err)
	}
	if collision {
		return tunnel.ErrCollision
	}
	var combined int
	err = tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM app_names WHERE active = 1) +
        (SELECT count(*) FROM tunnel_claims) + (SELECT count(*) FROM edge_routes)`).Scan(&combined)
	if err != nil {
		return fmt.Errorf("storage: inspect app capacity: %w", err)
	}
	if combined >= edge.MaximumTotalRoutes {
		return tunnel.ErrCapacity
	}
	return nil
}

func (s *Store) AppNameExists(ctx context.Context, publicName string) (bool, error) {
	if err := tunnel.ValidateHostname(publicName); err != nil {
		return false, err
	}
	var exists bool
	err := s.db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM app_names WHERE public_name = ?)", publicName).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("storage: inspect app name: %w", err)
	}
	return exists, nil
}

func (s *Store) SetAppNameInactive(ctx context.Context, publicName, ownerID string) error {
	if err := validateAppNameOwner(publicName, ownerID); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, "UPDATE app_names SET active = 0 WHERE public_name = ? AND owner_id = ?", publicName, ownerID)
	if err != nil {
		return fmt.Errorf("storage: retire app name: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("storage: inspect retired app name: %w", err)
	}
	if changed == 0 {
		return tunnel.ErrCollision
	}
	return nil
}

func validateAppNameOwner(publicName, ownerID string) error {
	if err := tunnel.ValidateHostname(publicName); err != nil {
		return err
	}
	decoded, err := base64.RawURLEncoding.DecodeString(ownerID)
	if err != nil || len(decoded) != 32 || base64.RawURLEncoding.EncodeToString(decoded) != ownerID {
		return errors.New("storage: app owner must be a canonical Ed25519 identity")
	}
	return nil
}
