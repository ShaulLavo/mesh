package apps

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfirmationBrowserPages(t *testing.T) {
	const renew, remove = "renew", "delete"
	for _, action := range []string{renew, remove} {
		t.Run(action, func(t *testing.T) { checkConfirmationBrowserPage(t, action) })
	}
	writeEscapingBrowserPages(t)
	requireBrowserFixtureFiles(t)
}

func checkConfirmationBrowserPage(t *testing.T, action string) {
	t.Helper()
	response := httptest.NewRecorder()
	render(response, pageData{Confirm: action, App: Record{ID: "7k3d"}, CSRF: "test-csrf", URL: URL("7k3d"), Return: URL("7k3d") + "/"})
	if !strings.Contains(response.Body.String(), "Confirm "+action) {
		t.Fatal("missing confirmation action")
	}
	if directory := os.Getenv("MESH_CONFIRM_BROWSER_DIR"); directory != "" {
		if err := os.WriteFile(filepath.Join(directory, action+".html"), response.Body.Bytes(), 0600); err != nil { //nolint:gosec // The verifier selects its own scratch directory for browser fixtures.
			t.Fatal(err)
		}
	}
}

func writeEscapingBrowserPages(t *testing.T) {
	directory := os.Getenv("MESH_CONFIRM_BROWSER_DIR")
	if directory == "" {
		return
	}
	fixtures := map[string]pageData{
		"escaping-delete":  {Confirm: "delete", App: Record{ID: rendererSpecials}, CSRF: rendererSpecials, URL: "https://app.mesh.test/a b?one=1&two=2", Return: rendererSpecials, Error: rendererSpecials},
		"escaping-renew":   {Confirm: rendererSpecials, App: Record{ID: rendererSpecials}, CSRF: rendererSpecials, URL: "notes:plain", Return: rendererSpecials},
		"escaping-start":   {PairStart: true, Return: rendererSpecials},
		"escaping-pending": {Code: rendererSpecials, Return: rendererSpecials, PairLifetimeMS: 543210},
		"escaping-list":    {Domain: rendererSpecials, Apps: []Record{{ID: rendererSpecials, Status: "active"}}},
	}
	for name, data := range fixtures {
		writeBrowserFixture(t, directory, name, renderPageBytes(data))
	}
	expected, err := json.Marshal(map[string]string{"value": rendererSpecials})
	if err != nil {
		t.Fatal(err)
	}
	writeBrowserFixture(t, directory, "escaping.json", expected)
}

func writeBrowserFixture(t *testing.T, directory, name string, body []byte) {
	t.Helper()
	if !strings.HasSuffix(name, ".json") {
		name += ".html"
	}
	if err := os.WriteFile(filepath.Join(directory, name), body, 0600); err != nil { //nolint:gosec // The verifier selects its scratch directory; fixture names are fixed test cases.
		t.Fatal(err)
	}
}

func requireBrowserFixtureFiles(t *testing.T) {
	t.Helper()
	directory := os.Getenv("MESH_CONFIRM_BROWSER_DIR")
	if directory == "" {
		return
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	for _, name := range []string{"renew.html", "delete.html", "escaping-delete.html", "escaping-renew.html", "escaping-start.html", "escaping-pending.html", "escaping-list.html", "escaping.json"} {
		body, err := root.ReadFile(name)
		if err != nil || len(body) == 0 {
			t.Fatalf("required browser fixture %s missing or empty: %v", name, err)
		}
	}
}
