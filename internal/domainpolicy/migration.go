package domainpolicy

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// The pre-policy releases served this namespace. It is used only when an
// existing deployment has no persisted certificate or name to identify it.
const legacyDeploymentDomain = "shaulavo.dev"

// InitializeDeployment preserves pre-policy deployments before listeners or
// service caches validate names. Native-only installations need no policy.
func InitializeDeployment(path, stateDir string, deploymentRequested bool) error {
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return Initialize(path)
	}
	domain, present, err := persistedDomain(stateDir)
	if err != nil {
		return err
	}
	if domain == "" && !present && !deploymentRequested {
		for _, evidence := range []string{filepath.Join(stateDir, "mesh.db"), filepath.Join(filepath.Dir(path), "hosts.json")} {
			if _, err := os.Stat(evidence); err == nil {
				present = true
				break
			} else if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("domain policy: inspect existing installation: %w", err)
			}
		}
		if !present {
			return nil
		}
	}
	if domain == "" {
		domain = legacyDeploymentDomain
	}
	policy := Policy{Primary: domain, LegacyCertificateDomain: domain}
	if err := policy.Validate(); err != nil {
		return err
	}
	if err := publishMigration(path, policy); err != nil {
		return err
	}
	return Initialize(path)
}

func persistedDomain(stateDir string) (string, bool, error) {
	domain, err := persistedPrivateDomain(stateDir)
	if err != nil {
		return "", false, err
	}
	present := domain != ""
	for _, profile := range []struct {
		root    string
		private bool
	}{
		{filepath.Join(stateDir, "private-tls"), true},
		{filepath.Join(stateDir, "certificates", "public-edge"), false},
	} {
		if _, err := os.Stat(profile.root); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return "", false, fmt.Errorf("domain policy: inspect legacy certificate store: %w", err)
		}
		present = true
		candidate, err := profileDomain(profile.root, profile.private)
		if err != nil {
			return "", false, err
		}
		domain, err = mergeLegacyDomain(domain, candidate)
		if err != nil {
			return "", false, err
		}

	}
	return domain, present, nil
}

func certificateDomain(slot string, private bool) (string, error) {
	pointer, err := readMigrationFile(filepath.Join(slot, "current"), 128)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	fingerprint := strings.TrimSpace(string(pointer))
	decoded, err := hex.DecodeString(fingerprint)
	if err != nil || len(decoded) != sha256.Size {
		return "", errors.New("domain policy: invalid legacy certificate pointer")
	}
	contents, err := readMigrationFile(filepath.Join(slot, fingerprint, "fullchain.pem"), 1<<20)
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(contents)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", errors.New("domain policy: invalid legacy certificate PEM")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("domain policy: parse legacy certificate: %w", err)
	}
	return wildcardDomain(certificate.DNSNames, private)
}

func wildcardDomain(names []string, private bool) (string, error) {

	prefix := "*."
	if private {
		prefix = "*.mesh."
	}
	var domain string
	for _, name := range names {
		candidate, ok := strings.CutPrefix(name, prefix)
		if !ok {
			continue
		}
		if err := validateDomain(candidate); err != nil {
			return "", err
		}
		if domain != "" && domain != candidate {
			return "", errors.New("domain policy: ambiguous legacy certificate domains")
		}
		domain = candidate
	}
	if domain == "" {
		return "", errors.New("domain policy: legacy certificate has no deployment wildcard")
	}
	return domain, nil
}

func readMigrationFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path) //nolint:gosec // paths are bounded legacy store slots below the selected state directory
	if err != nil {
		return nil, fmt.Errorf("domain policy: read legacy state: %w", err)
	}
	defer file.Close() //nolint:errcheck // read result is authoritative
	contents, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, fmt.Errorf("domain policy: read legacy state: %w", err)
	}
	if int64(len(contents)) > limit {
		return nil, errors.New("domain policy: legacy state exceeds size limit")
	}
	return contents, nil
}

func publishMigration(path string, policy Policy) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("domain policy: create configuration directory: %w", err)
	}
	contents, err := json.MarshalIndent(policy, "", "  ")
	if err != nil {
		return fmt.Errorf("domain policy: encode migration: %w", err)
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".domains-*")
	if err != nil {
		return fmt.Errorf("domain policy: prepare migration: %w", err)
	}
	defer os.Remove(file.Name()) //nolint:errcheck // the temporary name is disposable after publication
	defer file.Close()           //nolint:errcheck // explicit close before publication checks errors
	if _, err := file.Write(append(contents, '\n')); err != nil {
		return fmt.Errorf("domain policy: write migration: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("domain policy: sync migration: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("domain policy: close migration: %w", err)
	}
	// An atomic no-replace link preserves policy written by another startup or
	// the operator while we were inspecting state. Load that winner below.
	if err := os.Link(file.Name(), path); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("domain policy: publish migration: %w", err)
	}
	return nil
}

func persistedPrivateDomain(stateDir string) (string, error) {
	contents, err := readMigrationFile(filepath.Join(stateDir, "private-tls", "live", "private-name"), 254)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	name := strings.TrimSuffix(string(contents), "\n")
	label, domain, ok := strings.Cut(name, ".mesh.")
	if !ok || string(contents) != name+"\n" || strings.Contains(label, ".") {
		return "", errors.New("domain policy: invalid legacy private name")
	}
	if err := validateDomain(label + "." + domain); err != nil {
		return "", err
	}
	return domain, nil
}

func profileDomain(root string, private bool) (string, error) {
	var domain string
	for _, environment := range []string{"live", "staging"} {
		candidate, err := certificateDomain(filepath.Join(root, environment), private)
		if err != nil {
			return "", err
		}
		domain, err = mergeLegacyDomain(domain, candidate)
		if err != nil {
			return "", err
		}
	}
	return domain, nil
}

func mergeLegacyDomain(current, candidate string) (string, error) {
	if candidate == "" {
		return current, nil
	}
	if current != "" && current != candidate {
		return "", errors.New("domain policy: legacy certificate domains disagree")
	}
	return candidate, nil
}
