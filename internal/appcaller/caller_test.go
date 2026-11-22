package appcaller

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDeriveSeparatesCredentialsAndScopes(t *testing.T) {
	a, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	b, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	device := Scope{Class: ScopeDevice, Principal: "device-key"}
	other := Scope{Class: ScopeDevice, Principal: "other-key"}
	local := Scope{Class: ScopeLocalAccount, Principal: "daemon-account"}
	ids := map[ID]string{}
	for name, pair := range map[string]struct {
		scope Scope
		cred  Credential
	}{"a@device": {device, a}, "b@device": {device, b}, "a@other": {other, a}, "a@local": {local, a}} {
		id, err := Derive(pair.scope, pair.cred)
		if err != nil {
			t.Fatal(err)
		}
		if previous, ok := ids[id]; ok {
			t.Fatalf("%s and %s derived the same ID", name, previous)
		}
		ids[id] = name
	}
	again, _ := Derive(device, a)
	first, _ := Derive(device, a)
	if !again.Equal(first) || again.Equal(ID("")) {
		t.Fatal("derivation is not stable or Equal accepts an empty ID")
	}
}

func TestCredentialRoundTripAndRedaction(t *testing.T) {
	c, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseCredential(c.Encode())
	if err != nil || parsed.Encode() != c.Encode() {
		t.Fatalf("round trip failed: %v", err)
	}
	for _, text := range []string{fmt.Sprint(c), fmt.Sprintf("%v %+v %#v", c, c, c), fmt.Sprintf("%+v", Identity{Name: "x", Credential: c})} {
		if strings.Contains(text, c.Encode()[len(credentialPrefix):]) {
			t.Fatalf("formatting leaked the credential: %s", text)
		}
	}
	for _, bad := range []string{"", "mac1_", "mac2_" + c.Encode()[5:], c.Encode() + "A", "mac1_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"} {
		if _, err := ParseCredential(bad); !errors.Is(err, ErrInvalidCredential) {
			t.Errorf("accepted %q", bad)
		}
	}
	if _, err := Verify(Scope{Class: "scoped", Principal: "x"}, c.Encode()); err == nil {
		t.Fatal("verified a credential in an unauthenticated scope")
	}
}

func TestIdentityStoreIsPrivateAndNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	created, err := Create(dir, "codex", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Create(dir, "codex", time.Now()); !errors.Is(err, ErrIdentityExists) {
		t.Fatalf("second create = %v, want exists", err)
	}
	loaded, err := Load(dir, "codex")
	if err != nil || loaded.Credential.Encode() != created.Credential.Encode() {
		t.Fatalf("load = %v", err)
	}
	info, err := os.Stat(filepath.Join(Dir(dir), "codex.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("identity mode = %v, %v", info.Mode(), err)
	}
	if err := os.Chmod(filepath.Join(Dir(dir), "codex.json"), 0o644); err != nil { //nolint:gosec // the test makes the file readable on purpose

		t.Fatal(err)
	}
	if _, err := Load(dir, "codex"); err == nil {
		t.Fatal("loaded a world-readable credential")
	}
	if _, err := Create(dir, "claude", time.Now()); err != nil {
		t.Fatal(err)
	}
	list, err := List(dir)
	if err != nil || len(list) != 1 || list[0].Name != "claude" {
		t.Fatalf("list = %+v, %v; the unsafe identity must be skipped", list, err)
	}
	if _, err := Load(dir, "missing"); !errors.Is(err, ErrIdentityNotFound) {
		t.Fatalf("missing = %v", err)
	}
	for _, name := range []string{"", "-x", "a/b", "..", strings.Repeat("a", 65)} {
		if ValidName(name) == nil {
			t.Errorf("accepted name %q", name)
		}
	}
}
