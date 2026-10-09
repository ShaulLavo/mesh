package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	appspkg "github.com/shaul/mesh/internal/apps"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/sshd"
	"golang.org/x/crypto/ssh"
)

func TestAppCreateUploadsCallerSourceWithOffsetsAndDigest(t *testing.T) {
	dir := t.TempDir()
	data := make([]byte, appspkg.ChunkSize+1024)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "asset.bin"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	var archive []byte
	created := false
	calls := 0
	uploadID := strings.Repeat("a", 43)
	out, _, err := executeCommand(t, Dependencies{AppRequest: func(_ context.Context, host string, r appspkg.Request) (appspkg.Result, error) {
		calls++
		if host != "pc" {
			t.Fatalf("wrong host: %s", host)
		}
		switch r.Action {
		case "upload.begin":
			return appspkg.Result{UploadID: uploadID}, nil
		case "upload.chunk":
			if r.UploadID != uploadID || r.Offset != int64(len(archive)) || len(r.Data) > appspkg.ChunkSize {
				t.Fatalf("invalid chunk: offset=%d len=%d", r.Offset, len(r.Data))
			}
			archive = append(archive, r.Data...)
			return appspkg.Result{}, nil
		case "create":
			sum := sha256.Sum256(archive)
			if r.Digest != hex.EncodeToString(sum[:]) || r.UploadID != uploadID || r.Kind != "server" || r.Port != 3000 || r.Command != "bun run start" || r.Setup != "bun install" {
				t.Fatalf("invalid creation: %#v", r)
			}
			created = true
			return appspkg.Result{App: &appspkg.Record{ID: "7k3d", Owner: "owner", Kind: "server", Status: "active", ExpiresAt: time.Now().Add(24 * time.Hour)}}, nil
		default:
			t.Fatalf("unexpected %s", r.Action)
			return appspkg.Result{}, nil
		}
	}}, "app", "create", "pc", dir, "--run", "bun run start", "--port", "3000", "--setup", "bun install", "--env", "NODE_ENV=production", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if !created || calls < 4 {
		t.Fatalf("not chunked: created=%t calls=%d", created, calls)
	}
	var result struct {
		URL string
		App appspkg.Record
	}
	if err = json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if result.URL != "https://7k3d.mesh.test" {
		t.Fatalf("output: %s", out)
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	header, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	if header.Name != "asset.bin" {
		t.Fatalf("source archive contains unexpected %s", header.Name)
	}
	contents, err := io.ReadAll(tr)
	if err != nil || !bytes.Equal(contents, data) {
		t.Fatalf("source changed: %v", err)
	}
	if _, err = tr.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("credential file included: %v", err)
	}
}
func TestAppInvalidRecipeAndUnsafeSourceNeverUpload(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "identity.key"), []byte("private key"), 0600); err != nil {
		t.Fatal(err)
	}
	dependencies := Dependencies{AppRequest: func(context.Context, string, appspkg.Request) (appspkg.Result, error) {
		t.Error("invalid source or recipe sent a request")
		return appspkg.Result{}, nil
	}}
	tests := [][]string{
		{"app", "create", "pc", dir, "--run", "bun run start"},
		{"app", "create", "pc", dir, "--port", "3000"},
		{"app", "create", "pc", dir, "--env", "BAD"},
		{"app", "create", "pc", dir},
	}
	for _, args := range tests {
		_, _, err := executeCommand(t, dependencies, args...)
		if err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
func TestAppCLIManagementAndUpdateMapping(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		args    []string
		action  string
		id      string
		code    string
		browser string
	}{
		{[]string{"app", "renew", "pc", "7k3d"}, "renew", "7k3d", "", ""},
		{[]string{"app", "delete", "pc", "7k3d"}, "delete", "7k3d", "", ""},
		{[]string{"app", "info", "pc", "7k3d"}, "inspect", "7k3d", "", ""},
		{[]string{"app", "browser", "revoke", "pc", "browser-id"}, "browser.revoke", "", "", "browser-id"},
		{[]string{"app", "update", "pc", "7k3d", dir}, "update", "7k3d", "", ""},
	}
	for _, test := range tests {
		t.Run(test.action, func(t *testing.T) {
			called := false
			_, _, err := executeCommand(t, Dependencies{AppRequest: func(_ context.Context, host string, r appspkg.Request) (appspkg.Result, error) {
				if r.Action == "upload.begin" {
					return appspkg.Result{UploadID: strings.Repeat("a", 43)}, nil
				}
				if r.Action == "upload.chunk" {
					return appspkg.Result{}, nil
				}
				if host != "pc" || r.Action != test.action || r.ID != test.id || r.Code != test.code || r.BrowserID != test.browser {
					t.Fatalf("wrong management request: %#v", r)
				}
				called = true
				return appspkg.Result{}, nil
			}}, test.args...)
			if err != nil || !called {
				t.Fatalf("command: called=%t err=%v", called, err)
			}
		})
	}
}

