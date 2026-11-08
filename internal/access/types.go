// Package access owns target-local scoped grants and writer-only key-class exclusion.
package access

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"slices"
	"sync/atomic"
	"time"
)

const (
	MetadataPermission      = "metadata"
	SessionOutputPermission = "session-output"
)

type Class uint8

const (
	Unauthenticated Class = iota
	LocalAccount
	FullDevice
	UpdateCarrier
	Scoped
)

type Subject string

func ValidateSubject(subject Subject) error {
	raw, err := base64.RawURLEncoding.DecodeString(string(subject))
	if err != nil || len(raw) != ed25519.PublicKeySize || base64.RawURLEncoding.EncodeToString(raw) != string(subject) {
		return errors.New("access: invalid public identity")
	}
	return nil
}

var (
	ErrDenied   = errors.New("access: permission denied")
	ErrRevision = errors.New("access: policy revision changed")
)

type Entry struct {
	Subject     Subject   `json:"subject"`
	Incarnation string    `json:"incarnation"`
	Permissions []string  `json:"permissions"`
	CreatedAt   time.Time `json:"createdAt"`
	ExpiresAt   time.Time `json:"expiresAt"`
}
type PolicyView struct {
	Version  int     `json:"version"`
	Target   string  `json:"target"`
	Revision uint64  `json:"revision"`
	Entries  []Entry `json:"entries"`
}
type GrantInput struct {
	Permissions []string
	Subject     Subject
	ExpiresAt   time.Time
}
type OtherRoles struct{ FullDevice, UpdateAdministrator bool }
type ObserveOtherRoles func(context.Context, Subject) (OtherRoles, error)
type Change struct {
	Revision uint64 `json:"revision"`
	Changed  bool   `json:"changed"`
	Entry    *Entry `json:"entry"`
}

type Principal struct {
	target string
	entry  Entry
}

func (p Principal) Target() string        { return p.target }
func (p Principal) Subject() Subject      { return p.entry.Subject }
func (p Principal) Incarnation() string   { return p.entry.Incarnation }
func (p Principal) ExpiresAt() time.Time  { return p.entry.ExpiresAt }
func (p Principal) Permissions() []string { return append([]string{}, p.entry.Permissions...) }

type bindingState struct {
	principal Principal
	invalid   atomic.Bool
	current   func() bool
}
type Binding struct{ state *bindingState }

func (b Binding) Principal() Principal {
	if b.state == nil {
		return Principal{}
	}
	return b.state.principal
}
func (b Binding) Current() bool {
	if b.state == nil || b.state.invalid.Load() {
		return false
	}
	if !b.state.current() {
		b.state.invalid.Store(true)
		return false
	}
	return !b.state.invalid.Load()
}
func (b Binding) RequireMetadata() error {
	if !b.Current() {
		return ErrDenied
	}
	return nil
}

// Constrain composes a trusted boundary observation without changing the subject.
// Copies share the resulting invalidation latch, including its parent binding.
func (b Binding) Constrain(check func() bool) Binding {
	if b.state == nil || check == nil {
		return Binding{}
	}
	return Binding{state: &bindingState{principal: b.state.principal, current: func() bool { return b.Current() && check() }}}
}

func (b Binding) RequireOutput() error {
	if err := b.RequireMetadata(); err != nil {
		return err
	}
	if !slices.Equal(b.state.principal.entry.Permissions, []string{MetadataPermission, SessionOutputPermission}) {
		return ErrDenied
	}
	return nil
}

func ValidPermissions(permissions []string) bool {
	return slices.Equal(permissions, []string{MetadataPermission}) || slices.Equal(permissions, []string{MetadataPermission, SessionOutputPermission})
}
