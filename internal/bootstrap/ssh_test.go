package bootstrap

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestConnectSSHAuthenticatesAndRunsCommands(t *testing.T) {
	t.Parallel()

	clientPrivate, clientPublic := generateSSHKey(t)
	server := startSSHTestServer(t, clientPublic)
	identityPath := writeSSHIdentity(t, clientPrivate)
	knownHostsPath := writeKnownHost(t, server.address, server.hostKey)
	remoteTarget, err := parseTarget("shaul@" + server.address)
	if err != nil {
		t.Fatal(err)
	}

	remote, err := connectSSH(context.Background(), remoteTarget, SSHOptions{
		KnownHostsPath: knownHostsPath,
		IdentityFiles:  []string{identityPath},
	})
	if err != nil {
		t.Fatalf("connectSSH() error = %v", err)
	}
	stdout, stderr, err := remote.Run(context.Background(), "uname -s", nil)
	if err != nil {
		t.Fatalf("Run() error = %v, stderr = %q", err, stderr)
	}
	if string(stdout) != "ran:uname -s" {
		t.Fatalf("stdout = %q", stdout)
	}
	if err := remote.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestConnectSSHNamesAuthenticationFailure(t *testing.T) {
	t.Parallel()

	_, authorizedPublic := generateSSHKey(t)
	wrongPrivate, _ := generateSSHKey(t)
	server := startSSHTestServer(t, authorizedPublic)
	identityPath := writeSSHIdentity(t, wrongPrivate)
	knownHostsPath := writeKnownHost(t, server.address, server.hostKey)
	remoteTarget, err := parseTarget("shaul@" + server.address)
	if err != nil {
		t.Fatal(err)
	}

	_, err = connectSSH(context.Background(), remoteTarget, SSHOptions{
		KnownHostsPath: knownHostsPath,
		IdentityFiles:  []string{identityPath},
	})
	assertDiagnosticCode(t, err, DiagnosticSSHAuth)
}

func TestConnectSSHNamesUnreachableHost(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	clientPrivate, _ := generateSSHKey(t)
	remoteTarget, err := parseTarget("shaul@" + address)
	if err != nil {
		t.Fatal(err)
	}
	_, err = connectSSH(context.Background(), remoteTarget, SSHOptions{
		KnownHostsPath: filepath.Join(t.TempDir(), "known_hosts"),
		IdentityFiles:  []string{writeSSHIdentity(t, clientPrivate)},
	})
	assertDiagnosticCode(t, err, DiagnosticSSHConnect)
}

func TestConnectSSHRefusesUnknownHostWithoutConfirmation(t *testing.T) {
	t.Parallel()

	clientPrivate, clientPublic := generateSSHKey(t)
	server := startSSHTestServer(t, clientPublic)
	remoteTarget, err := parseTarget("shaul@" + server.address)
	if err != nil {
		t.Fatal(err)
	}
	_, err = connectSSH(context.Background(), remoteTarget, SSHOptions{
		KnownHostsPath: filepath.Join(t.TempDir(), "known_hosts"),
		IdentityFiles:  []string{writeSSHIdentity(t, clientPrivate)},
	})
	assertDiagnosticCode(t, err, DiagnosticSSHHostKey)
}

func TestConnectSSHTriesEveryIdentityFile(t *testing.T) {
	t.Parallel()

	wrongPrivate, _ := generateSSHKey(t)
	clientPrivate, clientPublic := generateSSHKey(t)
	server := startSSHTestServer(t, clientPublic)
	knownHostsPath := writeKnownHost(t, server.address, server.hostKey)
	remoteTarget, err := parseTarget("shaul@" + server.address)
	if err != nil {
		t.Fatal(err)
	}

	remote, err := connectSSH(context.Background(), remoteTarget, SSHOptions{
		KnownHostsPath: knownHostsPath,
		IdentityFiles:  []string{writeSSHIdentity(t, wrongPrivate), writeSSHIdentity(t, clientPrivate)},
	})
	if err != nil {
		t.Fatalf("connectSSH() error = %v", err)
	}
	if err := remote.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestConnectSSHConfirmsAndRecordsUnknownHost(t *testing.T) {
	t.Parallel()

	clientPrivate, clientPublic := generateSSHKey(t)
	server := startSSHTestServer(t, clientPublic)
	identityPath := writeSSHIdentity(t, clientPrivate)
	knownHostsPath := filepath.Join(t.TempDir(), ".ssh", "known_hosts")
	remoteTarget, err := parseTarget("shaul@" + server.address)
	if err != nil {
		t.Fatal(err)
	}
	var prompted HostKey
	remote, err := connectSSH(context.Background(), remoteTarget, SSHOptions{
		KnownHostsPath: knownHostsPath,
		IdentityFiles:  []string{identityPath},
		ConfirmHostKey: func(_ context.Context, key HostKey) (bool, error) {
			prompted = key
			return true, nil
		},
	})
	if err != nil {
		t.Fatalf("connectSSH() error = %v", err)
	}
	_ = remote.Close()
	if prompted.Fingerprint != ssh.FingerprintSHA256(server.hostKey) || prompted.Algorithm != server.hostKey.Type() {
		t.Fatalf("prompted host key = %#v", prompted)
	}
	contents, err := os.ReadFile(knownHostsPath) //nolint:gosec // test reads its own temporary known-hosts fixture
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), server.hostKey.Type()) {
		t.Fatalf("known_hosts = %q", contents)
	}
}

