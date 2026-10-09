package dnsname

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	registryIdentity := testIdentityID(t)
	configPath := writePrivateNamesConfig(t, directory, fmt.Sprintf(`{
  "zoneId": "0123456789abcdef0123456789abcdef",
  "tokenFile": %q,
  "acmeEmail": "owner@example.com",
  "directoryUrl": %q,
  "acceptTerms": true,
  "interval": "6h",
  "origins": [{"name":"desktop","tailscaleName":"desktop.example.ts.net","identity":%q,"controlPort":7337,"websocketPath":"/mesh"}],
  "appRegistry": {"tailscaleName":"edge.example.ts.net","identity":%q,"controlPort":7443,"websocketPath":"/control/ws"}
}`, tokenPath, LetsEncryptProductionURL, identity, registryIdentity))
	config, err := LoadPrivateNamesConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if config.Environment != EnvironmentLive || config.DirectoryURL != LetsEncryptProductionURL || config.Interval != 6*time.Hour || !config.AcceptTerms || len(config.Origins) != 1 {
		t.Fatalf("config = %#v", config)
	}
	if config.AppRegistry == nil || config.AppRegistry.Identity != registryIdentity || config.AppRegistry.TailscaleName != "edge.example.ts.net" || config.AppRegistry.ControlPort != 7443 || config.AppRegistry.WebSocketPath != "/control/ws" {
		t.Fatalf("registry edge = %#v", config.AppRegistry)
	}

	for name, contents := range map[string]string{
		"removed public field": strings.ReplaceAll(readTestFile(t, configPath), `"appRegistry"`, `"publicEdge"`),
		"unknown field":        strings.ReplaceAll(readTestFile(t, configPath), `"interval": "6h",`, `"interval": "6h", "surprise": true,`),
		"multiple JSON values": readTestFile(t, configPath) + `{}`,
		"short interval":       strings.ReplaceAll(readTestFile(t, configPath), `"6h"`, `"1m"`),
		"relative token":       strings.ReplaceAll(readTestFile(t, configPath), fmt.Sprintf("%q", tokenPath), `"relative.token"`),
		"unsafe path":          strings.ReplaceAll(readTestFile(t, configPath), `"/mesh"`, `"/a/../mesh"`),
		"escaped path":         strings.ReplaceAll(readTestFile(t, configPath), `"/mesh"`, `"/m%65sh"`),
		"forced query":         strings.ReplaceAll(readTestFile(t, configPath), `"/mesh"`, `"/mesh?"`),
		"unknown directory":    strings.ReplaceAll(readTestFile(t, configPath), LetsEncryptProductionURL, "https://acme.example/directory"),
		"registry identity":    strings.ReplaceAll(readTestFile(t, configPath), registryIdentity, "not-an-identity"),
		"registry name":        strings.ReplaceAll(readTestFile(t, configPath), "edge.example.ts.net", "Edge.example.ts.net"),
		"registry port":        strings.ReplaceAll(readTestFile(t, configPath), `"controlPort":7443`, `"controlPort":0`),
		"registry path":        strings.ReplaceAll(readTestFile(t, configPath), `"/control/ws"`, `"/a/../control"`),
		"registry escaped":     strings.ReplaceAll(readTestFile(t, configPath), `"/control/ws"`, `"/control%2fws"`),
		"registry backslash":   strings.ReplaceAll(readTestFile(t, configPath), `"/control/ws"`, `"/control\\ws"`),
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
	registryIdentity := testIdentityID(t)
	configPath := writePrivateNamesConfig(t, directory, fmt.Sprintf(`{
  "zoneId":"0123456789abcdef0123456789abcdef","tokenFile":%q,"acmeEmail":"owner@example.com",
  "directoryUrl":%q,"acceptTerms":true,
  "origins":[{"name":"desktop","tailscaleName":"desktop.example.ts.net","identity":%q,"controlPort":7337,"websocketPath":"/mesh"}],
  "appRegistry":{"tailscaleName":"edge.example.ts.net","identity":%q,"controlPort":7443,"websocketPath":"/control/ws"}
}`, tokenPath, LetsEncryptProductionURL, identity, registryIdentity))
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
	if live.ServiceManager == nil || staging.ServiceManager == nil {
		t.Fatal("private-service certificate manager was not constructed")
	}
	liveRegistryIssuer := live.ServiceManager.renewer.(*Issuer)
	stagingRegistryIssuer := staging.ServiceManager.renewer.(*Issuer)
	if liveRegistryIssuer.config.Name != ServiceWildcardName() || stagingRegistryIssuer.config.Name != ServiceWildcardName() ||
		!strings.HasSuffix(liveRegistryIssuer.config.StateDir, "/private-service/live") || !strings.HasSuffix(stagingRegistryIssuer.config.StateDir, "/private-service/staging") ||
		liveRegistryIssuer.config.StateDir == stagingRegistryIssuer.config.StateDir {
		t.Fatalf("registry issuer state = live %#v staging %#v", liveRegistryIssuer.config, stagingRegistryIssuer.config)
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
	registryDistributor := stagingWithDistribution.ServiceManager.distributor.(*Distributor)
	if registryDistributor.profile != ProfilePrivateService || registryDistributor.environment != EnvironmentStaging || registryDistributor.expectedName != ServiceWildcardName() {
		t.Fatalf("registry distributor = %#v", registryDistributor)
	}

	withoutRegistryPath := writePrivateNamesConfig(t, t.TempDir(), strings.ReplaceAll(
		readTestFile(t, configPath),
		fmt.Sprintf(",\n  \"appRegistry\":{\"tailscaleName\":\"edge.example.ts.net\",\"identity\":%q,\"controlPort\":7443,\"websocketPath\":\"/control/ws\"}", registryIdentity),
		"",
	))
	withoutRegistry, err := NewPrivateNamesRuntime(withoutRegistryPath, PrivateNamesRuntimeOptions{StateDir: filepath.Join(directory, "without-registry")})
	if err != nil {
		t.Fatal(err)
	}
	if withoutRegistry.ServiceManager != nil {
		t.Fatal("omitted registry edge constructed a private-service certificate manager")
	}

	if err := os.Chmod(tokenPath, 0o644); err != nil { //nolint:gosec // deliberately loose permissions are the rejection fixture
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
	contents, err := os.ReadFile(path) //nolint:gosec // test reads its own temporary configuration fixture
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

func TestRenewalDomainOverlapKeepsZonesAndStateSeparate(t *testing.T) {
	root := t.TempDir()
	options := PrivateNamesRuntimeOptions{StateDir: filepath.Join(root, "state")}
	identity := testIdentityID(t)
	write := func(directory, domain, zone, extra string) string {
		t.Helper()
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		token := filepath.Join(directory, "token")
		if err := os.WriteFile(token, []byte("fixture-token\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return writePrivateNamesConfig(t, directory, fmt.Sprintf(`{"domain":%q,"zoneId":%q,"tokenFile":%q,"acmeEmail":"owner@example.com","acceptTerms":true,"directoryUrl":%q,"origins":[{"name":"pc","tailscaleName":"pc.example.ts.net","identity":%q,"controlPort":7337,"websocketPath":"/mesh"}]%s}`, domain, zone, token, LetsEncryptProductionURL, identity, extra))
	}
	old := write(filepath.Join(root, "old"), "old.test", "old-zone", "")
	primary := write(filepath.Join(root, "primary"), "mesh.test", "primary-zone", fmt.Sprintf(`,"additionalConfigs":[%q]`, old))
	runtime, err := NewPrivateNamesRuntime(primary, options)
	if err != nil {
		t.Fatal(err)
	}
	all := runtime.All()
	if len(all) != 2 {
		t.Fatalf("runtime count %d", len(all))
	}
	for index, domain := range []string{"mesh.test", "old.test"} {
		manager := all[index].Manager
		issuer := manager.renewer.(*Issuer)
		solver := issuer.config.Solver.(DNS01Solver)
		if manager.domain != domain || solver.Zone != domain || issuer.config.Name != "*.mesh."+domain {
			t.Fatalf("domain mixup: %s %s %s", manager.domain, solver.Zone, issuer.config.Name)
		}
	}
	if all[0].Manager.renewer.(*Issuer).config.StateDir == all[1].Manager.renewer.(*Issuer).config.StateDir {
		t.Fatal("zones share ACME state")
	}
	duplicate := write(filepath.Join(root, "duplicate"), "mesh.test", "duplicate-zone", "")
	primary = write(filepath.Join(root, "primary"), "mesh.test", "primary-zone", fmt.Sprintf(`,"additionalConfigs":[%q]`, duplicate))
	if _, err := NewPrivateNamesRuntime(primary, options); err == nil {
		t.Fatal("duplicate renewal domain accepted")
	}
	primary = write(filepath.Join(root, "primary"), "mesh.test", "primary-zone", fmt.Sprintf(`,"additionalConfigs":[%q]`, primary))
	if _, err := NewPrivateNamesRuntime(primary, options); err == nil {
		t.Fatal("cyclic renewal graph accepted")
	}
}

func credentialBindingFixture(t *testing.T) (string, string, PrivateNamesRuntimeOptions) {
	t.Helper()
	root := t.TempDir()
	token := filepath.Join(root, "token")
	if err := os.WriteFile(token, []byte("dns-write-only-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	contents := fmt.Sprintf(`{"zoneId":"legacy-zone","tokenFile":%q,"acmeEmail":"owner@example.com","acceptTerms":true,"directoryUrl":%q,"origins":[{"name":"pc","tailscaleName":"pc.example.ts.net","identity":%q,"controlPort":7337,"websocketPath":"/mesh"}]}`, token, LetsEncryptProductionURL, testIdentityID(t))
	return writePrivateNamesConfig(t, root, contents), contents, PrivateNamesRuntimeOptions{StateDir: filepath.Join(root, "state")}
}

func TestRenewalCredentialBindingRejectsDomainOrZoneChanges(t *testing.T) {
	for _, change := range []string{"domain with stale zone", "zone with inherited domain"} {
		t.Run(change, func(t *testing.T) {
			path, original, options := credentialBindingFixture(t)
			if _, err := NewPrivateNamesRuntime(path, options); err != nil {
				t.Fatal(err)
			}
			changed := strings.Replace(original, `{`, `{"domain":"old.test",`, 1)
			if change == "zone with inherited domain" {
				changed = strings.Replace(original, "legacy-zone", "destination-zone", 1)
			}
			writePrivateNamesConfig(t, filepath.Dir(path), changed)
			if _, err := NewPrivateNamesRuntime(path, options); err == nil || !strings.Contains(err.Error(), "zoneDomain") {
				t.Fatalf("changed credentials accepted without explicit zoneDomain binding: %v", err)
			}
		})
	}
}

func TestRenewalCredentialBindingPreservesDNSWriteOnlyLegacyConfig(t *testing.T) {
	path, _, options := credentialBindingFixture(t)
	var records []cloudflareRecord
	created := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/client/v4/zones/legacy-zone/dns_records" && r.URL.Path != "/client/v4/zones/legacy-zone/dns_records/challenge-id" {
			t.Errorf("DNS-Write-only token was used for metadata request: %s", r.URL.Path)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch r.Method {
		case http.MethodGet:
			pages := 0
			if len(records) != 0 {
				pages = 1
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": records, "result_info": map[string]int{"page": 1, "total_pages": pages}})
		case http.MethodPost:
			var input cloudflareRecordBody
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			created++
			records = []cloudflareRecord{{ID: "challenge-id", Type: input.Type, Name: input.Name, Content: input.Content, TTL: input.TTL, Comment: input.Comment}}
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": records[0]})
		case http.MethodDelete:
			records = nil
			_, _ = w.Write([]byte(`{"success":true,"result":{"id":"challenge-id"}}`))
		default:
			t.Errorf("unexpected DNS operation: %s", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()
	for range 2 {
		runtime, err := NewPrivateNamesRuntime(path, options)
		if err != nil {
			t.Fatal(err)
		}
		provider := runtime.Manager.provider.(*Cloudflare)
		// Redirect only this fixture's transport; runtime construction stays offline.
		provider.base, _ = provider.base.Parse(server.URL)
		provider.client = server.Client()
		solver := runtime.Manager.renewer.(*Issuer).config.Solver.(DNS01Solver)
		challenge, err := solver.Present(context.Background(), "_acme-challenge.mesh."+Zone(), "challenge-value")
		if err != nil {
			t.Fatalf("legacy renewal DNS challenge: %v", err)
		}
		if err := solver.Cleanup(context.Background(), challenge); err != nil {
			t.Fatalf("legacy renewal challenge cleanup: %v", err)
		}
	}
	if created != 2 || len(records) != 0 {
		t.Fatalf("legacy renewal DNS passes = %d, retained records = %v", created, records)
	}
}

func TestRenewalCredentialBindingRequiresMatchingZoneDomainToRebind(t *testing.T) {
	path, original, options := credentialBindingFixture(t)
	if _, err := NewPrivateNamesRuntime(path, options); err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(original, `{`, `{"domain":"old.test","zoneDomain":"mesh.test",`, 1)
	writePrivateNamesConfig(t, filepath.Dir(path), changed)
	if _, err := NewPrivateNamesRuntime(path, options); err == nil || !strings.Contains(err.Error(), "does not match renewal domain") {
		t.Fatalf("mismatched explicit binding error = %v", err)
	}
	changed = strings.Replace(changed, `"zoneDomain":"mesh.test"`, `"zoneDomain":"old.test"`, 1)
	changed = strings.Replace(changed, "legacy-zone", "destination-zone", 1)
	writePrivateNamesConfig(t, filepath.Dir(path), changed)
	if _, err := NewPrivateNamesRuntime(path, options); err != nil {
		t.Fatalf("explicit rebind: %v", err)
	}
	bindingPath := renewalCredentialBindingPath(path, options.StateDir)
	binding := readTestFile(t, bindingPath)
	if binding != `{"domain":"old.test","zoneId":"destination-zone"}` {
		t.Fatalf("persisted binding = %s", binding)
	}
	info, err := os.Stat(bindingPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("binding permissions = %v, %v", info, err)
	}
	// The explicit field can be removed after registryation; the recorded pair
	// remains authoritative on the next process start.
	changed = strings.Replace(changed, `"zoneDomain":"old.test",`, "", 1)
	writePrivateNamesConfig(t, filepath.Dir(path), changed)
	if _, err := NewPrivateNamesRuntime(path, options); err != nil {
		t.Fatalf("unchanged rebound config: %v", err)
	}
	writePrivateNamesConfig(t, filepath.Dir(path), original)
	if _, err := NewPrivateNamesRuntime(path, options); err == nil {
		t.Fatal("previous credentials silently replaced the rebound pair")
	}
}

func TestRenewalCredentialBindingChecksDeclaredDomainOnFirstUse(t *testing.T) {
	for _, declaration := range []string{"old.test", "Mesh.test", " mesh.test"} {
		t.Run(declaration, func(t *testing.T) {
			path, original, options := credentialBindingFixture(t)
			writePrivateNamesConfig(t, filepath.Dir(path), strings.Replace(original, `{`, fmt.Sprintf(`{"zoneDomain":%q,`, declaration), 1))
			if _, err := NewPrivateNamesRuntime(path, options); err == nil || !strings.Contains(err.Error(), "zoneDomain") {
				t.Fatalf("inherited domain accepted mismatched declaration: %v", err)
			}
			if _, err := os.Stat(options.StateDir); !os.IsNotExist(err) {
				t.Fatalf("failed config wrote renewal state: %v", err)
			}
		})
	}
	path, original, options := credentialBindingFixture(t)
	writePrivateNamesConfig(t, filepath.Dir(path), strings.Replace(original, `{`, `{"zoneDomain":"mesh.test",`, 1))
	if _, err := NewPrivateNamesRuntime(path, options); err != nil {
		t.Fatalf("matching inherited-domain declaration: %v", err)
	}
}

func TestRenewalCredentialBindingFailsClosedOnDamagedState(t *testing.T) {
	for _, damage := range []string{"invalid JSON", "missing fields", "unknown field", "multiple values", "loose mode", "symlink"} {
		t.Run(damage, func(t *testing.T) {
			path, original, options := credentialBindingFixture(t)
			if _, err := NewPrivateNamesRuntime(path, options); err != nil {
				t.Fatal(err)
			}
			// Explicit rebind cannot turn unreadable or corrupt history into a
			// fresh installation.
			writePrivateNamesConfig(t, filepath.Dir(path), strings.Replace(original, `{`, `{"zoneDomain":"mesh.test",`, 1))
			bindingPath := renewalCredentialBindingPath(path, options.StateDir)
			switch damage {
			case "loose mode":
				if err := os.Chmod(bindingPath, 0o644); err != nil { //nolint:gosec // deliberate insecure-mode fixture
					t.Fatal(err)
				}
			case "symlink":
				backup := bindingPath + ".backup"
				if err := os.Rename(bindingPath, backup); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(backup, bindingPath); err != nil {
					t.Fatal(err)
				}
			default:
				contents := map[string]string{
					"invalid JSON": "{", "missing fields": `{}`, "unknown field": `{"domain":"mesh.test","zoneId":"legacy-zone","extra":true}`,
					"multiple values": `{"domain":"mesh.test","zoneId":"legacy-zone"}{}`,
				}[damage]
				if err := os.WriteFile(bindingPath, []byte(contents), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := NewPrivateNamesRuntime(path, options); err == nil || !strings.Contains(err.Error(), "renewal credential binding") {
				t.Fatalf("damaged binding was accepted: %v", err)
			}
		})
	}
}
