package serve

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestInspectServiceResolvesRemoteHomeAndInfersKind(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "site")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	preview, err := InspectService(context.Background(), home, Service{
		Name: "blog", Target: "./site", PublicName: "blog.shaulavo.dev",
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Service.Kind != Static || preview.Service.Target != root || preview.FileCount != 1 {
		t.Fatalf("directory preview = %#v", preview)
	}

	preview, err = InspectService(context.Background(), home, Service{Name: "api", Target: "03000"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Service.Kind != Proxy || preview.Service.Target != "3000" || preview.FileCount != 0 {
		t.Fatalf("proxy preview = %#v", preview)
	}
}

func TestInspectServiceRejectsMeaninglessFlags(t *testing.T) {
	home := t.TempDir()
	for _, test := range []struct {
		name    string
		service Service
		allow   bool
		want    string
	}{
		{name: "files numeric", service: Service{Name: "files", Kind: Files, Target: "3000"}, want: "numeric"},
		{name: "private wake", service: Service{Name: "site", Kind: Static, Target: home, WakeOnRequest: true}, want: "public"},
		{name: "private credentials override", service: Service{Name: "site", Kind: Static, Target: home}, allow: true, want: "public"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := InspectService(context.Background(), home, test.service, test.allow)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("InspectService error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestInspectServiceFindsCredentialsThroughInRootSymlinks(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "site")
	secrets := filepath.Join(root, "private")
	if err := os.MkdirAll(secrets, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secrets, ".env"), []byte("TOKEN=secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secrets, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}

	service := Service{Name: "site", Kind: Static, Target: root, PublicName: "site.shaulavo.dev"}
	_, err := InspectService(context.Background(), home, service, false)
	if !errors.Is(err, ErrCredentialsFound) || !strings.Contains(err.Error(), `.env`) {
		t.Fatalf("credential error = %v", err)
	}
	preview, err := InspectService(context.Background(), home, service, true)
	if err != nil {
		t.Fatal(err)
	}
	if preview.FileCount != 1 {
		t.Fatalf("allowed preview file count = %d, want 1", preview.FileCount)
	}
}

func TestInspectServiceChecksResolvedRootName(t *testing.T) {
	home := t.TempDir()
	credentialRoot := filepath.Join(home, ".ssh")
	if err := os.Mkdir(credentialRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, "public")
	if err := os.Symlink(credentialRoot, link); err != nil {
		t.Fatal(err)
	}
	_, err := InspectService(context.Background(), home, Service{
		Name: "keys", Target: link, PublicName: "keys.shaulavo.dev",
	}, false)
	if !errors.Is(err, ErrCredentialsFound) || !strings.Contains(err.Error(), ".ssh") {
		t.Fatalf("resolved root credential error = %v", err)
	}
}

func TestInspectServiceRejectsCredentialLikeAncestors(t *testing.T) {
	for _, relative := range []string{filepath.Join("repo", ".git", "objects"), filepath.Join(".ssh", "archive")} {
		t.Run(filepath.ToSlash(relative), func(t *testing.T) {
			home := t.TempDir()
			root := filepath.Join(home, relative)
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			_, err := InspectService(context.Background(), home, Service{
				Name: "site", Target: root, PublicName: "site.shaulavo.dev",
			}, false)
			if !errors.Is(err, ErrCredentialsFound) {
				t.Fatalf("credential ancestor %s error = %v", relative, err)
			}
		})
	}
}

func TestCredentialLikeMatchesOnlyPromisedPatterns(t *testing.T) {
	for _, name := range []string{".env", ".env.local", ".git", ".ssh", "id_ed25519", "server.pem", "SERVER.PEM"} {
		if !credentialLike(name) {
			t.Errorf("credentialLike(%q) = false", name)
		}
	}
	for _, name := range []string{".gitignore", "environment", "identity", "pem", "server.pem.txt"} {
		if credentialLike(name) {
			t.Errorf("credentialLike(%q) = true", name)
		}
	}
}

func TestInspectServiceFailsClosedOnSymlinkCycleAndSpecialFile(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "site")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	service := Service{Name: "site", Kind: Static, Target: root, PublicName: "site.shaulavo.dev"}
	loop := filepath.Join(root, "loop")
	if err := os.Symlink("loop", loop); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectService(context.Background(), home, service, true); err == nil {
		t.Fatal("symlink cycle was accepted")
	}
	if err := os.Remove(loop); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(root, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectService(context.Background(), home, service, true); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("special-file error = %v", err)
	}
}

func TestInspectServiceFailsClosedOnUnsafeOrUnboundedTrees(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "site")
	outside := filepath.Join(home, "outside")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	service := Service{Name: "site", Kind: Static, Target: root, PublicName: "site.shaulavo.dev"}
	if _, err := InspectService(context.Background(), home, service, true); !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("outside-root symlink error = %v", err)
	}

	if err := os.Remove(filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"one", "two", "three"} {
		if err := os.WriteFile(filepath.Join(root, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := scanPublicDirectory(context.Background(), root, 2, 8); !errors.Is(err, ErrDirectoryLimit) {
		t.Fatalf("entry-limit error = %v", err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := scanPublicDirectory(cancelled, root, 10, 8); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled scan error = %v", err)
	}
}
