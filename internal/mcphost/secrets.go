package mcphost

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

const maxSecretBytes = 64 << 10

// ErrSecretFile reports a secret file that fails the ownership rules. Its
// message names the variable or header, never the path or the value.
var ErrSecretFile = errors.New("mcp secret file is not usable")

// readSecret opens path without following a final symlink and accepts only a
// regular file owned by the daemon account with no group or other access.
func readSecret(path string) (string, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		// The bare errno keeps the path out of the message.
		return "", fmt.Errorf("%w: open: %w", ErrSecretFile, err)
	}
	file := os.NewFile(uintptr(fd), "secret")
	defer func() { _ = file.Close() }()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return "", fmt.Errorf("%w: stat: %w", ErrSecretFile, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return "", fmt.Errorf("%w: not a regular file", ErrSecretFile)
	}
	if int(st.Uid) != os.Geteuid() || st.Mode&0o077 != 0 {
		return "", fmt.Errorf("%w: must be owned by the daemon account with mode 0600 or stricter", ErrSecretFile)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxSecretBytes+1))
	if err != nil {
		return "", fmt.Errorf("%w: read: %w", ErrSecretFile, err)
	}
	if len(data) > maxSecretBytes {
		return "", fmt.Errorf("%w: larger than 64 KiB", ErrSecretFile)
	}
	data = bytes.TrimRight(data, "\r\n")
	if bytes.IndexByte(data, 0) >= 0 {
		return "", fmt.Errorf("%w: contains a NUL byte", ErrSecretFile)
	}
	return string(data), nil
}

// launchEnvironment resolves the plain and secret variables for one launch.
func launchEnvironment(d Definition) ([]string, error) {
	env := make([]string, 0, len(d.Env)+len(d.SecretEnv))
	for name, value := range d.Env {
		env = append(env, name+"="+value)
	}
	for name, path := range d.SecretEnv {
		value, err := readSecret(path)
		if err != nil {
			return nil, fmt.Errorf("secret variable %s: %w", name, err)
		}
		env = append(env, name+"="+value)
	}
	return env, nil
}
