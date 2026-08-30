package dnsname

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadPrivateNamesConfigStrictlyValidatesOperationalFields(t *testing.T) {
	directory := t.TempDir()
	tokenPath := filepath.Join(directory, "cloudflare.token")
	if err := os.WriteFile(tokenPath, []byte("zone-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity := testIdentityID(t)
	configPath := writePrivateNamesConfig(t, directory, fmt.Sprintf(`{
  "zoneId": "0123456789abcdef0123456789abcdef",
  "tokenFile": %q,
  "acmeEmail": "owner@example.com",
  "directoryUrl": %q,
  "acceptTerms": true,
  "interval": "6h",
  "origins": [{"name":"desktop","tailscaleName":"desktop.example.ts.net","identity":%q,"controlPort":7337,"websocketPath":"/mesh"}]
}`, tokenPath, LetsEncryptProductionURL, identity))
	config, err := LoadPrivateNamesConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if config.Environment != EnvironmentLive || config.DirectoryURL != LetsEncryptProductionURL || config.Interval != 6*time.Hour || !config.AcceptTerms || len(config.Origins) != 1 {
		t.Fatalf("config = %#v", config)
	}

	for name, contents := range map[string]string{
		"unknown field":        strings.ReplaceAll(readTestFile(t, configPath), `"interval": "6h",`, `"interval": "6h", "surprise": true,`),
		"multiple JSON values": readTestFile(t, configPath) + `{}`,
		"short interval":       strings.ReplaceAll(readTestFile(t, configPath), `"6h"`, `"1m"`),
		"relative token":       strings.ReplaceAll(readTestFile(t, configPath), fmt.Sprintf("%q", tokenPath), `"relative.token"`),
		"unsafe path":          strings.ReplaceAll(readTestFile(t, configPath), `"/mesh"`, `"/a/../mesh"`),
		"escaped path":         strings.ReplaceAll(readTestFile(t, configPath), `"/mesh"`, `"/m%65sh"`),
		"forced query":         strings.ReplaceAll(readTestFile(t, configPath), `"/mesh"`, `"/mesh?"`),
		"unknown directory":    strings.ReplaceAll(readTestFile(t, configPath), LetsEncryptProductionURL, "https://acme.example/directory"),
	} {
		t.Run(name, func(t *testing.T) {
			path := writePrivateNamesConfig(t, t.TempDir(), contents)
			if _, err := LoadPrivateNamesConfig(path); err == nil {
				t.Fatal("invalid config was accepted")
			}
		})
	}
}

func TestPrivateNamesRuntimeRequiresSecureTokenAndSeparatesEnvironments(t *testing.T) {
	directory := t.TempDir()
	tokenPath := filepath.Join(directory, "cloudflare.token")
	if err := os.WriteFile(tokenPath, []byte("zone-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity := testIdentityID(t)
	configPath := writePrivateNamesConfig(t, directory, fmt.Sprintf(`{
  "zoneId":"0123456789abcdef0123456789abcdef","tokenFile":%q,"acmeEmail":"owner@example.com",
  "directoryUrl":%q,"acceptTerms":true,
  "origins":[{"name":"desktop","tailscaleName":"desktop.example.ts.net","identity":%q,"controlPort":7337,"websocketPath":"/mesh"}]
}`, tokenPath, LetsEncryptProductionURL, identity))
	stateDir := filepath.Join(directory, "state")
	live, err := NewPrivateNamesRuntime(configPath, PrivateNamesRuntimeOptions{StateDir: stateDir, DirectoryURL: LetsEncryptProductionURL})
	if err != nil {
		t.Fatal(err)
	}
	staging, err := NewPrivateNamesRuntime(configPath, PrivateNamesRuntimeOptions{StateDir: stateDir, DirectoryURL: LetsEncryptStagingURL})
	if err != nil {
		t.Fatal(err)
	}
	liveIssuer := live.Manager.renewer.(*Issuer)
	stagingIssuer := staging.Manager.renewer.(*Issuer)
	if liveIssuer.config.StateDir == stagingIssuer.config.StateDir || !strings.HasSuffix(liveIssuer.config.StateDir, "/private-names/live") || !strings.HasSuffix(stagingIssuer.config.StateDir, "/private-names/staging") {
		t.Fatalf("live state = %s, staging state = %s", liveIssuer.config.StateDir, stagingIssuer.config.StateDir)
	}
	stagingWithDistribution, err := NewPrivateNamesRuntime(configPath, PrivateNamesRuntimeOptions{
		StateDir: stateDir, DirectoryURL: LetsEncryptStagingURL, Distribute: true, Signer: testPrivateIdentity(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := stagingWithDistribution.Manager.distributor.(*Distributor).environment; got != EnvironmentStaging {
		t.Fatalf("staging distributor environment = %q", got)
	}

	if err := os.Chmod(tokenPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPrivateNamesRuntime(configPath, PrivateNamesRuntimeOptions{StateDir: stateDir}); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("loose token error = %v", err)
	}
}

func TestReadCloudflareTokenRejectsSymlinkAndWhitespace(t *testing.T) {
	directory := t.TempDir()
	realPath := filepath.Join(directory, "real.token")
	if err := os.WriteFile(realPath, []byte("valid-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlinkPath := filepath.Join(directory, "linked.token")
	if err := os.Symlink(realPath, symlinkPath); err != nil {
		t.Fatal(err)
	}
	if _, err := readCloudflareToken(symlinkPath); err == nil {
		t.Fatalf("symlink token error = %v", err)
	}
	if err := os.WriteFile(realPath, []byte(" leading-space\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readCloudflareToken(realPath); err == nil || !strings.Contains(err.Error(), "invalid token") {
		t.Fatalf("whitespace token error = %v", err)
	}
}

func writePrivateNamesConfig(t *testing.T, directory, contents string) string {
	t.Helper()
	path := filepath.Join(directory, "private-names.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}

func testPrivateIdentity(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return privateKey
}
