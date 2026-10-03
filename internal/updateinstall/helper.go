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

	"github.com/shaul/mesh/internal/release"
)

type HelperConfig struct {
	StateDir        string
	Executable      string
	ServiceDir      string
	Kind            string
	Domain          string
	beforePromotion func(context.Context) error
}

type HelperInstallation struct {
	Executable  string `json:"executable"`
	ServicePath string `json:"servicePath"`
	Digest      string `json:"digest"`
}

// PrepareHelper writes an independent executable and its own service definition.
// InstallHelper additionally enables that service through the platform manager.
func PrepareHelper(ctx context.Context, cfg HelperConfig) (HelperInstallation, error) {
	lock, err := lockInstallation(ctx, cfg.StateDir)
	if err != nil {
		return HelperInstallation{}, err
	}
	defer unlock(lock)
	return prepareHelper(ctx, cfg, false, "")
}

func prepareHelper(ctx context.Context, cfg HelperConfig, upgrade bool, approvedDigest string) (HelperInstallation, error) {
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
	digest := approvedDigest
	if digest == "" {
		var err error
		digest, err = fileDigest(cfg.Executable)
		if err != nil {
			return HelperInstallation{}, err
		}
	}
	installed, err := stageHelperImage(ctx, cfg, digest)
	if err != nil {
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
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if _, err = helperCommandOutput(ctx, installed.Executable, "update-helper", "--state-dir", cfg.StateDir, "--check-journal"); err != nil {
			return installed, err
		}
	}
	if cfg.beforePromotion != nil {
		if err = cfg.beforePromotion(ctx); err != nil {
			return installed, err
		}
	}
	if err = ctx.Err(); err != nil {
		return installed, fmt.Errorf("helper preparation cancelled: %w", err)
	}
	if err = atomicWrite(installed.ServicePath, []byte(data), 0644); err != nil {
		return installed, err
	}
	record, err := json.Marshal(installed)
	if err != nil {
		return installed, err
	}
	// The receipt retains the approved image binding after later daemon commits.
	if err = atomicWrite(helperRecord(cfg.StateDir), record, 0600); err != nil {
		return installed, err
	}
	return installed, replaceHelperLink(launcher, installed.Executable)
}

func stageHelperImage(ctx context.Context, cfg HelperConfig, digest string) (HelperInstallation, error) {
	dir := filepath.Join(transactionDir(cfg.StateDir), "helper", digest)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return HelperInstallation{}, fmt.Errorf("create immutable helper directory: %w", err)
	}
	installed := HelperInstallation{Executable: filepath.Join(dir, "mesh"), Digest: digest}
	info, err := os.Lstat(installed.Executable)
	if err == nil {
		if !info.Mode().IsRegular() {
			return installed, errors.New("retained helper image must be a regular file")
		}
		if err = release.VerifyExecutable(ctx, installed.Executable, digest); err != nil {
			return installed, fmt.Errorf("verify retained immutable helper: %w", err)
		}
		return installed, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return installed, fmt.Errorf("inspect immutable helper destination: %w", err)
	}
	if err := durableCopy(ctx, cfg.Executable, installed.Executable, digest); err != nil {
		return installed, err
	}
	return installed, nil
}

func helperRecord(stateDir string) string {
	return filepath.Join(transactionDir(stateDir), "helper", "installed.json")
}

// UpgradeHelper advances the helper after daemon commitment and a real journal
// probe. A verified newer helper survives older daemon bridge releases.
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
	digest, err := committedHelperDigest(ctx, cfg.Executable, status.Request.Manifest)
	if err != nil {
		return HelperInstallation{}, err
	}
	var prior HelperInstallation
	if err = readJSON(helperRecord(cfg.StateDir), &prior); err != nil {
		return prior, err
	}
	interrupted, err := verifyHelperReceipt(ctx, cfg.StateDir, prior, digest, status.Request.Manifest)
	if err != nil {
		return prior, err
	}
	if prior.Digest == digest {
		return finishHelperPromotion(ctx, cfg, prior, interrupted)
	}
	allowed, err := helperUpgradeAllowed(ctx, cfg, status.Request.Manifest, prior, digest)
	if err != nil {
		return prior, err
	}
	if !allowed {
		return finishHelperPromotion(ctx, cfg, prior, interrupted)
	}
	installed, err := prepareHelper(ctx, cfg, true, digest)
	if err != nil {
		return installed, err
	}
	return activateHelper(ctx, cfg, installed)
}

func finishHelperPromotion(ctx context.Context, cfg HelperConfig, installed HelperInstallation, interrupted bool) (HelperInstallation, error) {
	if !interrupted {
		return installed, nil
	}
	if err := replaceHelperLink(filepath.Join(transactionDir(cfg.StateDir), "helper", "current"), installed.Executable); err != nil {
		return installed, err
	}
	return activateHelper(ctx, cfg, installed)
}

func activateHelper(ctx context.Context, cfg HelperConfig, installed HelperInstallation) (HelperInstallation, error) {
	if cfg.Kind == "launchd" {
		_, err := runCommand(ctx, "launchctl", "kickstart", "-k", cfg.Domain+"/dev.shaulavo.mesh-update-helper")
		return installed, err
	}
	if _, err := runCommand(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		return installed, err
	}
	_, err := runCommand(ctx, "systemctl", "--user", "restart", "--no-block", "mesh-update-helper.service")
	return installed, err
}

