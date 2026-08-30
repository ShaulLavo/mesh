package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

type sshRemote struct {
	client *ssh.Client
}

func connectSSH(ctx context.Context, remoteTarget target, opts SSHOptions) (remoteHost, error) {
	authMethods, closeAuth, err := loadAuthMethods(ctx, remoteTarget, opts)
	if err != nil {
		return nil, err
	}
	defer closeAuth()
	hostKeyCallback, err := loadHostKeyCallback(ctx, opts)
	if err != nil {
		return nil, err
	}
	timeout := opts.ConnectTimeout
	if timeout == 0 {
		timeout = defaultConnectTimeout
	}

	dialer := net.Dialer{Timeout: timeout}
	networkConnection, err := dialer.DialContext(ctx, "tcp", remoteTarget.address())
	if err != nil {
		return nil, diagnostic(DiagnosticSSHConnect, fmt.Errorf("dial SSH %s: %w", remoteTarget.address(), err))
	}
	stopCancellation := context.AfterFunc(ctx, func() { _ = networkConnection.Close() })
	defer stopCancellation()
	if err := networkConnection.SetDeadline(time.Now().Add(timeout)); err != nil {
		_ = networkConnection.Close()
		return nil, diagnostic(DiagnosticSSHConnect, fmt.Errorf("deadline SSH handshake with %s: %w", remoteTarget.address(), err))
	}

	clientConnection, channels, requests, err := ssh.NewClientConn(networkConnection, remoteTarget.address(), &ssh.ClientConfig{
		User:            remoteTarget.user,
		Auth:            authMethods,
		HostKeyCallback: hostKeyCallback,
	})
	if err != nil {
		_ = networkConnection.Close()
		return nil, classifySSHHandshake(remoteTarget, err)
	}
	if err := networkConnection.SetDeadline(time.Time{}); err != nil {
		_ = clientConnection.Close()
		return nil, diagnostic(DiagnosticSSHConnect, fmt.Errorf("clear SSH deadline with %s: %w", remoteTarget.address(), err))
	}
	return &sshRemote{client: ssh.NewClient(clientConnection, channels, requests)}, nil
}

func loadAuthMethods(ctx context.Context, remoteTarget target, opts SSHOptions) ([]ssh.AuthMethod, func(), error) {
	var signers []ssh.Signer
	var agentConnection net.Conn
	var loadErrors []error
	if socket := os.Getenv("SSH_AUTH_SOCK"); socket != "" {
		connection, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "unix", socket)
		if err != nil {
			loadErrors = append(loadErrors, fmt.Errorf("connect to SSH agent: %w", err))
		} else {
			agentSigners, err := agent.NewClient(connection).Signers()
			if err != nil {
				_ = connection.Close()
				loadErrors = append(loadErrors, fmt.Errorf("list SSH agent keys: %w", err))
			} else if len(agentSigners) > 0 {
				agentConnection = connection
				// Public-key authentication is one SSH method. Keep every agent
				// and file signer in one method so a rejected first source does
				// not prevent later keys from being tried.
				signers = append(signers, agentSigners...)
			} else {
				_ = connection.Close()
			}
		}
	}

	identityFiles := append([]string(nil), opts.IdentityFiles...)
	explicitIdentityFiles := len(identityFiles) > 0
	if !explicitIdentityFiles {
		home, err := os.UserHomeDir()
		if err == nil {
			identityFiles = []string{
				filepath.Join(home, ".ssh", "id_ed25519"),
				filepath.Join(home, ".ssh", "id_ecdsa"),
				filepath.Join(home, ".ssh", "id_rsa"),
			}
		}
	}
	for _, identityFile := range identityFiles {
		signer, err := loadSigner(ctx, identityFile, opts.Passphrase)
		if err == nil {
			signers = append(signers, signer)
			continue
		}
		if !explicitIdentityFiles && errors.Is(err, os.ErrNotExist) {
			continue
		}
		loadErrors = append(loadErrors, err)
	}
	var methods []ssh.AuthMethod
	if len(signers) > 0 {
		methods = append(methods, ssh.PublicKeys(signers...))
	}
	if opts.Password != nil {
		methods = append(methods, ssh.PasswordCallback(func() (string, error) {
			return opts.Password(ctx, remoteTarget.display())
		}))
	}
	closeAuth := func() {
		if agentConnection != nil {
			_ = agentConnection.Close()
		}
	}
	if len(methods) == 0 {
		closeAuth()
		cause := errors.Join(loadErrors...)
		if cause == nil {
			cause = errors.New("no SSH agent keys, identity files, or password callback are available")
		}
		return nil, func() {}, diagnostic(DiagnosticSSHAuth, cause)
	}
	return methods, closeAuth, nil
}

