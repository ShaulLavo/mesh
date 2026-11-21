package paths

import (
	"errors"
	"path/filepath"
)

// UpdateCacheDir is where a Mesh process with environment env on goos keeps
// downloaded updates. It follows os.UserCacheDir, which the daemon uses, so a
// caller reading another process's environment lands in that daemon's cache:
// macOS ignores XDG_CACHE_HOME, and an empty variable counts as unset.
func UpdateCacheDir(goos string, env func(string) string) (string, error) {
	if configured := env("MESH_UPDATE_CACHE_DIR"); configured != "" {
		return configured, nil
	}
	home := env("HOME")
	if goos == "darwin" || goos == "ios" {
		if home == "" {
			return "", errors.New("update cache: $HOME is not defined")
		}
		return filepath.Join(home, "Library", "Caches", "mesh", "updates"), nil
	}
	if xdg := env("XDG_CACHE_HOME"); xdg != "" {
		if !filepath.IsAbs(xdg) {
			return "", errors.New("update cache: path in $XDG_CACHE_HOME is relative")
		}
		return filepath.Join(xdg, "mesh", "updates"), nil
	}
	if home == "" {
		return "", errors.New("update cache: neither $XDG_CACHE_HOME nor $HOME are defined")
	}
	return filepath.Join(home, ".cache", "mesh", "updates"), nil
}

// UpdateCacheShell sets cache to UpdateCacheDir's answer for the shell's own
// environment. Scripts that run on a remote account embed it rather than
// restating the rule; TestUpdateCacheShellMatchesGo keeps the two equal.
const UpdateCacheShell = `case "$(uname -s)" in
  Darwin) cache_home=$HOME/Library/Caches ;;
  *) cache_home=${XDG_CACHE_HOME:-$HOME/.cache} ;;
esac
cache=${MESH_UPDATE_CACHE_DIR:-$cache_home/mesh/updates}
`
