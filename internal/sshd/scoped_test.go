package sshd

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	charmssh "charm.land/ssh"
	"charm.land/wish/v2/testsession"
	"github.com/shaul/mesh/internal/access"
	"github.com/shaul/mesh/internal/identity"
	gossh "golang.org/x/crypto/ssh"
)

func registeredScope(t *testing.T, state, target, subject string) {
	t.Helper()
	root, err := os.OpenRoot(state)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := root.MkdirAll("access", 0700); err != nil {
		t.Fatal(err)
	}
	policy := access.PolicyView{Version: 1, Target: target, Revision: 1, Entries: []access.Entry{{Subject: access.Subject(subject), Incarnation: "00112233445566778899aabbccddeeff", Permissions: []string{"metadata"}, CreatedAt: time.Now().UTC().Add(-2 * time.Hour), ExpiresAt: time.Now().UTC().Add(-time.Hour)}}}
	data, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile("access/scopes.json", data, 0600); err != nil {
		t.Fatal(err)
	}
}
func TestSSHScopeMembershipIsRequiredConfiguration(t *testing.T) {
	_, err := validateConfig(t.Context(), Config{HostKey: generatePrivateKey(t), AuthorizedKeys: writeAuthorizedKeys(t, generatePrivateKey(t), 0600), Addr: "127.0.0.1:2222"})
	if err == nil {
		t.Fatal("nil scope membership observation accepted")
	}
}
func TestSSHScopedKeysCannotReachAnyApplicationEntry(t *testing.T) {
	for _, output := range []bool{false, true} {
		t.Run(fmt.Sprint(output), func(t *testing.T) { testScopedSSHEntries(t, output) })
	}
}
func testScopedSSHEntries(t *testing.T, output bool) {
	state := t.TempDir()
	host, hostKey, err := identity.LoadOrCreate(state)
	if err != nil {
		t.Fatal(err)
	}
	actor, actorKey, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := identity.ApproveDevice(t.Context(), state, actor.ID); err != nil {
		t.Fatal(err)
	}
	registeredScope(t, state, host.ID, actor.ID)
	if output {
		registerOutputPermission(t, state)
	}

	membership := func(subject string) (bool, error) { return access.Registered(state, access.Subject(subject)) }
	cfg, err := validateConfig(t.Context(), Config{HostKey: hostKey, AuthorizedKeys: filepath.Join(state, "authorized_keys"), Addr: "127.0.0.1:2222", ScopeMembership: membership})
	if err != nil {
		t.Fatal(err)
	}
	var effects atomic.Int32
	server, err := newServer(cfg, func(server *charmssh.Server) error {
		server.Handler = func(charmssh.Session) { effects.Add(1) }
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	address := testsession.Listen(t, server)
	commands := []string{"hello", "", "7K3D", "ls", "recover 7K3D --shell", "app list", `run {"argv":["sh"]}`, "scp -f /private", "sftp", "direct-tcpip", "tcpip-forward", "unknown"}
	for _, command := range commands {
		for _, pty := range []bool{false, true} {
			assertScopedSSHAuthenticationDenied(t, actorKey, address, command, pty)
		}
	}
	if effects.Load() != 0 {
		t.Fatal("scoped SSH reached application owner")
	}
}

type scopedSigningBarrier struct {
	gossh.Signer
	entered chan struct{}
	release <-chan struct{}
	ctx     context.Context
}

func (s scopedSigningBarrier) Sign(random io.Reader, data []byte) (*gossh.Signature, error) {
	close(s.entered)
	select {
	case <-s.release:
	case <-s.ctx.Done():
		return nil, fmt.Errorf("fixture signing cancelled: %w", s.ctx.Err())
	}
	signature, err := s.Signer.Sign(random, data)
	if err != nil {
		return nil, fmt.Errorf("fixture sign: %w", err)
	}
	return signature, nil
}
func TestSSHChecksScopedMembershipAfterUnsignedQuery(t *testing.T) {
	state := t.TempDir()
	host, hostKey, err := identity.LoadOrCreate(state)
	if err != nil {
		t.Fatal(err)
	}
	actor, actorKey, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := identity.ApproveDevice(t.Context(), state, actor.ID); err != nil {
		t.Fatal(err)
	}
	cfg := Config{HostKey: hostKey, AuthorizedKeys: filepath.Join(state, "authorized_keys"), Addr: "127.0.0.1:2222", ScopeMembership: func(subject string) (bool, error) { return access.Registered(state, access.Subject(subject)) }}
	server := mustServer(t, cfg)
	address := testsession.Listen(t, server)
	signer, err := gossh.NewSignerFromKey(actorKey)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	done := make(chan error, 1)
	configuration := clientConfig(t, actorKey, nil)
	configuration.Auth = []gossh.AuthMethod{gossh.PublicKeys(scopedSigningBarrier{Signer: signer, entered: entered, release: release, ctx: ctx})}
	go func() {
		client, err := gossh.Dial("tcp", address, configuration)
		if client != nil {
			_ = client.Close()
		}
		done <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("unsigned approved query did not reach signer")
	}
	registeredScope(t, state, host.ID, actor.ID)
	// A closed release channel admits exactly the already queried signed attempt.
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("signed acceptance ignored newly registered scope")
		}
	case <-ctx.Done():
		t.Fatal("signed scope denial did not finish")
	}
}
func TestSSHScopeCheckUsesTheProvedIdentity(t *testing.T) {
	key := generatePrivateKey(t)
	public, err := gossh.NewPublicKey(key.Public())
	if err != nil {
		t.Fatal(err)
	}
	expected := base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
	checked := ""
	cfg := normalizedConfig{scopeMembership: func(subject string) (bool, error) { checked = subject; return true, nil }}
	if cfg.unscoped(public) || checked != expected {
		t.Fatal("SSH membership was not derived from proved key")
	}
}

