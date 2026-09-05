// Package agentresume describes native conversation recovery without storing transcripts.
package agentresume

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/shaul/mesh/internal/session"
)

const (
	Version        = 1
	MaxFieldBytes  = 4096
	MaxEventBytes  = 16 << 10
	MaxOptions     = 64
	MaxOptionBytes = 32 << 10
)

type Provider string

const (
	Codex  Provider = "codex"
	Claude Provider = "claude"
)

type Lifecycle string

const (
	Active   Lifecycle = "active"
	Closed   Lifecycle = "closed"
	Explicit Lifecycle = "explicit"
)

type Launch struct {
	Provider        Provider `json:"provider"`
	Executable      string   `json:"executable"`
	ProviderVersion string   `json:"providerVersion"`
	Directory       string   `json:"directory"`
	DataRoot        string   `json:"dataRoot"`
	Options         []string `json:"options,omitempty"`
}

type Recipe struct {
	Version int `json:"version"`
	Launch
	ConversationID  string    `json:"conversationId"`
	InvocationToken string    `json:"invocationToken"`
	RegisteredAt    time.Time `json:"registeredAt"`
	Lifecycle       Lifecycle `json:"lifecycle"`
	Explicit        bool      `json:"explicit,omitempty"`
}

type Receipt struct {
	SourceSessionID string    `json:"sourceSessionId"`
	Provider        Provider  `json:"provider"`
	ConversationID  string    `json:"conversationId"`
	InvocationToken string    `json:"invocationToken"`
	VerifiedAt      time.Time `json:"verifiedAt"`
}

type EventKind string

const (
	Start EventKind = "start"
	End   EventKind = "end"
)

type Event struct {
	Kind           EventKind `json:"kind"`
	ConversationID string    `json:"conversationId"`
	Directory      string    `json:"directory"`
	Subagent       bool      `json:"subagent,omitempty"`
}

func ValidateProvider(provider Provider) error {
	if provider != Codex && provider != Claude {
		return fmt.Errorf("agent recovery: unsupported provider %q", provider)
	}
	return nil
}

func ValidateLaunch(launch Launch) error {
	if err := ValidateProvider(launch.Provider); err != nil {
		return err
	}
	if !absoluteField(launch.Executable) || !absoluteField(launch.Directory) || !absoluteField(launch.DataRoot) {
		return fmt.Errorf("agent recovery: executable, project and data root must be absolute bounded paths")
	}
	if !field(launch.ProviderVersion) {
		return fmt.Errorf("agent recovery: missing or invalid provider version")
	}
	return validateOptions(launch.Provider, launch.Options)
}

func ValidateRecipe(recipe Recipe) error {
	if recipe.Version != Version {
		return fmt.Errorf("agent recovery: unsupported recipe version %d", recipe.Version)
	}
	if err := ValidateLaunch(recipe.Launch); err != nil {
		return err
	}
	if !field(recipe.ConversationID) || !field(recipe.InvocationToken) || recipe.RegisteredAt.IsZero() {
		return fmt.Errorf("agent recovery: missing or invalid conversation identity")
	}
	switch recipe.Lifecycle {
	case Active, Closed, Explicit:
		return nil
	default:
		return fmt.Errorf("agent recovery: invalid lifecycle %q", recipe.Lifecycle)
	}
}

func ValidateReceipt(receipt Receipt) error {
	if err := ValidateProvider(receipt.Provider); err != nil {
		return err
	}
	id, err := session.ParseID(receipt.SourceSessionID)
	if err != nil || id != receipt.SourceSessionID || receipt.VerifiedAt.IsZero() || !field(receipt.ConversationID) || !field(receipt.InvocationToken) {
		return fmt.Errorf("agent recovery: invalid resume receipt")
	}
	return nil
}

func ValidateEvent(event Event) error {
	if event.Kind != Start && event.Kind != End {
		return fmt.Errorf("agent recovery: unsupported event %q", event.Kind)
	}
	if !field(event.ConversationID) || !absoluteField(event.Directory) {
		return fmt.Errorf("agent recovery: invalid event identity or directory")
	}
	return nil
}

func field(value string) bool {
	if value == "" || len(value) > MaxFieldBytes || !utf8.ValidString(value) {
		return false
	}
	return !strings.ContainsFunc(value, unicode.IsControl)
}

func absoluteField(value string) bool { return field(value) && filepath.IsAbs(value) }
