package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/mcpwire"
)

func TestParseLoopbackListenAddr(t *testing.T) {
	for _, tc := range []struct {
		raw       string
		allowZero bool
		want      string
	}{
		{"127.0.0.1:8765", false, "127.0.0.1:8765"},
		{"127.0.0.1:0", true, "127.0.0.1:0"},
		{"[::1]:8765", false, "[::1]:8765"},
	} {
		got, err := parseLoopbackListenAddr(tc.raw, tc.allowZero)
		if err != nil || got != tc.want {
			t.Errorf("parseLoopbackListenAddr(%q, %v) = %q, %v; want %q", tc.raw, tc.allowZero, got, err, tc.want)
		}
	}
	for _, tc := range []struct {
		raw       string
		allowZero bool
	}{
		{"", true},
		{"0.0.0.0:8765", true},
		{"[::]:8765", true},
		{"192.168.1.10:8765", true},
		{"localhost:8765", true},
		{"http://127.0.0.1:8765/mcp", true},
		{"127.0.0.1:0", false},
		{"127.0.0.1:99999", true},
		{"127.0.0.1:-1", true},
	} {
		if got, err := parseLoopbackListenAddr(tc.raw, tc.allowZero); err == nil {
			t.Errorf("parseLoopbackListenAddr(%q, %v) = %q, want error", tc.raw, tc.allowZero, got)
		}
	}
}

func TestMCPHTTPValidationAndTokenFileSecurity(t *testing.T) {
	prepareCommandEnvironment(t)

	// Missing --identity with --http fails with usage error and empty stdout.
	stdout, _, err := executeCommand(t, Dependencies{}, "mcp", "--http", "127.0.0.1:0")
	if err == nil || stdout != "" {
		t.Fatalf("missing identity: stdout=%q err=%v", stdout, err)
	}

	// --allow-origin or --token-file without --http fails with usage error.
	for _, args := range [][]string{
		{"mcp", "--allow-origin", "http://127.0.0.1:3000"},
		{"mcp", "--token-file", filepath.Join(t.TempDir(), "tok.json")},
		{"mcp", "--http", "0.0.0.0:8765", "--identity", "agent-a"},
		{"mcp", "--http", "127.0.0.1:0", "--identity", "missing-id"},
	} {
		stdout, _, err := executeCommand(t, Dependencies{}, args...)
		if err == nil || stdout != "" {
			t.Fatalf("%v: stdout=%q err=%v, want error and empty stdout", args, stdout, err)
		}
	}

	// Create identity, then verify non-loopback origin and symlink token-file are rejected.
	if _, _, err := executeCommand(t, Dependencies{}, "mcp", "identity", "create", "agent-http", "--json"); err != nil {
		t.Fatalf("create identity: %v", err)
	}
	if stdout, _, err := executeCommand(t, Dependencies{}, "mcp", "--http", "127.0.0.1:0", "--identity", "agent-http", "--allow-origin", "https://example.com"); err == nil || stdout != "" {
		t.Fatalf("public origin accepted: stdout=%q err=%v", stdout, err)
	}

	symlinkPath := filepath.Join(t.TempDir(), "link.json")
	if err := os.Symlink(filepath.Join(t.TempDir(), "real.json"), symlinkPath); err != nil {
		t.Fatal(err)
	}
	if stdout, _, err := executeCommand(t, Dependencies{}, "mcp", "--http", "127.0.0.1:0", "--identity", "agent-http", "--token-file", symlinkPath); err == nil || stdout != "" {
		t.Fatalf("symlink token file accepted: stdout=%q err=%v", stdout, err)
	}
}

