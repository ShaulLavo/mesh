package bootstrap

import (
	"context"
	"fmt"
	"io"
	"os/user"
	"time"
)

// RunSystemSSH uses the account's configured SSH keys and known hosts. It never
// provisions access, offers a prompt, or falls back to a Mesh session.
func RunSystemSSH(ctx context.Context, alias, command string, stdin io.Reader) ([]byte, []byte, error) {
	remoteTarget, err := parseTarget(alias)
	if err != nil {
		return nil, nil, err
	}
	resolve := userSSHConfig()
	remoteTarget = applySSHConfig(remoteTarget, resolve)
	if remoteTarget.user == "" {
		current, err := user.Current()
		if err != nil {
			return nil, nil, fmt.Errorf("resolve system SSH account: %w", err)
		}
		remoteTarget.user, remoteTarget.guessedUser = current.Username, true
	}
	opts := SSHOptions{ConnectTimeout: 10 * time.Second}
	if file := sshConfigIdentityFile(remoteTarget.alias, resolve); file != "" {
		opts.IdentityFiles = []string{file}
	}
	remote, err := connectSSH(ctx, remoteTarget, opts)
	if err != nil {
		return nil, nil, fmt.Errorf("system SSH %s: %w", alias, err)
	}
	defer func() { _ = remote.Close() }()
	stdout, stderr, err := remote.Run(ctx, command, stdin, remoteOutputLimits{Stdout: 64 << 10, Stderr: 16 << 10})
	if err != nil {
		return stdout, stderr, fmt.Errorf("run system SSH command: %w", err)
	}
	return stdout, stderr, nil
}
