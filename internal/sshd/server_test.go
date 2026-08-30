package sshd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	charmssh "github.com/charmbracelet/ssh"
	"github.com/charmbracelet/wish"
	"github.com/charmbracelet/wish/testsession"
	"github.com/shaul/mesh/internal/identity"
	gossh "golang.org/x/crypto/ssh"
)

func TestServerAuthenticatesOnlyAuthorizedPublicKeys(t *testing.T) {
	hostKey := generatePrivateKey(t)
	authorized := generatePrivateKey(t)
	unauthorized := generatePrivateKey(t)
	authorizedKeys := writeAuthorizedKeys(t, authorized, 0o600)
	server := mustServer(t, Config{
		HostKey: hostKey, AuthorizedKeys: authorizedKeys, Addr: "127.0.0.1:2222",
	})
	address := testsession.Listen(t, server)

	t.Run("authorized", func(t *testing.T) {
		session, err := testsession.NewClientSession(t, address, clientConfig(t, authorized, nil))
		if err != nil {
			t.Fatal(err)
		}
		var output bytes.Buffer
		session.Stdout = &output
		if err := session.Run(""); err != nil {
			t.Fatal(err)
		}
		if output.String() != helloMessage {
			t.Fatalf("hello output = %q, want %q", output.String(), helloMessage)
		}
	})

	for name, config := range map[string]*gossh.ClientConfig{
		"unauthorized key": clientConfig(t, unauthorized, nil),
		"no key":           {User: "mesh", HostKeyCallback: gossh.InsecureIgnoreHostKey()}, //nolint:gosec // in-process authentication test
		"password": {
			User: "mesh", Auth: []gossh.AuthMethod{gossh.Password("nope")},
			HostKeyCallback: gossh.InsecureIgnoreHostKey(), //nolint:gosec // in-process authentication test
		},
		"keyboard interactive": {
			User: "mesh", Auth: []gossh.AuthMethod{gossh.KeyboardInteractive(func(string, string, []string, []bool) ([]string, error) {
				return []string{"nope"}, nil
			})},
			HostKeyCallback: gossh.InsecureIgnoreHostKey(), //nolint:gosec // in-process authentication test
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := testsession.NewClientSession(t, address, config)
			if err == nil || !strings.Contains(err.Error(), "unable to authenticate") {
				t.Fatalf("authentication error = %v", err)
			}
		})
	}
}

