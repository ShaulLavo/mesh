package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/machinename"
	"github.com/shaul/mesh/internal/transport"
)

func TestAgentRootExplicitHostRoutesShadowedName(t *testing.T) {
	f := namedDestination(t)
	info := f.info()
	info.MachineName = "session"
	f.reported.Store(&info)
	if err := SaveHost(f.host); err != nil {
		t.Fatal(err)
	}
	config, err := ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := machinename.RememberClaim(t.Context(), filepath.Dir(config), f.host.ID, declaredName(info)); err != nil {
		t.Fatal(err)
	}
	_, _, err = executeCommand(t, Dependencies{DialHost: dialControlHost, DialControl: dialControlHost}, "--host", "session", "-r")
	if err == nil || !strings.Contains(err.Error(), "session has no active sessions") || f.operations.Load() != 1 {
		t.Fatalf("explicit host did not reach destination: %v, calls %d", err, f.operations.Load())
	}
}

func TestAgentRootExplicitHostRequiresSeparator(t *testing.T) {
	_, _, err := executeCommand(t, Dependencies{}, "--host", "pc", "tool")
	if err == nil || !strings.Contains(err.Error(), "put the command after --") {
		t.Fatalf("missing separator: %v", err)
	}
}

func TestAgentRootCommandSeparatorKeepsChildJSON(t *testing.T) {
	host := setupCommandTestHost(t)
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	host.host.MachineName, host.host.NameRevision = "session", 1
	if err := saveNamedTestHost(t, host.host); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := executeCommand(t, Dependencies{DialHost: host.dial, DialControl: host.dial}, "--host", "session", "--", "tool", "--json")
	if err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	created := host.create
	host.mu.Unlock()
	if !slices.Equal(created.Command, []string{"tool", "--json"}) {
		t.Fatalf("child arguments changed: %v", created.Command)
	}
	if strings.Contains(stdout, `"schemaVersion"`) {
		t.Fatalf("child flag enabled envelope mode: %s", stdout)
	}
}

func TestAgentServicesEarlyFailures(t *testing.T) {
	for _, args := range [][]string{
		{"serve", "ls", "--json", "--bogus"},
		{"serve", "list", "--json", "extra"},
		{"serve", "ls", "--json", "--privacy"},
		{"serve", "ls", "--json", "--timeout=0"},
	} {
		stdout, stderr, err := executeCommand(t, Dependencies{}, args...)
		assertAgentFailure(t, stdout, stderr, err, "usage.invalid", 2)
	}
}

func assertAgentFailure(t *testing.T, stdout, stderr string, err error, code string, exit int) {
	t.Helper()
	var result struct {
		Operation string
		Status    string
		Errors    []struct{ Code string }
	}
	if decodeErr := json.Unmarshal([]byte(stdout), &result); decodeErr != nil {
		t.Fatalf("expected one envelope, got %q: %v", stdout, decodeErr)
	}
	if result.Operation != "services.list" || result.Status != "error" || len(result.Errors) != 1 || result.Errors[0].Code != code {
		t.Fatalf("unexpected envelope: %s", stdout)
	}
	if status, ok := StatusCode(err); !ok || status != exit || stderr != "" {
		t.Fatalf("exit=%v stderr=%q", err, stderr)
	}
}

func TestAgentServicesMissingStateIsNotCreated(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "absent-state")
	t.Setenv("MESH_CONFIG_DIR", filepath.Join(root, "absent-config"))
	t.Setenv("MESH_STATE_DIR", state)
	stdout, stderr, err := executeCommand(t, Dependencies{}, "serve", "list", "--json")
	if err != nil || stderr != "" || !strings.Contains(stdout, `"services":[]`) {
		t.Fatalf("empty catalog: %q %q %v", stdout, stderr, err)
	}
	if _, err := os.Lstat(state); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("passive read created state: %v", err)
	}
}

func TestAgentServicesKeylessSavedRemotePreservesState(t *testing.T) {
	f := namedDestination(t)
	if err := SaveHost(f.host); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(t.TempDir(), "absent")
	t.Setenv("MESH_STATE_DIR", state)
	dial := func(context.Context, HostRecord) (transport.Conn, error) {
		t.Error("active dial called")
		return nil, context.Canceled
	}
	stdout, stderr, err := executeCommand(t, Dependencies{DialControl: dial}, "serve", "ls", "--json")
	assertAgentFailure(t, stdout, stderr, err, "host.uninitialized", 1)
	if _, err := os.Lstat(state); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("keyless read created state: %v", err)
	}
	if f.operations.Load() != 0 {
		t.Fatal("session control used")
	}
}
