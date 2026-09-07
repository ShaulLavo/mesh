package updatebootstrap

import (
	"os"

	"github.com/shaul/mesh/internal/updateinstall"
)

// lsof may name either hard link, including the backup, for a loaded vnode.
// The recorded install path is usable only while it names that same image.
func activeInstallationPath(stateDir string, image executableImage) string {
	status, err := updateinstall.Read(stateDir)
	if err != nil {
		return image.Installed
	}
	loaded, err := os.Stat(image.Path)
	if err != nil {
		return image.Installed
	}
	installed, err := os.Stat(status.Settings.Executable)
	if err == nil && os.SameFile(loaded, installed) {
		return status.Settings.Executable
	}
	return image.Installed
}
