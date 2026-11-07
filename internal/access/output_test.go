package access

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestPolicyAcceptsClosedSessionOutputPermission(t *testing.T) {
	s, subject, expiry := testStore(t)
	change, err := s.Grant(t.Context(), 0, GrantInput{Permissions: []string{"metadata"}, Subject: subject, ExpiresAt: expiry}, noRoles)
	if err != nil {
		t.Fatal(err)
	}
	entry := *change.Entry
	entry.Permissions = []string{"metadata", "session-output"}
	data, err := json.Marshal(PolicyView{Version: 1, Target: s.target, Revision: 1, Entries: []Entry{entry}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parsePolicy(data); err != nil {
		t.Fatal("reviewed optional output grant rejected", err)
	}
}

func TestOutputReplacementRetiresBindingsAndExactRetryPreserves(t *testing.T) {
	store, subject, expiry := testStore(t)
	first := testGrant(t, store, 0, subject, expiry)
	metadata, err := store.Bind(subject)
	if err != nil {
		t.Fatal(err)
	}
	if err := metadata.RequireOutput(); !errors.Is(err, ErrDenied) {
		t.Fatal("metadata read output", err)
	}
	output := GrantInput{Subject: subject, ExpiresAt: expiry, Permissions: []string{"metadata", "session-output"}}
	replaced, err := store.Grant(t.Context(), 1, output, noRoles)
	if err != nil || replaced.Entry.Incarnation == first.Entry.Incarnation || metadata.Current() {
		t.Fatal("expansion failed to retire", replaced, err)
	}
	bound, err := store.Bind(subject)
	if err != nil || bound.RequireOutput() != nil {
		t.Fatal("output did not bind", err)
	}
	retry, err := store.Grant(t.Context(), 1, output, noRoles)
	if err != nil || retry.Changed || !bound.Current() {
		t.Fatal("exact output retry changed binding", err)
	}
	testGrant(t, store, 2, testSubject(t), expiry)
	if bound.RequireOutput() != nil {
		t.Fatal("unrelated grant retired output")
	}
	testGrant(t, store, 3, subject, expiry)
	if bound.Current() || metadata.Current() {
		t.Fatal("permission reduction healed binding")
	}
}
func TestGrantRejectsEveryOtherPermissionSet(t *testing.T) {
	store, subject, expiry := testStore(t)
	for _, permissions := range [][]string{nil, {}, {"session-output"}, {"session-output", "metadata"}, {"metadata", "metadata"}, {"metadata", "execute"}, {"metadata", "session-output", "execute"}} {
		_, err := store.Grant(t.Context(), 0, GrantInput{Subject: subject, ExpiresAt: expiry, Permissions: permissions}, noRoles)
		if err == nil {
			t.Fatalf("invalid permissions accepted: %v", permissions)
		}
	}
}

func TestOutputBindingExpiryNeverHeals(t *testing.T) {
	store, subject, expiry := testStore(t)
	_, err := store.Grant(t.Context(), 0, GrantInput{Subject: subject, ExpiresAt: expiry, Permissions: []string{"metadata", "session-output"}}, noRoles)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := store.Bind(subject)
	if err != nil || bound.RequireOutput() != nil {
		t.Fatal("output grant unavailable", err)
	}
	now := expiry.Add(time.Second)
	store.now = func() time.Time { return now }
	if err := bound.RequireOutput(); !errors.Is(err, ErrDenied) {
		t.Fatal("expired output remained admitted", err)
	}
	now = expiry.Add(-time.Hour)
	if bound.Current() {
		t.Fatal("clock rollback healed expired output")
	}
}
