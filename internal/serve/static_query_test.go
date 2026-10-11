package serve

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestFileHandlersResolveOnlyEscapedPath(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"hello world.html", "question?.html", "hash#.html"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("asset"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, kind := range []Kind{Static, Files} {
		t.Run(string(kind), func(t *testing.T) {
			handler, err := Handler(Service{Name: "site", Kind: kind, Target: root}, "/site")
			if err != nil {
				t.Fatal(err)
			}
			for _, target := range []string{
				"/site/hello%20world.html", "/site/hello%20world.html?", "/site/hello%20world.html?a=1",
				"/site/hello%20world.html?file=../secret&escape=%zz", "/site/question%3F.html?", "/site/hash%23.html?a=1",
			} {
				t.Run(target, func(t *testing.T) {
					request := httptest.NewRequest(http.MethodGet, target, nil)
					request.URL.Fragment = "ignored"
					response := httptest.NewRecorder()
					handler.ServeHTTP(response, request)
					if response.Code != http.StatusOK || response.Body.String() != "asset" {
						t.Fatalf("GET %s = %d %q, want 200 asset", target, response.Code, response.Body.String())
					}
				})
			}
		})
	}
}
