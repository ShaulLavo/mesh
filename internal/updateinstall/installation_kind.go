package updateinstall

import (
	"path/filepath"

	"github.com/shaul/mesh/internal/release"
)

const (
	InstallationManaged     = "managed"
	InstallationPackage     = "package"
	InstallationClient      = "client"
	InstallationDevelopment = "development"
	InstallationUnknown     = "unknown"
)

func InstallationKind(executable string, build release.Build, daemon bool) string {
	if _, err := release.CompareVersions(build.Version, build.Version); err != nil || build.Modified {
		return InstallationDevelopment
	}
	if !daemon {
		return InstallationClient
	}
	if validateUnmanagedPrefix(filepath.Clean(executable)) != nil {
		return InstallationPackage
	}
	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return InstallationUnknown
	}
	if validateUnmanagedPrefix(resolved) != nil {
		return InstallationPackage
	}
	if ValidateInstallationPath(executable) != nil {
		return InstallationUnknown
	}
	return InstallationManaged
}
