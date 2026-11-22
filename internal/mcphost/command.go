package mcphost

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// findCommand checks that the server's command exists and is executable
// before any worker is reserved. A missing command is then reported the same
// way on every platform, instead of through a worker that fails after it
// started, whose outcome is uncertain, or a shell's platform-specific exit
// status.
func findCommand(argv []string, cwd string, env map[string]string) error {
	if len(argv) == 0 {
		return invalid("no command")
	}
	name := argv[0]
	if strings.ContainsRune(name, '/') {
		if !filepath.IsAbs(name) {
			name = filepath.Join(cwd, name)
		}
		return executable(argv[0], name)
	}
	path, ok := env["PATH"]
	if !ok {
		path = os.Getenv("PATH")
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			dir = "."
		}
		if executable(name, filepath.Join(dir, name)) == nil {
			return nil
		}
	}
	return fmt.Errorf("the server command %s was not found on this host", argv[0])
}

func executable(shown, path string) error {
	info, err := os.Stat(path) //nolint:gosec // The administrator's own published command, checked before it runs.
	if err != nil || info.IsDir() {
		return fmt.Errorf("the server command %s was not found on this host", shown)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("the server command %s is not executable", shown)
	}
	return nil
}