type sshTestServer struct {
	address  string
	hostKey  ssh.PublicKey
	listener net.Listener
	wg       sync.WaitGroup
}

func startSSHTestServer(t *testing.T, authorized ssh.PublicKey) *sshTestServer {
	t.Helper()
	_, hostPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPrivate)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if bytes.Equal(key.Marshal(), authorized.Marshal()) {
				return nil, nil
			}
			return nil, errors.New("unauthorized key")
		},
	}
	config.AddHostKey(hostSigner)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &sshTestServer{
		address:  listener.Addr().String(),
		hostKey:  hostSigner.PublicKey(),
		listener: listener,
	}
	server.wg.Add(1)
	go server.accept(config)
	t.Cleanup(func() {
		_ = listener.Close()
		server.wg.Wait()
	})
	return server
}

func (s *sshTestServer) accept(config *ssh.ServerConfig) {
	defer s.wg.Done()
	for {
		connection, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer connection.Close() //nolint:errcheck // test connection cleanup
			serverConnection, channels, requests, err := ssh.NewServerConn(connection, config)
			if err != nil {
				return
			}
			defer serverConnection.Close() //nolint:errcheck // test SSH connection cleanup
			go ssh.DiscardRequests(requests)
			for channelRequest := range channels {
				if channelRequest.ChannelType() != "session" {
					_ = channelRequest.Reject(ssh.UnknownChannelType, "session channel required")
					continue
				}
				channel, requests, err := channelRequest.Accept()
				if err != nil {
					continue
				}
				go serveSSHTestSession(channel, requests)
			}
		}()
	}
}

func serveSSHTestSession(channel ssh.Channel, requests <-chan *ssh.Request) {
	defer channel.Close() //nolint:errcheck // test channel cleanup
	for request := range requests {
		if request.Type != "exec" {
			_ = request.Reply(false, nil)
			continue
		}
		var payload struct{ Command string }
		if err := ssh.Unmarshal(request.Payload, &payload); err != nil {
			_ = request.Reply(false, nil)
			return
		}
		_ = request.Reply(true, nil)
		_, _ = fmt.Fprintf(channel, "ran:%s", payload.Command)
		_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{Status: 0}))
		return
	}
}

func generateSSHKey(t *testing.T) (ed25519.PrivateKey, ssh.PublicKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshPublic, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	return private, sshPublic
}

func writeSSHIdentity(t *testing.T, private ed25519.PrivateKey) string {
	t.Helper()
	encoded, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "id_ed25519")
	contents := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded})
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeKnownHost(t *testing.T, address string, key ssh.PublicKey) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "known_hosts")
	line := knownhosts.Line([]string{knownhosts.Normalize(address)}, key) + "\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
