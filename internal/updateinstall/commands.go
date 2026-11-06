package updateinstall

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// CommandCopy is a separate installation that a shell can select instead of
// the executable maintained by Mesh's updater.
type CommandCopy struct {
	Path   string `json:"path"`
	Target string `json:"target"`
	Owner  string `json:"owner"`
}

// CommandSearchDirs includes common command locations because daemon and login
// shell PATHs can differ. Empty and relative entries are ignored by OtherCommands.
func CommandSearchDirs(home, pathEnv string) []string {
	dirs := filepath.SplitList(pathEnv)
	return append(dirs, "/opt/homebrew/bin", "/usr/local/bin", "/home/linuxbrew/.linuxbrew/bin",
		"/usr/bin", "/snap/bin", filepath.Join(home, ".linuxbrew", "bin"),
		filepath.Join(home, ".nix-profile", "bin"), filepath.Join(home, "go", "bin"),
		filepath.Join(home, "bin"), filepath.Join(home, ".local", "bin"))
}

// OtherCommands inspects paths without executing discovered files. Hardlinks
// at different paths remain separate: atomic updates replace only one pathname.
func OtherCommands(managed string, dirs []string) []CommandCopy {
	resolved, err := filepath.EvalSymlinks(managed)
	if err != nil {
		resolved = filepath.Clean(managed)
	}
	seen := map[string]bool{resolved: true}
	var copies []CommandCopy
	for _, dir := range dirs {
		command, ok := inspectCommand(dir)
		if !ok || seen[command.Target] {
			continue
		}
		seen[command.Target] = true
		copies = append(copies, command)
	}
	sort.Slice(copies, func(i, j int) bool { return copies[i].Path < copies[j].Path })
	return copies
}

func inspectCommand(dir string) (CommandCopy, bool) {
	if !filepath.IsAbs(dir) {
		return CommandCopy{}, false
	}
	path := filepath.Join(dir, "mesh")
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return CommandCopy{}, false
	}
	info, err := os.Stat(target)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return CommandCopy{}, false
	}
	return CommandCopy{Path: path, Target: target, Owner: commandOwner(target)}, true
}

func commandOwner(target string) string {
	switch {
	case strings.Contains(target, "/Caskroom/mesh/"):
		return "homebrew-cask"
	case strings.Contains(target, "/Cellar/mesh/"):
		return "homebrew-formula"
	case validateUnmanagedPrefix(target) != nil:
		return "package"
	default:
		return "unknown"
	}
}

// Remedy describes how to maintain an extra copy without changing its files.
func (c CommandCopy) Remedy() string {
	switch c.Owner {
	case "homebrew-cask":
		return "Update it with brew upgrade --cask shaullavo/mesh/mesh, or remove it with brew uninstall --cask shaullavo/mesh/mesh and put the managed Mesh directory first in every shell's PATH."
	case "homebrew-formula":
		return "Update or remove it with Homebrew, or put the managed Mesh directory first in every shell's PATH."
	case "package":
		return "Update or remove it with its package manager, or put the managed Mesh directory first in every shell's PATH."
	default:
		return "Remove the extra command or put the managed Mesh directory first in every shell's PATH."
	}
}

// CommandPath makes commands started by a daemon prefer that installation.
func CommandPath(executable, pathEnv string) string {
	dir := filepath.Dir(executable)
	dirs := []string{dir}
	for _, entry := range filepath.SplitList(pathEnv) {
		if entry != dir {
			dirs = append(dirs, entry)
		}
	}
	return strings.Join(dirs, string(os.PathListSeparator))
}
