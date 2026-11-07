package domainpolicy

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
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
	if err := initializeDeploymentPolicy(path, stateDir, deploymentRequested, true); err != nil {
		return err
	}
	_, _ = ConsumerCatalogToken(stateDir)
	return nil
}

// InitializeReadOnlyDeployment infers legacy names without changing any files.
func InitializeReadOnlyDeployment(path, stateDir string) error {
	if err := initializeDeploymentPolicy(path, stateDir, false, false); err != nil {
		return err
	}
	_, _ = ConsumerCatalogToken(stateDir)
	return nil
}

func initializeDeploymentPolicy(path, stateDir string, deploymentRequested, persist bool) error {
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return Initialize(path)
	}
	domain, present, err := persistedDomain(stateDir)
	if err != nil {
		return err
	}
	if domain == "" && !present {
		present, err = existingInstallation(path, stateDir)
		if err != nil {
			return err
		}
		if !present && !deploymentRequested {
			return nil
		}
	}
	// Only a fresh deployment request chooses the current namespace.
	// Existing installations retain their inferred legacy policy.
	policy := Policy{Primary: "sprockt.dev"}
	if present {
		if domain == "" {
			domain = legacyDeploymentDomain
		}
		policy = Policy{Primary: domain, LegacyCertificateDomain: domain}
	}
	if err := policy.Validate(); err != nil {
		return err
	}
	if !persist {
		return installPolicy(policy)
	}
	if err := publishDeploymentPolicy(path, stateDir, policy); err != nil {
		return err
	}
	return Initialize(path)
}

// MissingPolicyError identifies state whose primary and aliases need recovery.
type MissingPolicyError struct {
	Profile string
}

func (e *MissingPolicyError) Error() string {
	return fmt.Sprintf("domain policy: per-domain certificate slots exist in %s; restore domains.json from the deployment configuration backup", e.Profile)
}

func persistedDomain(stateDir string) (string, bool, error) {
	if err := requireLegacyLayout(stateDir); err != nil {
		return "", false, err
	}
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
		{filepath.Join(stateDir, "certificates", "private-service"), false},
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

func requireLegacyLayout(stateDir string) error {
	for _, profile := range []string{"private-tls", filepath.Join("certificates", "private-service")} {
		root := filepath.Join(stateDir, profile)
		if _, err := os.Lstat(filepath.Join(root, "domains")); err == nil {
			return &MissingPolicyError{Profile: root}
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("domain policy: inspect per-domain certificate slots: %w", err)
		}
	}
	return nil
}

func existingInstallation(path, stateDir string) (bool, error) {
	for _, evidence := range []string{filepath.Join(stateDir, "mesh.db"), filepath.Join(filepath.Dir(path), "hosts.json")} {
		if _, err := os.Stat(evidence); err == nil {
			return true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("domain policy: inspect existing installation: %w", err)
		}
	}
	return false, nil
}

func publishDeploymentPolicy(path, stateDir string, policy Policy) error {
	ctx := context.Background()
	return WithCatalogGate(ctx, stateDir, func(gate *CatalogGate) error { return publishInitialDomain(ctx, path, policy, gate) })
}
func publishInitialDomain(ctx context.Context, path string, policy Policy, gate *CatalogGate) error {
	return WithDomainFile(ctx, path, func(file *DomainFile) error {
		if file.Present {
			return file.Settle()
		}
		if err := gate.Prepare(policy.Digest(), nil); err != nil {
			return err
		}
		return file.Save(policy)
	})
}
