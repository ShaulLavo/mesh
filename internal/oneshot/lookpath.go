package oneshot

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LookPath resolves file the way a worker started with env resolves it: a
// name with a slash is used as given, and a bare name is searched in env's
// PATH, not the daemon's. Relative PATH entries are skipped, as exec refuses
// what they find.
func LookPath(file string, env []string) (string, error) {
	if strings.Contains(file, "/") {
		return file, nil
	}
	for _, dir := range filepath.SplitList(pathOf(env)) {
		if !filepath.IsAbs(dir) {
			continue
		}
		candidate := filepath.Join(dir, file)
		if executable(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("executable %q not found in PATH", file)
}

func pathOf(env []string) string {
	path := ""
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, "PATH="); ok {
			path = value
		}
	}
	return path
}

func executable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}
