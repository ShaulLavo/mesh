package sshd

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"testing"

	"github.com/shaul/mesh/internal/tunnel"
	gossh "golang.org/x/crypto/ssh"
)

func TestTunnelAuthorizerReloadsAndFailsClosed(t *testing.T) {
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	public := key.Public().(ed25519.PublicKey)
	sshPublic, err := gossh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "authorized_keys")
	allowed := Authorizer(path)
	id := tunnel.KeyID(public)
	if allowed(id) {
		t.Fatal("missing authorization file accepted")
	}
	if err := os.WriteFile(path, gossh.MarshalAuthorizedKey(sshPublic), 0o600); err != nil {
		t.Fatal(err)
	}
	if !allowed(id) {
		t.Fatal("managed key refused")
	}
	if err := os.Chmod(path, 0o666); err != nil { //nolint:gosec // deliberately insecure authorization state must fail closed
		t.Fatal(err)
	}
	if allowed(id) {
		t.Fatal("insecure file accepted")
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	if allowed(id) {
		t.Fatal("unreadable file accepted")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if allowed(id) {
		t.Fatal("removed key remained authorized")
	}
	if allowed("display-alias") {
		t.Fatal("alias used as key ID")
	}
}
