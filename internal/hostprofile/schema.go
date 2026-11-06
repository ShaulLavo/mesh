// Package hostprofile owns owner-authored notes and their local persistence.
package hostprofile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const SchemaVersion = 1

type Editable struct {
	SchemaVersion int               `json:"schemaVersion"`
	Description   string            `json:"description"`
	Labels        map[string]string `json:"labels"`
	Warnings      []string          `json:"warnings"`
	Instructions  string            `json:"instructions"`
}

type Limits struct{ Description, Instructions, Labels, LabelKey, LabelValue, Warnings, Warning, Encoded int }

var HostLimits = Limits{1 << 10, 16 << 10, 32, 64, 256, 16, 512, 32 << 10}

type HostProfile struct {
	Editable
	HostID    string    `json:"hostId"`
	Revision  uint64    `json:"revision"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func (p HostProfile) Digest() string {
	data, _ := json.Marshal(p)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

type ProfileState string

const (
	StateAbsent      ProfileState = "absent"
	StateValid       ProfileState = "valid"
	StateInvalid     ProfileState = "invalid"
	StateUnavailable ProfileState = "unavailable"
)

type Snapshot[T any] struct {
	State    ProfileState
	Value    *T
	Revision *uint64
	Problem  string
}

type ValidationError struct{ Field, Problem string }

func (e *ValidationError) Error() string { return fmt.Sprintf("profile %s: %s", e.Field, e.Problem) }

type ConflictError struct{ Expected, Current uint64 }

func (e *ConflictError) Error() string {
	return fmt.Sprintf("The profile changed since revision %d; it is now revision %d. Read it again, then retry with --expected-revision %d.", e.Expected, e.Current, e.Current)
}

func problemText(text string) string {
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(text, "?"))
	if len(text) <= 512 {
		return text
	}
	limit := 512
	for !utf8.ValidString(text[:limit]) {
		limit--
	}
	return text[:limit]
}
