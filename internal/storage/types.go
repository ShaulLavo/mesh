package storage

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	meshserve "github.com/shaul/mesh/internal/serve"
	dbsqlc "github.com/shaul/mesh/internal/storage/sqlc"
)

// HostID is the stable identity of a Mesh host.
type HostID string

// SessionID identifies a session within one host.
type SessionID string

// SessionState is the last state observed by the daemon.
type SessionState string

const (
	StateRunning     SessionState = "running"
	StateDetached    SessionState = "detached"
	StateExited      SessionState = "exited"
	StateInterrupted SessionState = "interrupted"
)

// Host is durable metadata for one Mesh host.
type Host struct {
	ID            HostID
	Alias         *string
	MeshIdentity  string
	TailscaleName *string
	LastSeenAt    time.Time
}

// Session is durable metadata for one terminal session.
type Session struct {
	ID                 SessionID
	HostID             HostID
	Command            []string
	Cwd                string
	State              SessionState
	CreatedAt          time.Time
	LastAttachedAt     *time.Time
	ExitCode           *int
	LastOutputSequence uint64
}

// CachedService is the last service status observed from one adopted host.
// It is advisory state for offline CLI listing, never daemon routing state.
type CachedService struct {
	HostID      HostID
	PrivateName string
	Service     meshserve.Service
	Healthy     bool
	Problem     string
	ObservedAt  time.Time
}

func validateHostID(id HostID) error {
	if strings.TrimSpace(string(id)) == "" {
		return fmt.Errorf("storage: empty host ID")
	}
	return nil
}

func validateSessionID(id SessionID) error {
	if strings.TrimSpace(string(id)) == "" {
		return fmt.Errorf("storage: empty session ID")
	}
	return nil
}

func validateState(state SessionState) error {
	switch state {
	case StateRunning, StateDetached, StateExited, StateInterrupted:
		return nil
	default:
		return fmt.Errorf("storage: unsupported session state %q", state)
	}
}

func validateExit(state SessionState, exitCode *int) error {
	switch {
	case state == StateExited && exitCode == nil:
		return fmt.Errorf("storage: exited session has no exit code")
	case state != StateExited && exitCode != nil:
		return fmt.Errorf("storage: %s session has an exit code", state)
	default:
		return nil
	}
}

func hostParams(h Host) (dbsqlc.UpsertHostParams, error) {
	if err := validateHostID(h.ID); err != nil {
		return dbsqlc.UpsertHostParams{}, err
	}
	if strings.TrimSpace(h.MeshIdentity) == "" {
		return dbsqlc.UpsertHostParams{}, fmt.Errorf("storage: host %s has an empty Mesh identity", h.ID)
	}
	if err := validateOptionalText("alias", h.Alias); err != nil {
		return dbsqlc.UpsertHostParams{}, fmt.Errorf("storage: host %s: %w", h.ID, err)
	}
	if err := validateOptionalText("Tailscale name", h.TailscaleName); err != nil {
		return dbsqlc.UpsertHostParams{}, fmt.Errorf("storage: host %s: %w", h.ID, err)
	}
	lastSeenAt, err := timeMillis("last seen time", h.LastSeenAt)
	if err != nil {
		return dbsqlc.UpsertHostParams{}, fmt.Errorf("storage: host %s: %w", h.ID, err)
	}
	return dbsqlc.UpsertHostParams{
		ID:            string(h.ID),
		Alias:         cloneString(h.Alias),
		MeshIdentity:  h.MeshIdentity,
		TailscaleName: cloneString(h.TailscaleName),
		LastSeenAt:    lastSeenAt,
	}, nil
}

