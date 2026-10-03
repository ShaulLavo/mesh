package sshd

import (
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"charm.land/wish/v2/testsession"
	"github.com/shaul/mesh/internal/identity"
	gossh "golang.org/x/crypto/ssh"
)

type queriedPublicSigner struct{ key gossh.PublicKey }

func (s queriedPublicSigner) PublicKey() gossh.PublicKey { return s.key }
func (s queriedPublicSigner) Sign(io.Reader, []byte) (*gossh.Signature, error) {
	return nil, errors.New("fixture has no queried-key private material")
}

func TestQueriedKeyMustNotOwnAuthenticatedSSHGrant(t *testing.T) {
	state := t.TempDir()
	queried, queriedKey, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	actor, actorKey, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{queried.ID, actor.ID} {
		if err := identity.ApproveDevice(state, id); err != nil {
			t.Fatal(err)
		}
	}
	queriedPublic, err := gossh.NewPublicKey(queriedKey.Public())
	if err != nil {
		t.Fatal(err)
	}
	actorSigner, err := gossh.NewSignerFromKey(actorKey)
	if err != nil {
		t.Fatal(err)
	}
	server := mustServer(t, Config{HostKey: generatePrivateKey(t), AuthorizedKeys: filepath.Join(state, "authorized_keys"), Addr: "127.0.0.1:2222"})
	address := testsession.Listen(t, server)
	attempts := 0
	//nolint:gosec // loopback fixture inspects client authentication, not destination identity
	config := &gossh.ClientConfig{User: "mesh", HostKeyCallback: gossh.InsecureIgnoreHostKey(), Timeout: time.Second,
		AuthCallback: func(*gossh.ClientAuthContext) (gossh.AuthMethod, error) {
			attempts++
			if attempts == 1 {
				return gossh.PublicKeys(queriedPublicSigner{queriedPublic}), nil
			}
			if attempts == 2 {
				return gossh.PublicKeys(actorSigner), nil
			}
			return nil, errors.New("unexpected fixture auth attempt")
		},
	}
	client, err := gossh.Dial("tcp", address, config)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	if attempts != 2 {
		t.Fatalf("auth attempts=%d", attempts)
	}
	good, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := good.Run(""); err != nil {
		t.Fatal(err)
	}
	_ = good.Close()
	if err := identity.RevokeDevice(state, queried.ID); err != nil {
		t.Fatal(err)
	}
	retained, err := client.NewSession()
	if err != nil {
		t.Fatalf("revoking queried key retired the signed device: %v", err)
	}
	if err := retained.Run(""); err != nil {
		t.Fatalf("signed device lost its independent grant: %v", err)
	}
	_ = retained.Close()
	if err := identity.RevokeDevice(state, actor.ID); err != nil {
		t.Fatal(err)
	}
	// No temporary absence sample or watcher delay; a post-revocation request must fail.
	bad, err := client.NewSession()
	if err != nil {
		return
	}
	defer func() { _ = bad.Close() }()
	if err := bad.Run(""); err == nil {
		t.Fatal("revoked authenticated key still executed SSH hello using only another device's public-key query")
	}
}
