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
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appspkg "github.com/shaul/mesh/internal/apps"
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
			return appspkg.Result{App: &appspkg.Record{ID: "7k3d", Owner: "owner", Kind: "server", Visibility: "private", Status: "active", ExpiresAt: time.Now().Add(24 * time.Hour)}}, nil
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
	if result.URL != "https://7k3d.shaulavo.dev" || result.App.Visibility != "private" {
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
		{[]string{"app", "public", "pc", "7k3d"}, "public", "7k3d", "", ""},
		{[]string{"app", "private", "pc", "7k3d"}, "private", "7k3d", "", ""},
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
	transport.request = func(_ context.Context, r appspkg.Request) (appspkg.Result, error) {
		return appspkg.Result{Data: []byte("complete"), Done: true}, nil
	}
	if err := downloadApp(context.Background(), transport, "7k3d", dest); err != nil {
		t.Fatal(err)
	}
	if err := downloadApp(context.Background(), transport, "7k3d", dest); !errors.Is(err, os.ErrExist) {
		t.Fatalf("overwrote existing archive: %v", err)
	}
	contents, err := os.ReadFile(dest) //nolint:gosec // Read the test-selected download destination to verify atomic publication.
	if err != nil || string(contents) != "complete" {
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