func TestMCPHTTPLiveLifecycleRotateAndCleanup(t *testing.T) {
	prepareCommandEnvironment(t)
	if _, _, err := executeCommand(t, Dependencies{}, "mcp", "identity", "create", "agent-http", "--json"); err != nil {
		t.Fatalf("create identity: %v", err)
	}

	tokenPath := filepath.Join(t.TempDir(), "mcp-http-token.json")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	stdoutFile, err := os.CreateTemp(t.TempDir(), "stdout-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdoutFile.Close() }()
	stderrFile, err := os.CreateTemp(t.TempDir(), "stderr-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stderrFile.Close() }()

	cmd := NewCommand(Dependencies{
		Stdout: stdoutFile,
		Stderr: stderrFile,
		Now:    time.Now,
	})
	cmd.SetArgs([]string{"mcp", "--http", "127.0.0.1:0", "--identity", "agent-http", "--token-file", tokenPath, "--allow-origin", "http://127.0.0.1:3456"})
	done := make(chan error, 1)
	go func() {
		done <- cmd.ExecuteContext(ctx)
	}()

	// Wait until the private 0600 token file appears.
	var record mcpHTTPTokenFile
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(tokenPath) //nolint:gosec // test temp file
		if err == nil && json.Unmarshal(raw, &record) == nil && record.Token != "" {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if record.Token == "" {
		cancel()
		<-done
		stderrBytes, _ := os.ReadFile(stderrFile.Name())
		t.Fatalf("token file never populated (stderr=%q)", stderrBytes)
	}

	info, err := os.Lstat(tokenPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode = %v, err=%v; want regular 0600", info.Mode(), err)
	}
	if record.Identity != "agent-http" || record.Generation != 1 || !strings.HasSuffix(record.Endpoint, "/mcp") {
		t.Fatalf("unexpected token record: %+v", record)
	}

	httpClient := &http.Client{Timeout: 5 * time.Second}
	skillBody := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}},"name":"mesh_skill","arguments":{}}}`

	postSkill := func(tok string) int {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, record.Endpoint, strings.NewReader(skillBody))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set(mcpwire.ProtocolHeader, mcpwire.Version)
		req.Header.Set(mcpwire.MethodHeader, mcpwire.MethodCallTool)
		req.Header.Set(mcpwire.NameHeader, "mesh_skill")
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	// Unauthenticated fails 401; authenticated succeeds 200.
	if got := postSkill(""); got != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", got)
	}
	if got := postSkill(record.Token); got != http.StatusOK {
		t.Fatalf("authenticated status = %d, want 200", got)
	}

	// Wrong identity on rotate fails cleanly and leaves token intact.
	if _, _, err := executeCommand(t, Dependencies{}, "mcp", "token", "rotate", "--http", record.Listener, "--identity", "other-id", "--json"); err == nil {
		t.Fatal("rotate succeeded with wrong identity")
	}
	if got := postSkill(record.Token); got != http.StatusOK {
		t.Fatalf("token broken by failed rotate: status = %d", got)
	}

	// Valid rotate replaces token atomically and never prints the secret.
	rotOut, rotErr, err := executeCommand(t, Dependencies{}, "mcp", "token", "rotate", "--http", record.Listener, "--identity", "agent-http", "--json")
	if err != nil {
		t.Fatalf("rotate failed: out=%q err=%q errVal=%v", rotOut, rotErr, err)
	}
	var env struct {
		Status agentapi.Status             `json:"status"`
		Data   agentapi.MCPTokenRotateData `json:"data"`
	}
	if err := json.Unmarshal([]byte(rotOut), &env); err != nil || env.Status != agentapi.StatusOK || env.Data.Generation != 2 {
		t.Fatalf("rotate envelope = %q, err=%v", rotOut, err)
	}

	rawAfter, err := os.ReadFile(tokenPath) //nolint:gosec // test temp file
	if err != nil {
		t.Fatal(err)
	}
	var rotated mcpHTTPTokenFile
	if err := json.Unmarshal(rawAfter, &rotated); err != nil || rotated.Generation != 2 || rotated.Token == "" || rotated.Token == record.Token {
		t.Fatalf("rotated token file = %+v, err=%v", rotated, err)
	}
	if strings.Contains(rotOut, rotated.Token) || strings.Contains(rotErr, rotated.Token) ||
		strings.Contains(rotOut, record.Token) || strings.Contains(rotErr, record.Token) {
		t.Fatalf("rotate output leaked bearer token: out=%q err=%q", rotOut, rotErr)
	}

	if got := postSkill(record.Token); got != http.StatusUnauthorized {
		t.Fatalf("old token after rotate status = %d, want 401", got)
	}
	if got := postSkill(rotated.Token); got != http.StatusOK {
		t.Fatalf("new token after rotate status = %d, want 200", got)
	}

	// Cancel context -> clean exit, empty stdout, zero token leakage, token file removed.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("HTTP server exit error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP server did not exit promptly")
	}
	stdoutBytes, err := os.ReadFile(stdoutFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	stderrBytes, err := os.ReadFile(stderrFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	if len(stdoutBytes) != 0 {
		t.Fatalf("expected empty stdout, got %q", stdoutBytes)
	}
	if logs := string(stderrBytes); strings.Contains(logs, record.Token) || strings.Contains(logs, rotated.Token) {
		t.Fatalf("server stderr leaked bearer token: %q", logs)
	}
	if _, err := os.Lstat(tokenPath); !os.IsNotExist(err) {
		t.Fatalf("token file was not cleaned up on exit: %v", err)
	}
}
