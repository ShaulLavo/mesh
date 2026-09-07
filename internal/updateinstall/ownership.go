package updateinstall

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ValidateInstallationPath rejects managed package payloads until an adapter can
// install the exact approved package version through its owning package manager.
func ValidateInstallationPath(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("installation executable path must be absolute")
	}
	clean := filepath.Clean(path)
	if err := validateUnmanagedPrefix(clean); err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return err
	}
	if err = validateUnmanagedPrefix(resolved); err != nil {
		return err
	}
	info, err := os.Lstat(clean)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("updater requires a regular Mesh-managed executable; migrate package-owned links explicitly")
	}
	return nil
}

func validateUnmanagedPrefix(clean string) error {
	for _, prefix := range []string{"/usr/", "/bin/", "/sbin/", "/nix/", "/snap/", "/opt/homebrew/", "/home/linuxbrew/"} {
		if strings.HasPrefix(clean, prefix) {
			return fmt.Errorf("package-managed Mesh executable %s requires its package manager or an explicitly approved migration to a Mesh-managed installation", clean)
		}
	}
	if strings.Contains(clean, "/Cellar/") || strings.Contains(clean, "/Caskroom/") {
		return errors.New("Homebrew payload requires an exact-version package adapter or an explicitly approved installation migration")
	}
	return nil
}
