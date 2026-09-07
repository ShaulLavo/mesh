package updateinstall

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type HelperConfig struct {
	StateDir   string
	Executable string
	ServiceDir string
	Kind       string
	Domain     string
}

type HelperInstallation struct {
	Executable  string `json:"executable"`
	ServicePath string `json:"servicePath"`
	Digest      string `json:"digest"`
}

// PrepareHelper writes an independent executable and its own service definition.
// InstallHelper additionally enables that service through the platform manager.
func PrepareHelper(cfg HelperConfig) (HelperInstallation, error) {
	lock, err := lockInstallation(context.Background(), cfg.StateDir)
	if err != nil {
		return HelperInstallation{}, err
	}
	defer unlock(lock)
	return prepareHelper(cfg, false)
}

func prepareHelper(cfg HelperConfig, upgrade bool) (HelperInstallation, error) {
	if !filepath.IsAbs(cfg.StateDir) || !filepath.IsAbs(cfg.Executable) {
		return HelperInstallation{}, errors.New("helper requires absolute state and executable paths")
	}
	if cfg.Kind != "systemd" && cfg.Kind != "launchd" {
		return HelperInstallation{}, errors.New("unsupported helper service manager")
	}
	if cfg.Kind == "launchd" && !launchDomainPattern.MatchString(cfg.Domain) {
		return HelperInstallation{}, errors.New("helper requires an explicit launchd user domain")
	}
	if cfg.ServiceDir == "" {
		dir, err := serviceDirectory(cfg.Kind)
		if err != nil {
			return HelperInstallation{}, err
		}
		cfg.ServiceDir = dir
	}
	if !filepath.IsAbs(cfg.ServiceDir) {
		return HelperInstallation{}, errors.New("helper service directory must be absolute")
	}
	digest, err := fileDigest(cfg.Executable)
	if err != nil {
		return HelperInstallation{}, err
	}
	dir := filepath.Join(transactionDir(cfg.StateDir), "helper", digest)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return HelperInstallation{}, err
	}
	installed := HelperInstallation{Executable: filepath.Join(dir, "mesh"), Digest: digest}
	if err = durableCopy(cfg.Executable, installed.Executable, digest); err != nil {
		return installed, err
	}
	launcher := filepath.Join(transactionDir(cfg.StateDir), "helper", "current")
	data, name, err := helperService(cfg, launcher)
	if err != nil {
		return installed, err
	}
	installed.ServicePath = filepath.Join(cfg.ServiceDir, name)
	var current HelperInstallation
	currentErr := readJSON(helperRecord(cfg.StateDir), &current)
	if currentErr == nil && !upgrade {
		return current, verifyFile(current.Executable, current.Digest)
	}
	if currentErr != nil && !errors.Is(currentErr, os.ErrNotExist) {
		return installed, currentErr
	}
	if currentErr != nil {
		existing, readErr := os.ReadFile(installed.ServicePath)
		if readErr == nil && !bytes.Equal(existing, []byte(data)) {
			return installed, errors.New("refusing to overwrite an unmanaged update helper service")
		}
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return installed, readErr
		}
	}
	if upgrade {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err = runCommand(ctx, installed.Executable, "update-helper", "--state-dir", cfg.StateDir, "--check-journal"); err != nil {
			return installed, err
		}
	}
	if err = atomicWrite(installed.ServicePath, []byte(data), 0644); err != nil {
		return installed, err
	}
	if err = replaceHelperLink(launcher, installed.Executable); err != nil {
		return installed, err
	}
	record, err := json.Marshal(installed)
	if err != nil {
		return installed, err
	}
	return installed, atomicWrite(helperRecord(cfg.StateDir), record, 0600)
}

func helperRecord(stateDir string) string {
	return filepath.Join(transactionDir(stateDir), "helper", "installed.json")
}