func TestSSHOtherNativeKeyKeepsItsGrantAndFailsOnUnsafeScopeState(t *testing.T) {
	state := t.TempDir()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(state)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := root.WriteFile("authorized_keys", gossh.MarshalAuthorizedKey(signer.PublicKey()), 0600); err != nil {
		t.Fatal(err)
	}
	server := mustServer(t, Config{HostKey: generatePrivateKey(t), AuthorizedKeys: filepath.Join(state, "authorized_keys"), Addr: "127.0.0.1:2222", ScopeMembership: func(subject string) (bool, error) { return access.Registered(state, access.Subject(subject)) }})
	address := testsession.Listen(t, server)
	config := &gossh.ClientConfig{User: "mesh", Auth: []gossh.AuthMethod{gossh.PublicKeys(signer)}, HostKeyCallback: gossh.FixedHostKey(server.HostSigners[0].PublicKey()), Timeout: time.Second}
	client, err := gossh.Dial("tcp", address, config)
	if err != nil {
		t.Fatal("native non-scoped key lost existing grant", err)
	}
	_ = client.Close()
	if err := root.Mkdir("access", 0700); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile("access/scopes.json", []byte("damaged"), 0600); err != nil {
		t.Fatal(err)
	}
	client, err = gossh.Dial("tcp", address, config)
	if client != nil {
		_ = client.Close()
	}
	if err == nil {
		t.Fatal("non-Ed25519 SSH key bypassed unsafe scope snapshot")
	}
}

func assertScopedSSHAuthenticationDenied(t *testing.T, key ed25519.PrivateKey, address, command string, pty bool) {
	t.Helper()
	client, err := gossh.Dial("tcp", address, clientConfig(t, key, nil))
	if client != nil {
		_ = client.Close()
	}
	if err == nil {
		t.Fatalf("scoped authentication reached %q pty=%v", command, pty)
	}
}

func registerOutputPermission(t *testing.T, state string) {
	t.Helper()
	root, err := os.OpenRoot(state)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile("access/scopes.json")
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte(`["metadata"]`), []byte(`["metadata","session-output"]`), 1)
	if err := root.WriteFile("access/scopes.json", data, 0600); err != nil {
		t.Fatal(err)
	}
}
