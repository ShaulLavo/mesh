package serve

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestDemandFailureKeepsDiagnosticsInOwnerLog(t *testing.T) {
	const diagnostic = "route /dev did not start: exit status 7 (session TESTSESSION, command \"SECRET=x ./dev\")\n\nlast output:\nprivate log line"
	registry, err := NewRegistry([]Service{onDemand()})
	if err != nil {
		t.Fatal(err)
	}
	registry.SetDemandGate(&fakeGate{err: errString(diagnostic)})
	var ownerLog bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&ownerLog)
	t.Cleanup(func() { log.SetOutput(previous) })

	var previousID string
	for range 2 {
		response := httptest.NewRecorder()
		registry.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/dev/", nil))
		if response.Code != http.StatusBadGateway {
			t.Fatalf("failed start answered %d, want 502", response.Code)
		}
		for _, private := range []string{"/dev", "TESTSESSION", "SECRET=x ./dev", "private log line", "exit status 7"} {
			if strings.Contains(response.Body.String(), private) {
				t.Errorf("HTTP failure disclosed %q", private)
			}
		}
		match := regexp.MustCompile(`^on-demand service unavailable; reference ([A-Z2-7]{26,})\n$`).FindStringSubmatch(response.Body.String())
		if match == nil {
			t.Fatalf("failure has no generic message and opaque reference: %q", response.Body.String())
		}
		if match[1] == previousID {
			t.Fatal("failure references repeat")
		}
		previousID = match[1]
		if !strings.Contains(ownerLog.String(), "on-demand failure "+match[1]+": "+diagnostic) {
			t.Fatalf("owner log lost the correlated diagnostic: %q", ownerLog.String())
		}
		if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("failure headers = %v", response.Header())
		}
	}
}

func TestDemandFailurePreservesCancellationStatus(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		response := httptest.NewRecorder()
		WriteDemandFailure(response, fmt.Errorf("private diagnostic: %w", cause))
		if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "private diagnostic") {
			t.Fatalf("cancelled admission answered %d %q", response.Code, response.Body.String())
		}
	}
}

func TestPublicDirectoriesDenyLateCredentialFiles(t *testing.T) {
	for _, kind := range []Kind{Files, Static} {
		t.Run(string(kind), func(t *testing.T) {
			root := t.TempDir()
			preview, err := InspectService(context.Background(), root, Service{
				Name: "docs", Kind: kind, Target: root, PublicName: "docs.shaulavo.dev",
			}, false)
			if err != nil {
				t.Fatal(err)
			}
			registry, err := NewRegistry([]Service{preview.Service})
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{".env", ".ENV.local", ".git/config", ".ssh/config", "id_rsa", "cert.PEM", "nested/.env", "ordinary.txt", "index.html"} {
				filename := filepath.Join(root, name)
				if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filename, []byte("late fixture bytes"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(".env", filepath.Join(root, "environment.txt")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(".git", filepath.Join(root, "repository")); err != nil {
				t.Fatal(err)
			}
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				for _, target := range []string{".env", ".ENV.local", ".git/config", ".ssh/config", "id_rsa", "cert.PEM", "nested/.env", "%2eenv", "%252eenv", ".git%2fconfig", ".%2fgit/../.env", "nested//./.env", "environment.txt", "repository/config"} {
					request := httptest.NewRequest(method, "http://docs.shaulavo.dev/docs/"+target, nil)
					response := httptest.NewRecorder()
					registry.ServeHTTP(response, request)
					if response.Code != http.StatusNotFound {
						t.Errorf("%s %s answered %d, want 404", method, target, response.Code)
					}
				}
			}
			for _, target := range []string{"ordinary.txt", "index.html"} {
				response := httptest.NewRecorder()
				registry.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://docs.shaulavo.dev/docs/"+target, nil))
				if response.Code != http.StatusOK || response.Body.String() != "late fixture bytes" {
					t.Fatalf("ordinary file %s answered %d %q", target, response.Code, response.Body.String())
				}
			}
			if kind == Files {
				response := httptest.NewRecorder()
				registry.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://docs.shaulavo.dev/docs/", nil))
				for _, name := range []string{".env", ".ENV.local", ".git", ".ssh", "id_rsa", "cert.PEM"} {
					if strings.Contains(response.Body.String(), name) {
						t.Errorf("public listing disclosed credential entry %q", name)
					}
				}
				if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "ordinary.txt") {
					t.Fatalf("ordinary listing answered %d %q", response.Code, response.Body.String())
				}
			}
		})
	}
}

func TestPrivateFilesRetainCredentialAccess(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("private fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	handler, err := Handler(Service{Name: "files", Kind: Files, Target: root}, "/files")
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/files/.env", nil))
	if response.Code != http.StatusOK || response.Body.String() != "private fixture" {
		t.Fatalf("private file answered %d %q", response.Code, response.Body.String())
	}
}
