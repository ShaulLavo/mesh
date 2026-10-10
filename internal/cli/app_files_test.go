package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appspkg "github.com/shaul/mesh/internal/apps"
)

func TestAppFilesCreateAndUpdateUseNormalUpload(t *testing.T) {
	root := t.TempDir()
	image := filepath.Join(root, "image.png")
	if err := os.WriteFile(image, []byte("image"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"create", "update"} {
		t.Run(operation, func(t *testing.T) {
			testAppFileUpload(t, operation, image)
		})
	}
}

func testAppFileUpload(t *testing.T, operation, image string) {
	t.Helper()
	prepareCommandEnvironment(t)
	staging := t.TempDir()
	t.Setenv("TMPDIR", staging)
	t.Setenv("TMP", staging)
	t.Setenv("TEMP", staging)
	var archive []byte
	deps := Dependencies{AppRequest: func(_ context.Context, host string, request appspkg.Request) (appspkg.Result, error) {
		if host != "pc" {
			t.Fatalf("wrong host: %s", host)
		}
		return appFileRequest(t, operation, request, &archive)
	}}
	args := []string{"app", operation, "pc"}
	if operation == "update" {
		args = append(args, "7k3d")
	}
	args = append(args, image, "--json")
	out, _, err := executeCommand(t, deps, args...)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		URL string         `json:"url"`
		App appspkg.Record `json:"app"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if result.URL != appspkg.URL("7k3d") {
		t.Fatalf("changed URL: %s", out)
	}
	files := appFileArchive(t, archive)
	if !strings.Contains(files["index.html"], `<img src="0-image.png"`) || files["0-image.png"] != "image" {
		t.Fatalf("uploaded page missing selected image: %v", files)
	}
	entries, err := os.ReadDir(staging)
	if err != nil || len(entries) != 0 {
		t.Fatalf("upload retained staging files: %v %v", entries, err)
	}
}

func appFileRequest(t *testing.T, operation string, request appspkg.Request, archive *[]byte) (appspkg.Result, error) {
	t.Helper()
	switch request.Action {
	case "upload.begin":
		return appspkg.Result{UploadID: strings.Repeat("a", 43)}, nil
	case "upload.chunk":
		*archive = append(*archive, request.Data...)
		return appspkg.Result{}, nil
	case operation:
		if request.Kind != "static" || (operation == "update" && request.ID != "7k3d") {
			t.Fatalf("wrong request: %#v", request)
		}
		return appspkg.Result{App: &appspkg.Record{ID: "7k3d", ExpiresAt: time.Now().Add(24 * time.Hour)}}, nil
	default:
		t.Fatalf("unexpected operation: %s", request.Action)
		return appspkg.Result{}, nil
	}
}

func appFileArchive(t *testing.T, data []byte) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gz.Close() }()
	reader := tar.NewReader(gz)
	files := make(map[string]string)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return files
		}
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		files[header.Name] = string(content)
	}
}

func TestAppFilesRejectServerRecipesAndCleanFailedUpload(t *testing.T) {
	prepareCommandEnvironment(t)
	image := filepath.Join(t.TempDir(), "image.png")
	if err := os.WriteFile(image, []byte("image"), 0600); err != nil {
		t.Fatal(err)
	}
	staging := t.TempDir()
	t.Setenv("TMPDIR", staging)
	t.Setenv("TMP", staging)
	t.Setenv("TEMP", staging)
	calls := 0
	deps := Dependencies{AppRequest: func(context.Context, string, appspkg.Request) (appspkg.Result, error) {
		calls++
		return appspkg.Result{}, errors.New("host offline")
	}}
	for _, flags := range [][]string{{"--run", "bun run start", "--port", "3000"}, {"--setup", "bun install"}} {
		args := append([]string{"app", "create", "local", image}, flags...)
		if _, _, err := executeCommand(t, deps, args...); err == nil {
			t.Fatalf("file accepted server recipe: %v", flags)
		}
	}
	if calls != 0 {
		t.Fatal("invalid source contacted host")
	}
	if _, _, err := executeCommand(t, deps, "app", "create", "local", image); err == nil {
		t.Fatal("failed upload succeeded")
	}
	entries, err := os.ReadDir(staging)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed upload retained staging files: %v %v", entries, err)
	}
}

func TestAppMaxSizeTravelsWithCreateAndUpdate(t *testing.T) {
	prepareCommandEnvironment(t)
	file := filepath.Join(t.TempDir(), "clip.webm")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(file, appspkg.MaxArchive+1); err != nil {
		t.Fatal(err)
	}
	staging := t.TempDir()
	t.Setenv("TMPDIR", staging)
	for _, operation := range []string{"create", "update"} {
		testAppMaxSize(t, operation, file)
	}
	entries, err := os.ReadDir(staging)
	if err != nil || len(entries) != 0 {
		t.Fatalf("retained transfer files: %v %v", entries, err)
	}
}

func testAppMaxSize(t *testing.T, operation, file string) {
	t.Helper()
	args := []string{"app", operation, "pc"}
	if operation == "update" {
		args = append(args, "7k3d")
	}
	args = append(args, file)
	calls := 0
	deps := Dependencies{AppRequest: func(_ context.Context, _ string, q appspkg.Request) (appspkg.Result, error) {
		calls++
		if q.Action == "upload.begin" || q.Action == operation {
			if q.MaxBytes != -1 {
				t.Fatalf("size limit lost on %s: %d", q.Action, q.MaxBytes)
			}
		}
		return appspkg.Result{UploadID: strings.Repeat("a", 43)}, nil
	}}
	if _, _, err := executeCommand(t, deps, args...); err == nil || calls != 0 {
		t.Fatalf("default limit allowed large file: calls=%d err=%v", calls, err)
	}
	args = append(args, "--max-size", "unlimited")
	if _, _, err := executeCommand(t, deps, args...); err != nil || calls < 3 {
		t.Fatalf("unlimited upload: calls=%d err=%v", calls, err)
	}
}