// UpgradeHelper switches the service only after daemon commitment and a real
// journal-read probe by the replacement. Both executable copies are retained.
func UpgradeHelper(ctx context.Context, cfg HelperConfig) (HelperInstallation, error) {
	lock, err := lockInstallation(ctx, cfg.StateDir)
	if err != nil {
		return HelperInstallation{}, err
	}
	defer unlock(lock)
	status, err := Read(cfg.StateDir)
	if err != nil {
		return HelperInstallation{}, err
	}
	if status.Phase != Committed {
		return HelperInstallation{}, errors.New("helper replacement requires a committed daemon installation")
	}
	digest, err := fileDigest(cfg.Executable)
	if err != nil {
		return HelperInstallation{}, err
	}
	var prior HelperInstallation
	if err = readJSON(helperRecord(cfg.StateDir), &prior); err != nil {
		return prior, err
	}
	if prior.Digest == digest {
		return prior, nil
	}
	installed, err := prepareHelper(cfg, true)
	if err != nil {
		return installed, err
	}
	if cfg.Kind == "launchd" {
		_, err = runCommand(ctx, "launchctl", "kickstart", "-k", cfg.Domain+"/dev.shaulavo.mesh-update-helper")
		return installed, err
	}
	if _, err = runCommand(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		return installed, err
	}
	_, err = runCommand(ctx, "systemctl", "--user", "restart", "--no-block", "mesh-update-helper.service")
	return installed, err
}

func replaceHelperLink(path, target string) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".helper-link-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer func() { _ = os.Remove(temporary) }()
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Remove(temporary); err != nil {
		return err
	}
	if err = os.Symlink(target, temporary); err != nil {
		return err
	}
	if err = os.Rename(temporary, path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func InstallHelper(ctx context.Context, cfg HelperConfig) (HelperInstallation, error) {
	installed, err := PrepareHelper(cfg)
	if err != nil {
		return installed, err
	}
	if cfg.Kind == "launchd" {
		_, err = runCommand(ctx, "launchctl", "bootstrap", cfg.Domain, installed.ServicePath)
		if err == nil {
			return installed, nil
		}
		_, checkErr := runCommand(ctx, "launchctl", "print", cfg.Domain+"/dev.shaulavo.mesh-update-helper")
		return installed, checkErr
	}
	if _, err = runCommand(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		return installed, err
	}
	_, err = runCommand(ctx, "systemctl", "--user", "enable", "--now", "mesh-update-helper.service")
	return installed, err
}

func helperService(cfg HelperConfig, executable string) (string, string, error) {
	if strings.ContainsAny(executable+cfg.StateDir, "\x00\r\n") {
		return "", "", errors.New("invalid helper service path")
	}
	if cfg.Kind == "systemd" {
		data := "[Unit]\nDescription=Mesh durable update helper\n\n[Service]\nType=simple\nExecStart=" + systemdQuote(executable) + " update-helper --state-dir " + systemdQuote(cfg.StateDir) + "\nRestart=always\nRestartSec=2\n\n[Install]\nWantedBy=default.target\n"
		return data, "mesh-update-helper.service", nil
	}
	data := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>Label</key><string>dev.shaulavo.mesh-update-helper</string>
<key>ProgramArguments</key><array><string>` + xmlText(executable) + `</string><string>update-helper</string><string>--state-dir</string><string>` + xmlText(cfg.StateDir) + `</string></array>
<key>RunAtLoad</key><true/><key>KeepAlive</key><true/><key>ThrottleInterval</key><integer>2</integer>
</dict></plist>
`
	return data, "dev.shaulavo.mesh-update-helper.plist", nil
}

func systemdQuote(value string) string {
	replacer := strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "%", "%%", "$", "$$")
	return "\"" + replacer.Replace(value) + "\""
}

func xmlText(value string) string {
	var buffer bytes.Buffer
	if err := xml.EscapeText(&buffer, []byte(value)); err != nil {
		return fmt.Sprint(value)
	}
	return buffer.String()
}
