// Package macapp runs the macOS daemon from a Mesh.app bundle it builds in the
// state directory. System Settings shows a permission holder's icon and name
// only when the binary sits in an app bundle; the installed binary stays a
// plain file so the installers, launchd plist and updater need not change.
package macapp

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	bundleName = "Mesh.app"
	// Holds the installed binary's path, so code running from the bundle can
	// find the file the updater owns.
	installedMarker = "installed-executable"
)

var executableSuffix = filepath.Join(bundleName, "Contents", "MacOS", "mesh")

// Installed returns the path of the installed mesh binary: the running
// executable, or the file it was copied from when running inside Mesh.app.
func Installed() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve running executable: %w", err)
	}
	return installedFrom(executable), nil
}

func installedFrom(executable string) string {
	if !strings.HasSuffix(executable, string(filepath.Separator)+executableSuffix) {
		return executable
	}
	marker := filepath.Join(filepath.Dir(filepath.Dir(executable)), "Resources", installedMarker)
	data, err := os.ReadFile(marker) //nolint:gosec // the marker sits beside the running executable inside its own bundle
	if err != nil || len(bytes.TrimSpace(data)) == 0 {
		return executable
	}
	return string(bytes.TrimSpace(data))
}

// Bundle describes the Mesh.app to build.
type Bundle struct {
	Dir        string // directory that holds Mesh.app
	Executable string // installed binary copied into the bundle
	Version    string
	Icon       []byte
	// Sign seals the finished bundle; a bundle edited after signing fails
	// code-signature checks.
	Sign func(bundle string) error
}

// BundleExecutable returns the binary path inside the bundle.
func (b Bundle) BundleExecutable() string { return filepath.Join(b.Dir, executableSuffix) }

// Sync rebuilds Mesh.app when its binary or recorded install path differs
// from b, and reports whether it rebuilt.
func Sync(b Bundle) (bool, error) {
	clean(b.Dir)
	current, err := upToDate(b)
	if err != nil {
		return false, err
	}
	if current {
		return false, nil
	}
	staged := filepath.Join(b.Dir, fmt.Sprintf("%s.new-%d", bundleName, os.Getpid()))
	if err := build(b, staged); err != nil {
		_ = os.RemoveAll(staged)
		return false, err
	}
	return true, swap(staged, filepath.Join(b.Dir, bundleName))
}

// A missing or unreadable bundle is stale, not an error: Sync rebuilds it.
func upToDate(b Bundle) (bool, error) {
	marker, markerErr := os.ReadFile(filepath.Join(b.Dir, bundleName, "Contents", "Resources", installedMarker))
	if markerErr != nil || string(bytes.TrimSpace(marker)) != b.Executable {
		return false, nil //nolint:nilerr // see above
	}
	want, err := digest(b.Executable)
	if err != nil {
		return false, fmt.Errorf("hash installed mesh %s: %w", b.Executable, err)
	}
	have, haveErr := digest(b.BundleExecutable())
	if haveErr != nil {
		return false, nil //nolint:nilerr // see above
	}
	return bytes.Equal(want, have), nil
}

func build(b Bundle, root string) error {
	contents := filepath.Join(root, "Contents")
	for _, dir := range []string{"MacOS", "Resources"} {
		if err := os.MkdirAll(filepath.Join(contents, dir), 0o700); err != nil {
			return fmt.Errorf("create bundle directory %s: %w", dir, err)
		}
	}
	if err := copyFile(b.Executable, filepath.Join(contents, "MacOS", "mesh"), 0o700); err != nil {
		return err
	}
	files := map[string][]byte{
		"Info.plist":                                []byte(infoPlist(b.Version)),
		filepath.Join("Resources", "mesh.icns"):     b.Icon,
		filepath.Join("Resources", installedMarker): []byte(b.Executable + "\n"),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(contents, name), data, 0o600); err != nil {
			return fmt.Errorf("write bundle %s: %w", name, err)
		}
	}
	if b.Sign == nil {
		return nil
	}
	if err := b.Sign(root); err != nil {
		return fmt.Errorf("sign %s: %w", root, err)
	}
	return nil
}

// Running workers keep the old binary mapped after its directory is renamed
// and removed, so a rebuild never disturbs live sessions.
func swap(staged, target string) error {
	old := fmt.Sprintf("%s.old-%d", target, os.Getpid())
	if err := os.Rename(target, old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("move aside %s: %w", target, err)
	}
	if err := os.Rename(staged, target); err != nil {
		return fmt.Errorf("install %s: %w", target, err)
	}
	_ = os.RemoveAll(old)
	return nil
}

// clean removes staging and retired bundles a crash left behind.
func clean(dir string) {
	for _, pattern := range []string{bundleName + ".new-*", bundleName + ".old-*"} {
		matches, _ := filepath.Glob(filepath.Join(dir, pattern))
		for _, match := range matches {
			_ = os.RemoveAll(match)
		}
	}
}

func copyFile(from, to string, mode os.FileMode) error {
	source, err := os.Open(from) //nolint:gosec // the installed mesh binary this process runs as
	if err != nil {
		return fmt.Errorf("open installed mesh %s: %w", from, err)
	}
	defer func() { _ = source.Close() }()
	target, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode) //nolint:gosec // the binary slot inside the bundle being staged
	if err != nil {
		return fmt.Errorf("create bundle binary %s: %w", to, err)
	}
	if _, err := io.Copy(target, source); err != nil {
		_ = target.Close()
		return fmt.Errorf("copy mesh into bundle: %w", err)
	}
	if err := target.Close(); err != nil {
		return fmt.Errorf("close bundle binary %s: %w", to, err)
	}
	return nil
}

func digest(path string) ([]byte, error) {
	file, err := os.Open(path) //nolint:gosec // the installed binary or its copy inside the bundle
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return hash.Sum(nil), nil
}

var numericVersion = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*$`)

func infoPlist(version string) string {
	// Launch Services accepts only dotted numbers here; dev builds report 0.
	short := strings.TrimPrefix(version, "v")
	if !numericVersion.MatchString(short) {
		short = "0"
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleIdentifier</key><string>dev.shaulavo.mesh</string>
	<key>CFBundleName</key><string>Mesh</string>
	<key>CFBundleDisplayName</key><string>Mesh</string>
	<key>CFBundleExecutable</key><string>mesh</string>
	<key>CFBundleIconFile</key><string>mesh</string>
	<key>CFBundlePackageType</key><string>APPL</string>
	<key>CFBundleInfoDictionaryVersion</key><string>6.0</string>
	<key>CFBundleShortVersionString</key><string>` + short + `</string>
	<key>CFBundleVersion</key><string>` + short + `</string>
	<key>LSUIElement</key><true/>
</dict>
</plist>
`
}