func sessionParams(s Session) (dbsqlc.UpsertSessionParams, error) {
	if err := validateSessionID(s.ID); err != nil {
		return dbsqlc.UpsertSessionParams{}, err
	}
	if err := validateHostID(s.HostID); err != nil {
		return dbsqlc.UpsertSessionParams{}, fmt.Errorf("storage: session %s: %w", s.ID, err)
	}
	if len(s.Command) == 0 || s.Command[0] == "" {
		return dbsqlc.UpsertSessionParams{}, fmt.Errorf("storage: session %s has no command", s.ID)
	}
	if err := validateState(s.State); err != nil {
		return dbsqlc.UpsertSessionParams{}, fmt.Errorf("storage: session %s: %w", s.ID, err)
	}
	if err := validateExit(s.State, s.ExitCode); err != nil {
		return dbsqlc.UpsertSessionParams{}, fmt.Errorf("storage: session %s: %w", s.ID, err)
	}
	createdAt, err := timeMillis("creation time", s.CreatedAt)
	if err != nil {
		return dbsqlc.UpsertSessionParams{}, fmt.Errorf("storage: session %s: %w", s.ID, err)
	}
	lastAttachedAt, err := optionalTimeMillis("last attachment time", s.LastAttachedAt)
	if err != nil {
		return dbsqlc.UpsertSessionParams{}, fmt.Errorf("storage: session %s: %w", s.ID, err)
	}
	if s.LastOutputSequence > math.MaxInt64 {
		return dbsqlc.UpsertSessionParams{}, fmt.Errorf("storage: session %s output sequence exceeds SQLite INTEGER", s.ID)
	}
	command, err := json.Marshal(s.Command)
	if err != nil {
		return dbsqlc.UpsertSessionParams{}, fmt.Errorf("storage: encode session %s command: %w", s.ID, err)
	}
	return dbsqlc.UpsertSessionParams{
		ID:                 string(s.ID),
		HostID:             string(s.HostID),
		Command:            string(command),
		Cwd:                s.Cwd,
		State:              string(s.State),
		CreatedAt:          createdAt,
		LastAttachedAt:     lastAttachedAt,
		ExitCode:           intToInt64(s.ExitCode),
		LastOutputSequence: int64(s.LastOutputSequence),
	}, nil
}

func hostFromRow(row dbsqlc.Host) (Host, error) {
	h := Host{
		ID:            HostID(row.ID),
		Alias:         cloneString(row.Alias),
		MeshIdentity:  row.MeshIdentity,
		TailscaleName: cloneString(row.TailscaleName),
		LastSeenAt:    time.UnixMilli(row.LastSeenAt).UTC(),
	}
	if _, err := hostParams(h); err != nil {
		return Host{}, fmt.Errorf("storage: decode host row: %w", err)
	}
	return h, nil
}

func sessionFromRow(row dbsqlc.Session) (Session, error) {
	state := SessionState(row.State)
	if err := validateState(state); err != nil {
		return Session{}, fmt.Errorf("storage: decode session %s: %w", row.ID, err)
	}
	var command []string
	if err := json.Unmarshal([]byte(row.Command), &command); err != nil {
		return Session{}, fmt.Errorf("storage: decode session %s command: %w", row.ID, err)
	}
	exitCode, err := int64ToInt(row.ExitCode)
	if err != nil {
		return Session{}, fmt.Errorf("storage: decode session %s exit code: %w", row.ID, err)
	}
	s := Session{
		ID:                 SessionID(row.ID),
		HostID:             HostID(row.HostID),
		Command:            command,
		Cwd:                row.Cwd,
		State:              state,
		CreatedAt:          time.UnixMilli(row.CreatedAt).UTC(),
		LastAttachedAt:     timeFromMillis(row.LastAttachedAt),
		ExitCode:           exitCode,
		LastOutputSequence: uint64(row.LastOutputSequence),
	}
	if _, err := sessionParams(s); err != nil {
		return Session{}, fmt.Errorf("storage: decode session row: %w", err)
	}
	return s, nil
}

func validateOptionalText(name string, value *string) error {
	if value != nil && *value == "" {
		return fmt.Errorf("empty %s", name)
	}
	return nil
}

func timeMillis(name string, value time.Time) (int64, error) {
	if value.IsZero() {
		return 0, fmt.Errorf("missing %s", name)
	}
	millis := value.UTC().UnixMilli()
	if millis < 0 {
		return 0, fmt.Errorf("%s predates the Unix epoch", name)
	}
	return millis, nil
}

func optionalTimeMillis(name string, value *time.Time) (*int64, error) {
	if value == nil {
		return nil, nil
	}
	millis, err := timeMillis(name, *value)
	if err != nil {
		return nil, err
	}
	return &millis, nil
}

func timeFromMillis(value *int64) *time.Time {
	if value == nil {
		return nil
	}
	t := time.UnixMilli(*value).UTC()
	return &t
}

func intToInt64(value *int) *int64 {
	if value == nil {
		return nil
	}
	v := int64(*value)
	return &v
}

func int64ToInt(value *int64) (*int, error) {
	if value == nil {
		return nil, nil
	}
	if strconv.IntSize == 32 && (*value < math.MinInt32 || *value > math.MaxInt32) {
		return nil, fmt.Errorf("value %d exceeds int", *value)
	}
	v := int(*value)
	return &v, nil
}

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	v := *value
	return &v
}