func TestAppCLIRejectsRemovedSharingCommands(t *testing.T) {
	prepareCommandEnvironment(t)
	dependencies := Dependencies{AppRequest: func(context.Context, string, appspkg.Request) (appspkg.Result, error) {
		t.Fatal("removed sharing command contacted a host")
		return appspkg.Result{}, nil
	}}
	for _, operation := range []string{"public", "private"} {
		_, _, err := executeCommand(t, dependencies, "app", operation, "pc", "7k3d")
		if err == nil || !strings.Contains(err.Error(), "unknown command") {
			t.Fatalf("removed %s command: %v", operation, err)
		}
	}
}
func TestAppDownloadIsAtomicAndDoesNotOverwrite(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "source.tar.gz")
	transport := appTransport{request: func(_ context.Context, r appspkg.Request) (appspkg.Result, error) {
		if r.Offset == 0 {
			return appspkg.Result{Data: []byte("archive")}, nil
		}
		return appspkg.Result{}, errors.New("connection lost")
	}}
	if err := downloadApp(context.Background(), transport, "7k3d", dest); err == nil {
		t.Fatal("partial download succeeded")
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial download published: %v", err)
	}
	complete := gzipBytes(t, "complete")
	transport.request = func(_ context.Context, r appspkg.Request) (appspkg.Result, error) {
		return appspkg.Result{Data: complete, Done: true}, nil
	}
	if err := downloadApp(context.Background(), transport, "7k3d", dest); err != nil {
		t.Fatal(err)
	}
	if err := downloadApp(context.Background(), transport, "7k3d", dest); !errors.Is(err, os.ErrExist) {
		t.Fatalf("overwrote existing archive: %v", err)
	}
	contents, err := os.ReadFile(dest) //nolint:gosec // Read the test-selected download destination to verify atomic publication.
	if err != nil || !bytes.Equal(contents, complete) {
		t.Fatalf("archive: %q %v", contents, err)
	}
}
func TestAppSSHChecksExactMeshHostIdentity(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	wrong, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	callback, err := appHostKeyCallback(base64.RawURLEncoding.EncodeToString(public))
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	if err = callback("pc:2222", &net.TCPAddr{}, key); err != nil {
		t.Fatalf("pinned key rejected: %v", err)
	}
	key, err = ssh.NewPublicKey(wrong)
	if err != nil {
		t.Fatal(err)
	}
	if err = callback("pc:2222", &net.TCPAddr{}, key); err == nil {
		t.Fatal("different Mesh host accepted")
	}
	if _, err = appHostKeyCallback("missing pin"); err == nil {
		t.Fatal("missing Mesh pin accepted")
	}
}

func TestAppInspectTellsTheOwnerWhyAnAppIsNotServed(t *testing.T) {
	var out bytes.Buffer
	result := appspkg.Result{App: &appspkg.Record{ID: "7k3d", Status: "active"}, Runtime: &appspkg.RuntimeInfo{Phase: "ready", Problem: "port 5173 is held by a process outside the app\x1b[2J"}}
	if err := writeAppResult(&out, "laptop", result, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "not serving: port 5173 is held by a process outside the app") || strings.Contains(out.String(), "\x1b") {
		t.Fatalf("inspect output does not show the safe reason:\n%q", out.String())
	}
	out.Reset()
	result.Runtime.Problem = ""
	if err := writeAppResult(&out, "laptop", result, false); err != nil || strings.Contains(out.String(), "not serving") {
		t.Fatalf("served app reported as not serving: %q %v", out.String(), err)
	}
}

func TestAppInspectShowsSetupFailure(t *testing.T) {
	var out bytes.Buffer
	result := appspkg.Result{App: &appspkg.Record{ID: "7k3d", Status: "deleted"}, Runtime: &appspkg.RuntimeInfo{Phase: "failed", Failure: &appspkg.SetupFailure{Error: "app: setup exited 1", Output: "npm ERR! missing script: build\n\x1b]0;title\x07\n"}}}
	if err := writeAppResult(&out, "local", result, false); err != nil {
		t.Fatal(err)
	}
	shown := out.String()
	if !strings.Contains(shown, "setup failed: app: setup exited 1") || !strings.Contains(shown, "  npm ERR! missing script: build") {
		t.Fatalf("setup failure not shown: %q", shown)
	}
	if strings.ContainsRune(shown, '\x1b') {
		t.Fatalf("setup output reached the terminal unescaped: %q", shown)
	}
}

func gzipBytes(t *testing.T, text string) []byte {
	t.Helper()
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	if _, err := gz.Write([]byte(text)); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestAppDownloadRefusesArchiveMixedFromTwoSnapshots(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "source.tar.gz")
	before, after := gzipBytes(t, strings.Repeat("before ", 4096)), gzipBytes(t, strings.Repeat("after! ", 4096))
	half := len(before) / 2
	transport := appTransport{request: func(_ context.Context, r appspkg.Request) (appspkg.Result, error) {
		if r.Offset == 0 {
			return appspkg.Result{Data: before[:half]}, nil
		}
		return appspkg.Result{Data: after[r.Offset:], Done: true}, nil
	}}
	err := downloadApp(context.Background(), transport, "7k3d", dest)
	if err == nil || !strings.Contains(err.Error(), "run it again") {
		t.Fatalf("mixed archive was not refused: %v", err)
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mixed archive published: %v", err)
	}
}

