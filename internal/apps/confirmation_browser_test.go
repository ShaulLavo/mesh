package apps

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfirmationBrowserPages(t *testing.T) {
	for _, action := range []string{"public", "private", "renew", "delete"} {
		t.Run(action, func(t *testing.T) {
			response := httptest.NewRecorder()
			render(response, pageData{Confirm: action, App: Record{ID: "7k3d"}, CSRF: "test-csrf", URL: URL("7k3d"), Return: URL("7k3d") + "/"})
			if !strings.Contains(response.Body.String(), "Confirm "+action) { t.Fatal("missing confirmation action") }
			if directory := os.Getenv("MESH_CONFIRM_BROWSER_DIR"); directory != "" {
				if err := os.WriteFile(filepath.Join(directory, action+".html"), response.Body.Bytes(), 0600); err != nil { //nolint:gosec // The verifier selects its own scratch directory for browser fixtures.
					t.Fatal(err)
				}
			}
		})
	}
}
