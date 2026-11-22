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
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/testenv"
	"github.com/shaul/mesh/internal/transport"
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

	symlinkDir := t.TempDir()
	symlinkPath := filepath.Join(symlinkDir, "link.json")
	if err := os.Symlink(filepath.Join(symlinkDir, "real.json"), symlinkPath); err != nil {
		t.Fatal(err)
	}
	if stdout, _, err := executeCommand(t, Dependencies{}, "mcp", "--http", "127.0.0.1:0", "--identity", "agent-http", "--token-file", symlinkPath); err == nil || stdout != "" || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("symlink token file accepted or wrong error: stdout=%q err=%v", stdout, err)
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
	record := waitForMCPHTTPTokenFile(t, tokenPath, done, stderrFile)

	info, err := os.Lstat(tokenPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode = %v, err=%v; want regular 0600", info.Mode(), err)
	}
	if record.Identity != "agent-http" || record.Generation != 1 || !strings.HasSuffix(record.Endpoint, "/mcp") {
		t.Fatalf("unexpected token record: %+v", record)
	}

	httpClient := &http.Client{Timeout: testenv.HangTimeout}
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
	case <-time.After(testenv.HangTimeout):
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

func TestMCPHTTPForwardsCallerIdentityToDaemon(t *testing.T) {
	prepareCommandEnvironment(t)
	if _, _, err := executeCommand(t, Dependencies{}, "mcp", "identity", "create", "review-a", "--json"); err != nil {
		t.Fatalf("create identity: %v", err)
	}
	wantID, err := loadApplicationIdentity("test", "review-a")
	if err != nil {
		t.Fatal(err)
	}
	hostID := "khI9qfAZ1eqQXe4C2JhMIfS8lwSL_GC5Aef-MsKEYZE"
	if err := SaveHost(HostRecord{
		ID:           hostID,
		MeshIdentity: hostID,
		MachineName:  "pc",
		NameRevision: 1,
		Endpoint:     "ws://127.0.0.1:1/control/ws",
	}); err != nil {
		t.Fatal(err)
	}

	gotCaller := make(chan protocol.CallerCredential, 4)
	dial := func(_ context.Context, h HostRecord) (transport.Conn, error) {
		return newPoolFixtureConn(func(req protocol.Control) (protocol.Control, error) {
			if req.Type == protocol.TypeHostInfo {
				return protocol.Control{
					Type: protocol.TypeHostInfoResult,
					Host: &protocol.HostInfo{ID: h.ID, MeshIdentity: h.MeshIdentity, MachineName: "pc", NameRevision: 1, ExecutionPipeSupported: true},
				}, nil
			}
			gotCaller <- req.Caller
			return protocol.Control{
				Type: protocol.TypeExecutionReceipt,
				ExecutionReceipt: &protocol.ExecutionReceipt{
					HostID: h.ID, SessionID: req.SessionID, Instance: req.Instance, RequestKey: "k1",
					KeyScope: "application", Kind: "pipe", Phase: "live", Acceptance: "durable", LaunchOutcome: "started", Publication: "published", Removal: "present",
				},
			}, nil
		}), nil
	}

	tokenPath := filepath.Join(t.TempDir(), "review-a.json")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stderrFile, err := os.CreateTemp(t.TempDir(), "stderr-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stderrFile.Close() }()
	cmd := NewCommand(Dependencies{DialPassive: dial, Stderr: stderrFile, Now: time.Now})
	cmd.SetArgs([]string{"mcp", "--http", "127.0.0.1:0", "--identity", "review-a", "--token-file", tokenPath})
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()
	defer func() {
		cancel()
		<-done
	}()

	record := waitForMCPHTTPTokenFile(t, tokenPath, done, stderrFile)

	jobHandle := "mj1." + hostID + ".7K3D.0123456789abcdef0123456789abcdef"
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}},"name":"mesh_job_status","arguments":{"job":"` + jobHandle + `"}}}`
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, record.Endpoint, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+record.Token)
	req.Header.Set(mcpwire.ProtocolHeader, mcpwire.Version)
	req.Header.Set(mcpwire.MethodHeader, mcpwire.MethodCallTool)
	req.Header.Set(mcpwire.NameHeader, "mesh_job_status")
	resp, err := (&http.Client{Timeout: testenv.HangTimeout}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	respBody, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || strings.Contains(string(respBody), `"isError":true`) {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, respBody)
	}

	select {
	case caller := <-gotCaller:
		if string(caller) != wantID.Credential.Encode() {
			t.Fatalf("HTTP control Caller = %q, want encoded review-a credential", caller)
		}
	case <-time.After(testenv.HangTimeout):
		t.Fatal("HTTP daemon control was not received")
	}

	// Stdio mode with --identity also forwards the loaded Caller credential.
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = stdinR.Close()
		_ = stdoutR.Close()
	}()
	stdioCmd := NewCommand(Dependencies{DialPassive: dial, Stdin: stdinR, Stdout: stdoutW, Stderr: stderrFile, Now: time.Now})
	stdioCmd.SetArgs([]string{"mcp", "--identity", "review-a"})
	stdioDone := make(chan error, 1)
	go func() {
		defer func() { _ = stdoutW.Close() }()
		stdioDone <- stdioCmd.ExecuteContext(t.Context())
	}()
	if _, err := stdinW.WriteString(body + "\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case caller := <-gotCaller:
		if string(caller) != wantID.Credential.Encode() {
			t.Fatalf("stdio control Caller = %q, want encoded review-a credential", caller)
		}
	case <-time.After(testenv.HangTimeout):
		t.Fatal("stdio daemon control was not received")
	}
	_ = stdinW.Close()
	_, _ = io.ReadAll(stdoutR)
	if err := <-stdioDone; err != nil {
		t.Fatalf("stdio mcp --identity failed: %v", err)
	}
}

func TestMCPHTTPTokenFileParentResolutionSwapAndExclusivity(t *testing.T) {
	prepareCommandEnvironment(t)
	if _, _, err := executeCommand(t, Dependencies{}, "mcp", "identity", "create", "agent-a", "--json"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := executeCommand(t, Dependencies{}, "mcp", "identity", "create", "agent-b", "--json"); err != nil {
		t.Fatal(err)
	}

	root := canonicalTempDir(t)
	realDir := filepath.Join(root, "real")
	if err := os.MkdirAll(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	symParent := filepath.Join(root, "sym-parent")
	if err := os.Symlink(realDir, symParent); err != nil {
		t.Fatal(err)
	}

	// 1. Symlinked parent directory (such as macOS /tmp or /work/repos) is
	// resolved once, then created/managed via O_NOFOLLOW descriptors under the
	// canonical directory — including when a leaf subdir does not yet exist.
	symTokenPath := filepath.Join(symParent, "sub", "tok.json")
	symCtx, symCancel := context.WithCancel(t.Context())
	defer symCancel()
	stderrSym, err := os.CreateTemp(root, "stderr-sym-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stderrSym.Close() }()
	symCmd := NewCommand(Dependencies{Stderr: stderrSym, Now: time.Now})
	symCmd.SetArgs([]string{"mcp", "--http", "127.0.0.1:0", "--identity", "agent-a", "--token-file", symTokenPath})
	symDone := make(chan error, 1)
	go func() { symDone <- symCmd.ExecuteContext(symCtx) }()

	symRec := waitForMCPHTTPTokenFile(t, symTokenPath, symDone, stderrSym)
	canonicalTokenPath := filepath.Join(realDir, "sub", "tok.json")
	info, err := os.Lstat(canonicalTokenPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || symRec.Identity != "agent-a" {
		symCancel()
		<-symDone
		t.Fatalf("symlinked parent did not create regular 0600 file at canonical path %s: info=%v err=%v", canonicalTokenPath, info, err)
	}
	symCancel()
	if err := <-symDone; err != nil {
		t.Fatalf("symlinked-parent listener exit error: %v", err)
	}
	if _, err := os.Lstat(canonicalTokenPath); !os.IsNotExist(err) {
		t.Fatalf("symlinked-parent token file not cleaned up on exit: %v", err)
	}

	// 2. A mid-operation symlink swap in the canonical path is still refused by
	// the strict O_NOFOLLOW descriptor walk.
	if f, err := openTokenDirNoFollow(filepath.Join(symParent, "sub")); err == nil || !strings.Contains(err.Error(), "sym-parent") {
		if f != nil {
			_ = f.Close()
		}
		t.Fatalf("openTokenDirNoFollow accepted symlink component: err=%v", err)
	}

	// 3. The token file itself must never be a symlink (pointing to an existing or missing target).
	targetFile := filepath.Join(realDir, "target.txt")
	const targetSentinel = "do-not-touch\n"
	if err := os.WriteFile(targetFile, []byte(targetSentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	leafLink := filepath.Join(symParent, "leaf-link.json")
	if err := os.Symlink(targetFile, leafLink); err != nil {
		t.Fatal(err)
	}
	if err := runCommandBounded(t, 15*time.Second, "mcp", "--http", "127.0.0.1:0", "--identity", "agent-a", "--token-file", leafLink); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("symlink token leaf accepted or wrong error: %v", err)
	}
	if got, err := os.ReadFile(targetFile); err != nil || string(got) != targetSentinel { //nolint:gosec // test temp file
		t.Fatalf("symlink leaf target altered: got=%q err=%v", got, err)
	}

	// 4. Existing unrelated regular file must be refused and left untouched.
	existing := filepath.Join(realDir, "unrelated.txt")
	const sentinel = "keep-me-intact\n"
	if err := os.WriteFile(existing, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runCommandBounded(t, 15*time.Second, "mcp", "--http", "127.0.0.1:0", "--identity", "agent-a", "--token-file", existing); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("existing unrelated regular file overwritten or wrong error: %v", err)
	}
	got, err := os.ReadFile(existing) //nolint:gosec // test temp file
	if err != nil || string(got) != sentinel {
		t.Fatalf("unrelated file altered: got=%q err=%v", got, err)
	}

	// 5. Two live listeners configured with the same token path: second must fail without overwriting first.
	sharedPath := filepath.Join(realDir, "shared.json")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stderr1, err := os.CreateTemp(root, "stderr1-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stderr1.Close() }()
	cmd1 := NewCommand(Dependencies{Stderr: stderr1, Now: time.Now})
	cmd1.SetArgs([]string{"mcp", "--http", "127.0.0.1:0", "--identity", "agent-a", "--token-file", sharedPath})
	done1 := make(chan error, 1)
	go func() { done1 <- cmd1.ExecuteContext(ctx) }()
	defer func() {
		cancel()
		<-done1
	}()

	rec1 := waitForMCPHTTPTokenFile(t, sharedPath, done1, stderr1)

	if err := runCommandBounded(t, 15*time.Second, "mcp", "--http", "127.0.0.1:0", "--identity", "agent-b", "--token-file", sharedPath); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second listener overwrote live listener token file or wrong error: %v", err)
	}
	rawAfter, err := os.ReadFile(sharedPath) //nolint:gosec // test temp file
	if err != nil {
		t.Fatal(err)
	}
	var recAfter mcpHTTPTokenFile
	if err := json.Unmarshal(rawAfter, &recAfter); err != nil || recAfter.Identity != "agent-a" || recAfter.Token != rec1.Token {
		t.Fatalf("first listener token file was altered by second listener: %+v", recAfter)
	}
}

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	canonical, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve canonical temp dir: %v", err)
	}
	return canonical
}

func waitForMCPHTTPTokenFile(t *testing.T, tokenPath string, done chan error, stderrFile *os.File) mcpHTTPTokenFile {
	t.Helper()
	var record mcpHTTPTokenFile
	deadline := time.Now().Add(testenv.HangTimeout)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(tokenPath) //nolint:gosec // test temp file
		if err == nil && json.Unmarshal(raw, &record) == nil && record.Token != "" {
			return record
		}
		select {
		case runErr := <-done:
			done <- runErr
			stderrBytes, _ := os.ReadFile(stderrFile.Name())
			t.Fatalf("MCP HTTP listener exited before publishing %s: err=%v stderr=%q", tokenPath, runErr, stderrBytes)
		case <-time.After(15 * time.Millisecond):
		}
	}
	stderrBytes, _ := os.ReadFile(stderrFile.Name())
	t.Fatalf("token file %s never populated within %s (stderr=%q)", tokenPath, testenv.HangTimeout, stderrBytes)
	return mcpHTTPTokenFile{}
}

func runCommandBounded(t *testing.T, timeout time.Duration, args ...string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	stderrFile, err := os.CreateTemp(t.TempDir(), "stderr-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stderrFile.Close() }()
	cmd := NewCommand(Dependencies{Stderr: stderrFile, Now: time.Now})
	cmd.SetArgs(args)
	return cmd.ExecuteContext(ctx)
}