func appSSHFixture(t *testing.T, handler sshd.SessionHandler) *ssh.Client {
	t.Helper()
	devicePublic, deviceKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, hostKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := identity.ApproveDevice(root, base64.RawURLEncoding.EncodeToString(devicePublic)); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- sshd.Serve(ctx, sshd.Config{Addr: address, HostKey: hostKey, AuthorizedKeys: filepath.Join(root, "authorized_keys"), Handler: handler})
	}()
	t.Cleanup(func() {
		cancel()
		var serveErr error
		select {
		case serveErr = <-done:
		case <-time.After(time.Second):
			t.Error("owned SSH server did not stop")
		}
		if serveErr != nil {
			t.Error(serveErr)
		}
	})
	signer, err := ssh.NewSignerFromKey(deviceKey)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostKey)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ClientConfig{User: "mesh", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: ssh.FixedHostKey(hostSigner.PublicKey()), Timeout: time.Second}
	deadline := time.Now().Add(3 * time.Second)
	var client *ssh.Client
	for time.Now().Before(deadline) {
		client, err = ssh.Dial("tcp", address, config)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if client == nil {
		t.Fatalf("owned SSH fixture never became ready: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestAppSSHChunkBurstBacksOffAdmission(t *testing.T) {
	var handled atomic.Int64
	client := appSSHFixture(t, func(ctx context.Context, session sshd.Session) (int, error) {
		var request appspkg.Request
		if err := json.NewDecoder(session.AppInput).Decode(&request); err != nil {
			return 1, fmt.Errorf("decode fixture app request: %w", err)
		}
		expected := handled.Add(1) - 1
		if request.Action != "upload.chunk" || request.Offset != expected {
			return 1, errors.New("chunk executed out of order or more than once")
		}
		return 0, json.NewEncoder(session.Out).Encode(appSSHReply{})
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for index := range 32 {
		_, err := remoteAppRequest(ctx, client, appspkg.Request{Action: "upload.chunk", UploadID: strings.Repeat("a", 43), Offset: int64(index), Data: []byte{1}})
		if err != nil {
			t.Fatalf("chunk%d/%d: %v", index, 32, err)
		}
	}
	if handled.Load() != 32 {
		t.Fatalf("handler executed%d chunks", handled.Load())
	}
}

func TestAppSSHAdmissionRetriesAreFinite(t *testing.T) {
	var calls atomic.Int64
	client := appSSHFixture(t, func(_ context.Context, session sshd.Session) (int, error) {
		calls.Add(1)
		return sshFixtureExit(session.Err, 1, sshd.RateLimitMessage)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := remoteAppRequest(ctx, client, appspkg.Request{Action: "list"})
	if !errors.Is(err, errAppSSHRateLimited) || calls.Load() != int64(appSSHAdmissionAttempts) {
		t.Fatalf("refusal: calls=%d err=%v", calls.Load(), err)
	}
}

func TestAppSSHAdmissionWaitHonorsCallerDeadline(t *testing.T) {
	var calls atomic.Int64
	client := appSSHFixture(t, func(_ context.Context, session sshd.Session) (int, error) {
		calls.Add(1)
		return sshFixtureExit(session.Err, 1, sshd.RateLimitMessage)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	_, err := remoteAppRequest(ctx, client, appspkg.Request{Action: "list"})
	if !errors.Is(err, context.DeadlineExceeded) || calls.Load() >= int64(appSSHAdmissionAttempts) {
		t.Fatalf("deadline: calls=%d err=%v", calls.Load(), err)
	}
}

func TestAppSSHDoesNotRetryOperationFailures(t *testing.T) {
	t.Run("application error", func(t *testing.T) { checkAppSSHOperationFailure(t, 0, "", sshd.RateLimitMessage) })
	t.Run("different exit", func(t *testing.T) { checkAppSSHOperationFailure(t, 2, sshd.RateLimitMessage, "") })
	t.Run("different message", func(t *testing.T) {
		checkAppSSHOperationFailure(t, 1, sshd.RateLimitMessage+" during application execution", "")
	})
}

func checkAppSSHOperationFailure(t *testing.T, status int, stderr, rpcError string) {
	t.Helper()
	var calls atomic.Int64
	client := appSSHFixture(t, func(_ context.Context, session sshd.Session) (int, error) {
		calls.Add(1)
		if stderr != "" {
			return sshFixtureExit(session.Err, status, stderr)
		}
		return status, json.NewEncoder(session.Out).Encode(appSSHReply{Error: rpcError})
	})
	_, err := remoteAppRequest(context.Background(), client, appspkg.Request{Action: "create"})
	if err == nil || errors.Is(err, errAppSSHRateLimited) || calls.Load() != 1 {
		t.Fatalf("operation retried: calls=%d err=%v", calls.Load(), err)
	}
}

func sshFixtureExit(output io.Writer, status int, message string) (int, error) {
	if _, err := io.WriteString(output, message+"\n"); err != nil {
		return status, fmt.Errorf("write fixture SSH rejection: %w", err)
	}
	return status, nil
}
