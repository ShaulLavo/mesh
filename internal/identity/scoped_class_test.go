package identity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestApprovalRejectsRegisteredScopedKey(t *testing.T) {
	state := t.TempDir()
	target, _, err := LoadOrCreate(state)
	if err != nil {
		t.Fatal(err)
	}
	subject, _, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(state, "access")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	policy := map[string]any{"version": 1, "target": target.ID, "revision": 1, "entries": []any{map[string]any{
		"subject": subject.ID, "incarnation": "00112233445566778899aabbccddeeff", "permissions": []string{"metadata"},
		"createdAt": time.Now().Add(-2 * time.Hour).UTC(), "expiresAt": time.Now().Add(-time.Hour).UTC(),
	}}}
	data, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scopes.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := ApproveDevice(t.Context(), state, subject.ID); err == nil {
		t.Fatal("full approval acknowledged an expired registered scoped key")
	}
	if GrantedIdentity(state, subject.ID) {
		t.Fatal("rejected approval left a latent full grant")
	}
}