func loadSigner(ctx context.Context, identityFile string, prompt func(context.Context, string) ([]byte, error)) (ssh.Signer, error) {
	contents, err := os.ReadFile(identityFile)
	if err != nil {
		return nil, fmt.Errorf("read SSH identity %s: %w", identityFile, err)
	}
	signer, err := ssh.ParsePrivateKey(contents)
	if err == nil {
		return signer, nil
	}
	var missing *ssh.PassphraseMissingError
	if !errors.As(err, &missing) || prompt == nil {
		return nil, fmt.Errorf("parse SSH identity %s: %w", identityFile, err)
	}
	passphrase, promptErr := prompt(ctx, identityFile)
	if promptErr != nil {
		return nil, fmt.Errorf("read passphrase for SSH identity %s: %w", identityFile, promptErr)
	}
	signer, err = ssh.ParsePrivateKeyWithPassphrase(contents, passphrase)
	if err != nil {
		return nil, fmt.Errorf("decrypt SSH identity %s: %w", identityFile, err)
	}
	return signer, nil
}

func loadHostKeyCallback(ctx context.Context, opts SSHOptions) (ssh.HostKeyCallback, error) {
	knownHostsPath := opts.KnownHostsPath
	if knownHostsPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, diagnostic(DiagnosticSSHHostKey, fmt.Errorf("locate ~/.ssh/known_hosts: %w", err))
		}
		knownHostsPath = filepath.Join(home, ".ssh", "known_hosts")
	}

	var knownCallback ssh.HostKeyCallback
	knownCallback, err := knownhosts.New(knownHostsPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, diagnostic(DiagnosticSSHHostKey, fmt.Errorf("read known hosts %s: %w", knownHostsPath, err))
	}
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		if knownCallback != nil {
			err := knownCallback(hostname, remote, key)
			if err == nil {
				return nil
			}
			var keyError *knownhosts.KeyError
			if !errors.As(err, &keyError) || len(keyError.Want) > 0 {
				return diagnostic(DiagnosticSSHHostKey, err)
			}
		}
		fingerprint := ssh.FingerprintSHA256(key)
		if opts.ConfirmHostKey == nil {
			return diagnostic(DiagnosticSSHHostKey, fmt.Errorf("host %s is unknown; presented %s %s", hostname, key.Type(), fingerprint))
		}
		accepted, err := opts.ConfirmHostKey(ctx, HostKey{
			Host:        hostname,
			Algorithm:   key.Type(),
			Fingerprint: fingerprint,
		})
		if err != nil {
			return diagnostic(DiagnosticSSHHostKey, fmt.Errorf("confirm host key for %s: %w", hostname, err))
		}
		if !accepted {
			return diagnostic(DiagnosticSSHHostKey, fmt.Errorf("host key for %s was not accepted", hostname))
		}
		if err := appendKnownHost(knownHostsPath, hostname, key); err != nil {
			return diagnostic(DiagnosticSSHHostKey, err)
		}
		return nil
	}, nil
}

func appendKnownHost(knownHostsPath, hostname string, key ssh.PublicKey) error {
	if info, err := os.Lstat(knownHostsPath); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("known hosts path %s is not a regular file", knownHostsPath)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect known hosts path %s: %w", knownHostsPath, err)
	}
	if err := os.MkdirAll(filepath.Dir(knownHostsPath), 0o700); err != nil {
		return fmt.Errorf("create known hosts directory: %w", err)
	}
	file, err := os.OpenFile(knownHostsPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("open known hosts %s: %w", knownHostsPath, err)
	}
	line := knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key) + "\n"
	_, writeErr := io.WriteString(file, line)
	closeErr := file.Close()
	if writeErr != nil {
		return fmt.Errorf("write known hosts %s: %w", knownHostsPath, writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close known hosts %s: %w", knownHostsPath, closeErr)
	}
	return nil
}

func classifySSHHandshake(remoteTarget target, err error) error {
	var diagnosticError *DiagnosticError
	if errors.As(err, &diagnosticError) {
		return diagnosticError
	}
	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "host key") || strings.Contains(lower, "knownhosts") {
		return diagnostic(DiagnosticSSHHostKey, fmt.Errorf("verify SSH host %s: %w", remoteTarget.display(), err))
	}
	return diagnostic(DiagnosticSSHAuth, fmt.Errorf("authenticate SSH host %s: %w", remoteTarget.display(), err))
}

func (r *sshRemote) Run(ctx context.Context, command string, stdin io.Reader) ([]byte, []byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	session, err := r.client.NewSession()
	if err != nil {
		return nil, nil, fmt.Errorf("open SSH session: %w", err)
	}
	defer session.Close()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr
	session.Stdin = stdin
	done := make(chan error, 1)
	go func() { done <- session.Run(command) }()
	select {
	case err := <-done:
		return stdout.Bytes(), stderr.Bytes(), err
	case <-ctx.Done():
		_ = session.Close()
		<-done
		return stdout.Bytes(), stderr.Bytes(), ctx.Err()
	}
}

func (r *sshRemote) Close() error {
	return r.client.Close()
}
