// Package paths locates Mesh's on-disk state.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
)

// StateDir returns the directory holding per-session state, creating it if
// needed. Unix socket paths are limited to around 100 bytes by the kernel, so
// this stays deliberately short.
func StateDir() (string, error) {
	base := os.Getenv("MESH_STATE_DIR")
	if base == "" {
		if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
			base = filepath.Join(xdg, "mesh")
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("locate home directory: %w", err)
			}
			base = filepath.Join(home, ".local", "state", "mesh")
		}
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", fmt.Errorf("create state dir: %w", err)
	}
	return base, nil
}

// SessionsDir returns the directory containing one subdirectory per session.
func SessionsDir() (string, error) {
	base, err := StateDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "s")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create sessions dir: %w", err)
	}
	return dir, nil
}

// SessionDir returns the directory for one session.
func SessionDir(id string) (string, error) {
	root, err := SessionsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, id), nil
}

// Socket returns the Unix socket path for a session directory.
func Socket(sessionDir string) string { return filepath.Join(sessionDir, "sock") }

// Meta returns the metadata file path for a session directory.
func Meta(sessionDir string) string { return filepath.Join(sessionDir, "meta.json") }

// Log returns the worker log path for a session directory.
func Log(sessionDir string) string { return filepath.Join(sessionDir, "worker.log") }

// Launching marks a reserved session directory that is not yet authoritative.
func Launching(sessionDir string) string { return filepath.Join(sessionDir, ".launching") }
