package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
)

func scopeCommandResult(t *testing.T, args ...string) agentapi.Envelope {
	t.Helper()
	stdout, stderr, err := executeCommand(t, Dependencies{}, args...)
	if err != nil || stderr != "" {
		t.Fatalf("scope command: %v stdout=%s stderr=%s", err, stdout, stderr)
	}
	var result agentapi.Envelope
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != agentapi.StatusOK || result.Errors == nil || !strings.HasPrefix(result.Operation, "scope.") {
		t.Fatal("invalid scope envelope", result)
	}
	return result
}
func TestScopeLocalWorkflowAndNoOutputEnrollment(t *testing.T) {
	root := t.TempDir()
	state, config, keyDir := filepath.Join(root, "target"), filepath.Join(root, "config"), filepath.Join(root, "dedicated")
	t.Setenv("MESH_STATE_DIR", state)
	t.Setenv("MESH_CONFIG_DIR", config)
	if _, _, err := identity.LoadOrCreate(state); err != nil {
		t.Fatal(err)
	}
	scopeCommandResult(t, "device", "scope", "keygen", "--key-dir", keyDir, "--json")
	subject, _, err := identity.LoadCredential(keyDir)
	if err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	grant := []string{"device", "scope", "grant", "--identity", subject.ID, "--permissions", "metadata", "--expires-at", expiry, "--expected-revision", "0", "--json"}
	scopeCommandResult(t, grant...)
	scopeCommandResult(t, grant...)
	result := scopeCommandResult(t, "device", "scope", "list", "--json")
	encoded, err := json.Marshal(result.Data)
	if err != nil {
		t.Fatal(err)
	}
	var policy agentapi.ScopePolicy
	if err := json.Unmarshal(encoded, &policy); err != nil {
		t.Fatal(err)
	}
	if policy.Revision != 1 || len(policy.Entries) != 1 || policy.Entries[0].Subject != subject.ID {
		t.Fatal("unexpected scoped policy", policy)
	}
	stdout, stderr, err := executeCommand(t, Dependencies{}, "device", "scope", "grant", "--identity", subject.ID, "--permissions", "metadata,session-output", "--expected-revision", "1", "--json")
	assertScopeFailure(t, stdout, stderr, err, "usage.invalid", 2)
	scopeCommandResult(t, "device", "scope", "revoke", "--identity", subject.ID, "--expected-revision", "1", "--json")
	if _, err := os.Stat(config); !os.IsNotExist(err) {
		t.Fatal("local scope operation initialized client config", err)
	}
}
func TestScopeKeygenBypassesNormalStateAndConfigActivation(t *testing.T) {
	root := t.TempDir()
	state, config := filepath.Join(root, "absent-state"), filepath.Join(root, "config")
	t.Setenv("MESH_STATE_DIR", state)
	t.Setenv("MESH_CONFIG_DIR", config)
	if err := os.Mkdir(config, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "hosts.json"), []byte("malformed"), 0600); err != nil {
		t.Fatal(err)
	}
	scopeCommandResult(t, "device", "scope", "keygen", "--key-dir", filepath.Join(root, "credential"), "--json")
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatal("keygen initialized normal identity", err)
	}
}
func TestScopeMissingCredentialNeverFallsBackOrCreatesState(t *testing.T) {
	root := t.TempDir()
	state, keyDir := filepath.Join(root, "normal"), filepath.Join(root, "absent-key")
	t.Setenv("MESH_STATE_DIR", state)
	t.Setenv("MESH_CONFIG_DIR", filepath.Join(root, "config"))
	stdout, stderr, err := executeCommand(t, Dependencies{}, "device", "scope", "read", "--host", "missing", "--key-dir", keyDir, "--json")
	assertScopeFailure(t, stdout, stderr, err, "state.unavailable", 1)
	for _, path := range []string{state, keyDir, filepath.Join(root, "config")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("scope read initialized %s: %v", path, err)
		}
	}
}
func TestScopeUsageErrorsAreSingleEnvelopes(t *testing.T) {
	t.Setenv("MESH_STATE_DIR", filepath.Join(t.TempDir(), "absent"))
	for _, args := range [][]string{
		{"device", "scope", "--json"}, {"device", "scope", "unknown", "--json"},
		{"device", "scope", "read", "--host", "x", "--key-dir", "x", "--limit=0", "--section=sessions", "--json"},
		{"device", "scope", "read", "--host", "x", "--key-dir", "x", "--timeout=99ms", "--json"},
		{"device", "scope", "read", "--host", "x", "--key-dir", "x", "--section=host", "--limit=1", "--json"},
		{"device", "scope", "read", "--host", "x", "--key-dir", "x", "--unexpected", "--json"},
		{"device", "scope", "grant", "--host", "remote", "--json"},
		{"device", "scope", "list", "--privacy", "--json"},
	} {
		stdout, stderr, err := executeCommand(t, Dependencies{}, args...)
		assertScopeFailure(t, stdout, stderr, err, "usage.invalid", 2)
	}
}

func assertScopeFailure(t *testing.T, stdout, stderr string, err error, code string, exit int) {
	t.Helper()
	var result agentapi.Envelope
	if decodeErr := json.Unmarshal([]byte(stdout), &result); decodeErr != nil {
		t.Fatal("single scope envelope missing", stdout, decodeErr)
	}
	if !strings.HasPrefix(result.Operation, "scope.") || result.Status != agentapi.StatusError || result.Data != nil || len(result.Errors) != 1 || string(result.Errors[0].Code) != code {
		t.Fatal("invalid scope failure", stdout)
	}
	if status, ok := StatusCode(err); !ok || status != exit || stderr != "" {
		t.Fatalf("exit=%v stderr=%s", err, stderr)
	}
}

func TestScopeMetadataDecodedDTOGolden(t *testing.T) {
	raw := `{"type":"access.read.result","requestId":"golden","accessResult":{"subject":{"targetId":"khI9qfAZ1eqQXe4C2JhMIfS8lwSL_GC5Aef-MsKEYZE","principal":"AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE","class":"scoped","permissions":["metadata"],"expiresAt":"2026-11-08T00:00:00Z"},"section":"sessions","observedAt":"2026-11-07T00:00:00Z","freshness":"live","snapshot":"non-atomic","host":null,"sessions":[{"id":"7K3D","hostId":"khI9qfAZ1eqQXe4C2JhMIfS8lwSL_GC5Aef-MsKEYZE","state":"detached","createdAt":"2026-11-06T00:00:00Z","lastAttachedAt":null,"detachedAt":null,"exitCode":null}],"services":[],"nextPage":null}}`
	reply, err := protocol.DecodeAccessReply([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(scopeMetadataResult(*reply.Result), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot("../agentapi/testdata")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if os.Getenv("MESH_UPDATE_AGENT_GOLDENS") == "1" {
		if err := root.WriteFile("scope.metadata.json", data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := root.ReadFile("scope.metadata.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(want, data) {
		t.Fatalf("scoped DTO golden differs:\n%s", data)
	}
}
