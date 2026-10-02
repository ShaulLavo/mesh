package macapp

import (
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"syscall"
)

//go:embed assets/mesh.icns
var icon []byte

// Enter rebuilds Mesh.app in stateDir if needed and re-executes the daemon
// from it, keeping the process ID launchd tracks. It returns only when the
// process stays where it is: off macOS, already inside the bundle, or on an
// error the caller reports before running unbundled.
func Enter(stateDir, version string, args []string) error {
	// A runtime check, not a build tag, so the bundle code stays compiled and
	// tested on Linux CI.
	if runtime.GOOS != "darwin" {
		return nil
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate running mesh: %w", err)
	}
	if installedFrom(executable) != executable {
		return nil
	}
	bundle := Bundle{Dir: stateDir, Executable: executable, Version: version, Icon: icon, Sign: codesign}
	if _, err := Sync(bundle); err != nil {
		return err
	}
	target := bundle.BundleExecutable()
	if err := syscall.Exec(target, append([]string{target}, args[1:]...), os.Environ()); err != nil { //nolint:gosec // this binary's own copy, rebuilt and signed above
		return fmt.Errorf("re-execute daemon from %s: %w", target, err)
	}
	return nil
}

func codesign(bundle string) error {
	output, err := exec.Command("/usr/bin/codesign", "--force", "--sign", "-", bundle).CombinedOutput() //nolint:gosec // fixed system tool signing the bundle Sync staged
	if err != nil {
		return fmt.Errorf("codesign: %w: %s", err, output)
	}
	return nil
}
