package bootstrap

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"
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
	if resolve != nil {
		opts.KnownHostsPath, err = systemKnownHostsPath(resolve(remoteTarget.alias, "UserKnownHostsFile"))
		if err != nil {
			return nil, nil, err
		}
	}
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
		// The remote's own diagnostic is the only record of why a step failed;
		// an exit status alone cannot be acted on.
		return stdout, stderr, remoteCommandError("run system SSH command", err, stdout, stderr)
	}
	return stdout, stderr, nil
}

// SystemSSHPlatform runs command over the configured system SSH alias and reads
// the remote OS and architecture from its output, with the same parser mesh add
// uses. command prints exactly `uname -s` and `uname -m` on stdout; any further
// checks report on stderr and refuse with a nonzero exit.
func SystemSSHPlatform(ctx context.Context, alias, command string) (Platform, error) {
	stdout, _, err := RunSystemSSH(ctx, alias, command, nil)
	if err != nil {
		return Platform{}, fmt.Errorf("detect remote platform: %w", err)
	}
	return parsePlatform(stdout)
}

func systemKnownHostsPath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "none" || strings.ContainsAny(value, "\r\n\x00") {
		return "", fmt.Errorf("system SSH requires an existing known-hosts file")
	}
	if value == "" {
		return "", nil
	}
	if strings.HasPrefix(value, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate configured system SSH known hosts: %w", err)
		}
		value = filepath.Join(home, strings.TrimPrefix(value, "~/"))
	}
	if !filepath.IsAbs(value) {
		return "", fmt.Errorf("configured system SSH known hosts must use one absolute file path")
	}
	return value, nil
}