func TestExtensionOptionsCannotReplaceSecurityBoundary(t *testing.T) {
	hostKey := generatePrivateKey(t)
	authorized := generatePrivateKey(t)
	unauthorized := generatePrivateKey(t)
	otherHostPEM, _, err := marshalHostKey(generatePrivateKey(t))
	if err != nil {
		t.Fatal(err)
	}
	normalized, err := validateConfig(context.Background(), Config{
		HostKey: hostKey, AuthorizedKeys: writeAuthorizedKeys(t, authorized, 0o600), Addr: "127.0.0.1:2222",
	})
	if err != nil {
		t.Fatal(err)
	}
	server, err := newServer(normalized,
		wish.WithAddress("0.0.0.0:22"),
		wish.WithHostKeyPEM(otherHostPEM),
		wish.WithPasswordAuth(func(charmssh.Context, string) bool { return true }),
		wish.WithKeyboardInteractiveAuth(func(charmssh.Context, gossh.KeyboardInteractiveChallenge) bool { return true }),
		wish.WithPublicKeyAuth(func(charmssh.Context, charmssh.PublicKey) bool { return true }),
		func(server *charmssh.Server) error {
			server.ServerConfigCallback = func(charmssh.Context) *gossh.ServerConfig {
				return &gossh.ServerConfig{NoClientAuth: true}
			}
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if server.Addr != "127.0.0.1:2222" || server.PasswordHandler != nil || server.KeyboardInteractiveHandler != nil || server.ServerConfigCallback != nil {
		t.Fatalf("security-sensitive server fields were replaced: %#v", server)
	}
	if len(server.HostSigners) != 1 {
		t.Fatalf("host signer count = %d, want 1", len(server.HostSigners))
	}
	expectedHost, err := gossh.NewPublicKey(hostKey.Public())
	if err != nil {
		t.Fatal(err)
	}
	if !charmssh.KeysEqual(server.HostSigners[0].PublicKey(), expectedHost) {
		t.Fatal("extension option replaced the Mesh host identity")
	}
	address := testsession.Listen(t, server)
	_, err = testsession.NewClientSession(t, address, clientConfig(t, unauthorized, nil))
	if err == nil || !strings.Contains(err.Error(), "unable to authenticate") {
		t.Fatalf("overridden public-key authentication error = %v", err)
	}
	_, err = testsession.NewClientSession(t, address, &gossh.ClientConfig{
		User: "mesh", Auth: []gossh.AuthMethod{gossh.Password("accepted by extension")},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(), //nolint:gosec // in-process authentication test
	})
	if err == nil || !strings.Contains(err.Error(), "unable to authenticate") {
		t.Fatalf("overridden password authentication error = %v", err)
	}
}

func TestServerFailsClosedWhenAuthorizedKeysCannotBeTrusted(t *testing.T) {
	hostKey := generatePrivateKey(t)
	clientKey := generatePrivateKey(t)
	tests := []struct {
		name string
		path func(*testing.T) string
	}{
		{name: "missing", path: func(t *testing.T) string {
			return filepath.Join(t.TempDir(), "authorized_keys")
		}},
		{name: "unreadable", path: func(t *testing.T) string {
			return writeAuthorizedKeys(t, clientKey, 0o000)
		}},
		{name: "group writable", path: func(t *testing.T) string {
			return writeAuthorizedKeys(t, clientKey, 0o620)
		}},
		{name: "world writable", path: func(t *testing.T) string {
			return writeAuthorizedKeys(t, clientKey, 0o602)
		}},
		{name: "symlink", path: func(t *testing.T) string {
			target := writeAuthorizedKeys(t, clientKey, 0o600)
			link := filepath.Join(t.TempDir(), "authorized_keys")
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			return link
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := mustServer(t, Config{
				HostKey: hostKey, AuthorizedKeys: tt.path(t), Addr: "127.0.0.1:2222",
			})
			_, err := testsession.NewClientSession(t, testsession.Listen(t, server), clientConfig(t, clientKey, nil))
			if err == nil || !strings.Contains(err.Error(), "unable to authenticate") {
				t.Fatalf("authentication error = %v", err)
			}
		})
	}
}

func TestServerReReadsAuthorizedKeysForRevocation(t *testing.T) {
	hostKey := generatePrivateKey(t)
	clientKey := generatePrivateKey(t)
	authorizedKeys := writeAuthorizedKeys(t, clientKey, 0o600)
	server := mustServer(t, Config{
		HostKey: hostKey, AuthorizedKeys: authorizedKeys, Addr: "127.0.0.1:2222",
	})
	address := testsession.Listen(t, server)

	first, err := testsession.NewClientSession(t, address, clientConfig(t, clientKey, nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Run(""); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(authorizedKeys, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = testsession.NewClientSession(t, address, clientConfig(t, clientKey, nil))
	if err == nil || !strings.Contains(err.Error(), "unable to authenticate") {
		t.Fatalf("authentication after revocation error = %v", err)
	}
}

func TestServerHostKeyIsTheMeshIdentity(t *testing.T) {
	stateDir := t.TempDir()
	host, private, err := identity.LoadOrCreate(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	clientKey := generatePrivateKey(t)
	server := mustServer(t, Config{
		HostKey: private, AuthorizedKeys: writeAuthorizedKeys(t, clientKey, 0o600), Addr: "127.0.0.1:2222",
	})
	expected, err := gossh.NewPublicKey(host.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	var fingerprint string
	hostCallback := func(_ string, _ net.Addr, key gossh.PublicKey) error {
		fingerprint = gossh.FingerprintSHA256(key)
		if !charmssh.KeysEqual(key, expected) {
			return errors.New("SSH host key differs from Mesh identity")
		}
		return nil
	}
	session, err := testsession.NewClientSession(t, testsession.Listen(t, server), clientConfig(t, clientKey, hostCallback))
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Run(""); err != nil {
		t.Fatal(err)
	}
	if fingerprint != gossh.FingerprintSHA256(expected) {
		t.Fatalf("host fingerprint = %q, want %q", fingerprint, gossh.FingerprintSHA256(expected))
	}
}

func TestServerExecDoesNotRequirePTY(t *testing.T) {
	clientKey := generatePrivateKey(t)
	server := mustServer(t, Config{
		HostKey:        generatePrivateKey(t),
		AuthorizedKeys: writeAuthorizedKeys(t, clientKey, 0o600),
		Addr:           "127.0.0.1:2222",
	})
	address := testsession.Listen(t, server)

	session, err := testsession.NewClientSession(t, address, clientConfig(t, clientKey, nil))
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	session.Stdout = &output
	if err := session.Run(helloCommand); err != nil {
		t.Fatal(err)
	}
	if output.String() != helloMessage {
		t.Fatalf("exec output = %q, want %q", output.String(), helloMessage)
	}

	denied, err := testsession.NewClientSession(t, address, clientConfig(t, clientKey, nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := denied.Run("uname"); err == nil {
		t.Fatal("unregistered command succeeded")
	}
}

func TestServeBindsOnlyConfiguredAddressAndStopsWithContext(t *testing.T) {
	endpoint := reserveEndpoint(t, "127.0.0.2")
	clientKey := generatePrivateKey(t)
	hostKey := generatePrivateKey(t)
	authorizedKeys := writeAuthorizedKeys(t, clientKey, 0o600)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, Config{
			HostKey:        hostKey,
			AuthorizedKeys: authorizedKeys,
			Addr:           endpoint,
		})
	}()
	waitForListener(t, endpoint)

	_, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", port), 50*time.Millisecond)
	if err == nil {
		_ = connection.Close()
		t.Fatal("SSH listener also accepted on 127.0.0.1")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve() shutdown error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve() did not stop after cancellation")
	}
	if connection, err := net.DialTimeout("tcp4", endpoint, 50*time.Millisecond); err == nil {
		_ = connection.Close()
		t.Fatal("SSH listener still accepts after shutdown")
	}
}

func TestValidateConfigRejectsWildcardAndInvalidHostKey(t *testing.T) {
	private := generatePrivateKey(t)
	tests := []Config{
		{HostKey: private, AuthorizedKeys: "/tmp/authorized_keys", Addr: "0.0.0.0:2222"},
		{HostKey: private, AuthorizedKeys: "/tmp/authorized_keys", Addr: "[::]:2222"},
		{HostKey: private, AuthorizedKeys: "/tmp/authorized_keys", Addr: "127.0.0.1:0"},
		{HostKey: private[:32], AuthorizedKeys: "/tmp/authorized_keys", Addr: "127.0.0.1:2222"},
	}
	for _, cfg := range tests {
		if _, err := validateConfig(context.Background(), cfg); err == nil {
			t.Fatalf("validateConfig(%+v) succeeded", cfg)
		}
	}
}

func mustServer(t *testing.T, cfg Config) *charmssh.Server {
	t.Helper()
	normalized, err := validateConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	server, err := newServer(normalized)
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func generatePrivateKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return private
}

func writeAuthorizedKeys(t *testing.T, private ed25519.PrivateKey, mode os.FileMode) string {
	t.Helper()
	public, err := gossh.NewPublicKey(private.Public())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "authorized_keys")
	if err := os.WriteFile(path, gossh.MarshalAuthorizedKey(public), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func clientConfig(t *testing.T, private ed25519.PrivateKey, callback gossh.HostKeyCallback) *gossh.ClientConfig {
	t.Helper()
	signer, err := gossh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	if callback == nil {
		callback = gossh.InsecureIgnoreHostKey() //nolint:gosec // in-process authentication test
	}
	return &gossh.ClientConfig{
		User: "mesh", Auth: []gossh.AuthMethod{gossh.PublicKeys(signer)}, HostKeyCallback: callback,
	}
}

func reserveEndpoint(t *testing.T, host string) string {
	t.Helper()
	listener, err := net.Listen("tcp4", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Skipf("cannot bind test address %s: %v", host, err)
	}
	endpoint := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return endpoint
}

func waitForListener(t *testing.T, endpoint string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		connection, err := net.DialTimeout("tcp4", endpoint, 25*time.Millisecond)
		if err == nil {
			_ = connection.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("listener %s did not start", endpoint)
}

func TestMarshalHostKeyUsesOpenSSHFormat(t *testing.T) {
	private := generatePrivateKey(t)
	encoded, signer, err := marshalHostKey(private)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte("BEGIN OPENSSH PRIVATE KEY")) {
		t.Fatalf("host key encoding = %q", encoded)
	}
	expected, err := gossh.NewPublicKey(private.Public())
	if err != nil {
		t.Fatal(err)
	}
	if !charmssh.KeysEqual(signer.PublicKey(), expected) {
		t.Fatal("exported host key differs from source identity")
	}
}