func committedHelperDigest(ctx context.Context, executable string, manifest release.Manifest) (string, error) {
	artifact, err := manifest.Artifact(release.CurrentPlatform())
	if err != nil {
		return "", fmt.Errorf("committed helper artifact: %w", err)
	}
	if err = release.VerifyExecutable(ctx, executable, artifact.BinarySHA256); err != nil {
		return "", fmt.Errorf("verify committed helper source: %w", err)
	}
	return artifact.BinarySHA256, nil
}

func verifyHelperReceipt(ctx context.Context, stateDir string, prior HelperInstallation, committedDigest string, manifest release.Manifest) (bool, error) {
	if prior.Executable != filepath.Join(transactionDir(stateDir), "helper", prior.Digest, "mesh") {
		return false, errors.New("helper receipt does not name its immutable executable copy")
	}
	if err := release.VerifyExecutable(ctx, prior.Executable, prior.Digest); err != nil {
		return false, fmt.Errorf("verify helper receipt: %w", err)
	}
	link, err := os.Readlink(filepath.Join(transactionDir(stateDir), "helper", "current"))
	if err != nil {
		return false, fmt.Errorf("read helper launcher: %w", err)
	}
	if link == prior.Executable {
		return false, nil
	}
	linkDigest := filepath.Base(filepath.Dir(link))
	if link != filepath.Join(transactionDir(stateDir), "helper", linkDigest, "mesh") {
		return false, errors.New("helper launcher does not name a retained immutable executable")
	}
	if err = release.VerifyExecutable(ctx, link, linkDigest); err != nil {
		return false, fmt.Errorf("verify interrupted helper launcher: %w", err)
	}
	return helperReceiptAhead(ctx, prior, link, linkDigest, committedDigest, manifest)
}

func helperReceiptAhead(ctx context.Context, prior HelperInstallation, link, linkDigest, committedDigest string, manifest release.Manifest) (bool, error) {
	current, err := helperBuild(ctx, prior.Executable, prior.Digest)
	if err != nil {
		return false, err
	}
	linked, err := helperBuild(ctx, link, linkDigest)
	if err != nil {
		return false, err
	}
	order, err := release.CompareVersions(current.Version, linked.Version)
	if err != nil {
		return false, fmt.Errorf("compare interrupted helper releases: %w", err)
	}
	if order > 0 {
		if err = helperCapabilities(current, manifest); err != nil {
			return false, err
		}
		return true, nil
	}
	// A legacy link-first promotion needs the still-current committed binding.
	if order < 0 && linkDigest == committedDigest {
		return false, nil
	}
	return false, errors.New("helper launcher advancement has no committed artifact binding")
}

func helperCapabilities(build release.Build, manifest release.Manifest) error {
	compatibility := manifest.Compatibility
	if compatibility.JournalVersion != release.CurrentJournalVersion ||
		build.StateVersion < compatibility.StateReadMin || build.StateVersion > compatibility.StateReadMax ||
		build.WorkerProtocol < compatibility.WorkerMin || build.WorkerProtocol > compatibility.WorkerMax {
		return errors.New("helper build has incompatible state, worker, or journal capabilities")
	}
	return nil
}

func helperUpgradeAllowed(ctx context.Context, cfg HelperConfig, manifest release.Manifest, prior HelperInstallation, digest string) (bool, error) {
	current, err := helperBuild(ctx, prior.Executable, prior.Digest)
	if err != nil {
		return false, err
	}
	candidate, err := helperBuild(ctx, cfg.Executable, digest)
	if err != nil {
		return false, err
	}
	if candidate.Version != manifest.Version || candidate.Commit != manifest.Commit {
		return false, errors.New("helper source build does not match the committed release")
	}
	compatibility := manifest.Compatibility
	if compatibility.JournalVersion != release.CurrentJournalVersion ||
		current.StateVersion < compatibility.StateReadMin || current.StateVersion > compatibility.StateReadMax ||
		current.WorkerProtocol < compatibility.WorkerMin || current.WorkerProtocol > compatibility.WorkerMax ||
		candidate.StateVersion != compatibility.StateWrite || candidate.WorkerProtocol != compatibility.WorkerWrite {
		return false, errors.New("helper builds have incompatible state, worker, or journal capabilities")
	}
	order, err := release.CompareVersions(candidate.Version, current.Version)
	if err != nil {
		return false, fmt.Errorf("compare helper releases: %w", err)
	}
	if order < 0 {
		return false, nil
	}
	if order == 0 {
		return false, errors.New("equal helper releases have different executable digests")
	}
	return true, nil
}

func helperBuild(ctx context.Context, executable, digest string) (release.Build, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	output, err := helperCommandOutput(ctx, executable, "version", "--json")
	if err != nil {
		return release.Build{}, err
	}
	var build release.Build
	if err = json.Unmarshal(output, &build); err != nil {
		return build, fmt.Errorf("decode helper build report: %w", err)
	}
	if build.Digest != digest || build.Platform != release.CurrentPlatform() || build.Modified ||
		len(build.Commit) != 40 || strings.Trim(build.Commit, "0123456789abcdef") != "" ||
		build.StateVersion <= 0 || build.WorkerProtocol <= 0 || build.UpdateProtocol != release.CurrentUpdateProtocol {
		return build, errors.New("helper build report does not match its verified executable")
	}
	return build, nil
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
	installed, err := PrepareHelper(ctx, cfg)
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
