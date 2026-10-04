package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/identity"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestApproveFleetSystemSSHRequiresExistingHostKeyTrust(t *testing.T) {
	systemSSH, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("native system SSH is unavailable")
	}
	fixture := createFleetFixture(t, 1)
	hostKeyState := t.TempDir()
	_, private, err := identity.LoadOrCreate(hostKeyState)
	if err != nil {
		t.Fatal(err)
	}
	hostKey, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		public, err := identity.IdentityKey(fixture.Source.Destination)
		if err != nil {
			return nil, fmt.Errorf("read fixture public key: %w", err)
		}
		expected, err := ssh.NewPublicKey(public)
		if err != nil || !bytes.Equal(expected.Marshal(), key.Marshal()) {
			return nil, errors.New("fixture SSH key mismatch")
		}
		return &ssh.Permissions{}, nil
	}}
	config.AddHostKey(hostKey)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	defer func() { _ = listener.Close() }()
	go serveEnrollmentSSH(ctx, listener, config)
	destination := fixture.Labels[0] + "-admin"
	files := t.TempDir()
	known := filepath.Join(files, "known_hosts")
	sshConfig := filepath.Join(files, "config")
	port := listener.Addr().(*net.TCPAddr).Port
	settings := fmt.Sprintf("Host %s\n HostName 127.0.0.1\n Port %d\n User fixture\n IdentityAgent none\n IdentitiesOnly yes\n IdentityFile %s\n UserKnownHostsFile %s\n GlobalKnownHostsFile /dev/null\n", destination, port, filepath.Join(fixture.Source.StateDir, "identity.key"), known)
	requireApprovalMutation(t, os.WriteFile(sshConfig, []byte(settings), 0600))
	fixtureSSH, err := exec.LookPath("ssh")
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nexec '" + strings.ReplaceAll(systemSSH, "'", "'\"'\"'") + "' -F '" + strings.ReplaceAll(sshConfig, "'", "'\"'\"'") + "' \"$@\"\n"
	requireApprovalMutation(t, os.WriteFile(fixtureSSH, []byte(script), 0700)) //nolint:gosec // private shim forwards only to an isolated loopback SSH server
	if _, err := executeFleetFixture(t, fixture, "--yes"); err == nil || !strings.Contains(err.Error(), "Host key verification failed") {
		t.Fatalf("unknown fixture host key was accepted: %v", err)
	}
	if _, err := os.Stat(known); !os.IsNotExist(err) {
		t.Fatal("system SSH created or accepted unknown host-key trust")
	}
	if identity.GrantedIdentity(fixture.Destinations[0].StateDir, fixture.Source.Destination) {
		t.Fatal("unknown SSH host key authorized enrollment")
	}
	trusted := knownhosts.Line([]string{net.JoinHostPort("127.0.0.1", strconv.Itoa(port))}, hostKey.PublicKey()) + "\n"
	requireApprovalMutation(t, os.WriteFile(known, []byte(trusted), 0600))
	output, err := executeFleetFixture(t, fixture, "--yes")
	if err != nil || !strings.Contains(output, ": approved") {
		t.Fatalf("existing trusted fixture SSH key did not permit checked enrollment: %s %v", output, err)
	}
}

func serveEnrollmentSSH(ctx context.Context, listener net.Listener, config *ssh.ServerConfig) {
	for {
		stream, err := listener.Accept()
		if err != nil {
			return
		}
		go serveEnrollmentSSHConnection(ctx, stream, config)
	}
}

func serveEnrollmentSSHConnection(ctx context.Context, stream net.Conn, config *ssh.ServerConfig) {
	defer func() { _ = stream.Close() }()
	_ = stream.SetDeadline(time.Now().Add(10 * time.Second))
	connection, channels, requests, err := ssh.NewServerConn(stream, config)
	if err != nil {
		return
	}
	defer func() { _ = connection.Close() }()
	go ssh.DiscardRequests(requests)
	for channel := range channels {
		if channel.ChannelType() != "session" {
			_ = channel.Reject(ssh.UnknownChannelType, "fixture only accepts exec sessions")
			continue
		}
		accepted, operations, err := channel.Accept()
		if err != nil {
			return
		}
		serveEnrollmentSSHExec(ctx, accepted, operations)
	}
}

func serveEnrollmentSSHExec(ctx context.Context, channel ssh.Channel, requests <-chan *ssh.Request) {
	defer func() { _ = channel.Close() }()
	for request := range requests {
		var payload struct{ Command string }
		if request.Type != "exec" || ssh.Unmarshal(request.Payload, &payload) != nil {
			_ = request.Reply(false, nil)
			continue
		}
		_ = request.Reply(true, nil)
		command := exec.CommandContext(ctx, "/bin/sh", "-c", payload.Command) //nolint:gosec // fixture SSH server executes the tested shell-quoted enrollment command in isolated state
		command.Stdout, command.Stderr = channel, channel.Stderr()
		status := uint32(0)
		if command.Run() != nil {
			status = 1
		}
		_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
		return
	}
}
