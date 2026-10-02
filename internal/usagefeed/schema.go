// Package usagefeed reads sanitized quota observations without contacting providers.
package usagefeed

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const MaxBytes = 64 << 10

// Snapshot preserves publication time separately from upstream observation times.
type Snapshot struct {
	SchemaVersion int       `json:"schemaVersion"`
	GeneratedAt   time.Time `json:"generatedAt"`
	Accounts      []Account `json:"accounts"`
}

type Account struct {
	ID         string     `json:"id"`
	Provider   string     `json:"provider"`
	Label      string     `json:"label"`
	Plan       string     `json:"plan"`
	CheckedAt  *time.Time `json:"checkedAt"`
	LastSeenAt *time.Time `json:"lastSeenAt"`
	State      string     `json:"state"`
	Source     string     `json:"source"`
	Routing    Routing    `json:"routing"`
	Windows    []Window   `json:"windows"`
	Cooldown   *Cooldown  `json:"cooldown"`
}

type Routing struct {
	Mode         string     `json:"mode"`
	Active       *bool      `json:"active"`
	LastServedAt *time.Time `json:"lastServedAt"`
}

type Window struct {
	ID            string     `json:"id"`
	Label         string     `json:"label"`
	UsedPercent   *float64   `json:"usedPercent"`
	ResetsAt      *time.Time `json:"resetsAt"`
	WindowMinutes *float64   `json:"windowMinutes"`
	Status        string     `json:"status"`
	LastSeenAt    *time.Time `json:"lastSeenAt"`
	Source        string     `json:"source"`
}

type Cooldown struct {
	Reason     string     `json:"reason"`
	Until      *time.Time `json:"until"`
	ObservedAt time.Time  `json:"observedAt"`
	Source     string     `json:"source"`
}

func decode(data []byte) (*Snapshot, error) {
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("usage feed: invalid text encoding")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var snapshot Snapshot
	if err := decoder.Decode(&snapshot); err != nil {
		return nil, fmt.Errorf("usage feed: invalid JSON")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("usage feed: trailing data")
	}
	if err := validate(snapshot); err != nil {
		return nil, err
	}
	return &snapshot, nil
}

func validate(snapshot Snapshot) error {
	if snapshot.SchemaVersion != 1 || !timestamp(&snapshot.GeneratedAt) || snapshot.Accounts == nil {
		return fmt.Errorf("usage feed: unsupported or incomplete snapshot")
	}
	ids := make(map[string]bool, len(snapshot.Accounts))
	for _, account := range snapshot.Accounts {
		if ids[account.ID] || !text(account.ID) || !text(account.Provider) || !text(account.Label) || !text(account.Plan) {
			return fmt.Errorf("usage feed: invalid account identity")
		}
		ids[account.ID] = true
		if err := validateAccount(account); err != nil {
			return err
		}
	}
	return nil
}

func validateAccount(account Account) error {
	if !oneOf(account.State, "ready", "cooldown", "disabled", "no-data", "unknown") || !source(account.Source) || !oneOf(account.Routing.Mode, "single", "rotating", "unknown") || account.Windows == nil {
		return fmt.Errorf("usage feed: invalid account state")
	}
	if !timestamp(account.CheckedAt) || !timestamp(account.LastSeenAt) || !timestamp(account.Routing.LastServedAt) {
		return fmt.Errorf("usage feed: invalid account timestamp")
	}
	if account.Cooldown != nil {
		if err := validateCooldown(*account.Cooldown); err != nil {
			return err
		}
	}
	ids := make(map[string]bool, len(account.Windows))
	for _, window := range account.Windows {
		if ids[window.ID] || !text(window.ID) || !text(window.Label) {
			return fmt.Errorf("usage feed: invalid window identity")
		}
		ids[window.ID] = true
		if err := validateWindow(window); err != nil {
			return err
		}
	}
	return nil
}

func validateWindow(window Window) error {
	if !oneOf(window.Status, "allowed", "warning", "exhausted", "unknown") || !source(window.Source) {
		return fmt.Errorf("usage feed: invalid window state")
	}
	if window.UsedPercent != nil && (!finite(*window.UsedPercent) || *window.UsedPercent < 0 || *window.UsedPercent > 100 || window.LastSeenAt == nil) {
		return fmt.Errorf("usage feed: invalid observed percentage")
	}
	if window.WindowMinutes != nil && (!finite(*window.WindowMinutes) || *window.WindowMinutes <= 0) {
		return fmt.Errorf("usage feed: invalid window duration")
	}
	if !timestamp(window.ResetsAt) || !timestamp(window.LastSeenAt) {
		return fmt.Errorf("usage feed: invalid window timestamp")
	}
	return nil
}

func validateCooldown(cooldown Cooldown) error {
	if !oneOf(cooldown.Reason, "unknown", "credential_quota", "quota", "cloudflare_challenge", "model_not_supported", "invalid_grant", "unauthorized", "payment_required", "not_found", "transient_error") || !source(cooldown.Source) || !timestamp(&cooldown.ObservedAt) || !timestamp(cooldown.Until) {
		return fmt.Errorf("usage feed: invalid cooldown")
	}
	return nil
}

func source(value string) bool  { return oneOf(value, "passive-header", "proxy-state") }
func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }
func oneOf(value string, choices ...string) bool {
	for _, choice := range choices {
		if value == choice {
			return true
		}
	}
	return false
}
func timestamp(value *time.Time) bool {
	if value == nil {
		return true
	}
	_, offset := value.Zone()
	return !value.IsZero() && offset == 0 && value.Year() >= 1 && value.Year() <= 9999
}
func text(value string) bool {
	if len(value) == 0 || len(value) > 128 || strings.TrimSpace(value) != value || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.Is(unicode.Cf, character) {
			return false
		}
	}
	return true
}

func clone(snapshot *Snapshot) *Snapshot {
	if snapshot == nil {
		return nil
	}
	result := *snapshot
	result.Accounts = make([]Account, len(snapshot.Accounts))
	for index, account := range snapshot.Accounts {
		account.CheckedAt, account.LastSeenAt = copyValue(account.CheckedAt), copyValue(account.LastSeenAt)
		account.Routing.Active = copyValue(account.Routing.Active)
		account.Routing.LastServedAt = copyValue(account.Routing.LastServedAt)
		if account.Cooldown != nil {
			cooldown := *account.Cooldown
			cooldown.Until = copyValue(cooldown.Until)
			account.Cooldown = &cooldown
		}
		account.Windows = make([]Window, len(account.Windows))
		for position, window := range snapshot.Accounts[index].Windows {
			window.UsedPercent, window.WindowMinutes = copyValue(window.UsedPercent), copyValue(window.WindowMinutes)
			window.LastSeenAt, window.ResetsAt = copyValue(window.LastSeenAt), copyValue(window.ResetsAt)
			account.Windows[position] = window
		}
		result.Accounts[index] = account
	}
	return &result
}
func copyValue[T any](value *T) *T {
	if value == nil {
		return nil
	}
	result := *value
	return &result
}
