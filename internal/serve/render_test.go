package serve

import (
	"bytes"
	"embed"
	"html/template"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

var referenceDirectory = template.Must(template.ParseFiles("testdata/directory.tmpl"))

//go:embed testdata/*.golden.html
var directoryGoldens embed.FS

func TestDirectoryPageGoldens(t *testing.T) {
	for _, name := range []string{"empty-root", "nonempty-root", "nested"} {
		t.Run(name, func(t *testing.T) { checkDirectoryGolden(t, name) })
	}
}

func checkDirectoryGolden(t *testing.T, name string) {
	t.Helper()
	root := t.TempDir()
	if name != "empty-root" {
		writeListingFiles(t, root, map[string]string{"a.txt": "a", "hello world.txt": "b"})
	}
	target := "/files/"
	if name == "nested" {
		writeListingFiles(t, filepath.Join(root, "sub%20dir"), map[string]string{"nested.txt": "n"})
		target = "/files/sub%2520dir/"
	}
	handler, err := Handler(Service{Name: "files", Kind: Files, Target: root}, "/files")
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
	want, err := directoryGoldens.ReadFile("testdata/" + name + ".golden.html")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(response.Body.Bytes(), want) {
		t.Fatalf("listing %s differs from original bytes", name)
	}
	head := httptest.NewRecorder()
	handler.ServeHTTP(head, httptest.NewRequest(http.MethodHead, target, nil))
	if head.Code != response.Code || head.Body.Len() != 0 || head.Header().Get("Content-Type") != response.Header().Get("Content-Type") {
		t.Fatal("listing HEAD differs from GET")
	}
	for _, header := range []string{"Set-Cookie", "Content-Security-Policy", "X-Content-Type-Options"} {
		if head.Header().Get(header) != "" || response.Header().Get(header) != "" {
			t.Fatalf("listing acquired %s", header)
		}
	}
}

func TestDirectoryRendererURLContexts(t *testing.T) {
	for _, href := range []string{"/files/a b?x=1&y=2", "HTTP://example.test/a'()雪%20%zz", "mailto:note@example.test", "notes:plain", "data:text/plain,notes"} {
		t.Run(href, func(t *testing.T) { checkDirectoryReference(t, href) })
	}
}

func checkDirectoryReference(t *testing.T, href string) {
	t.Helper()
	data := directoryPage{Title: `quotes " ' & + <mesh-note>雪` + "\u2028\u2029", Entries: []directoryEntry{{Name: `<mesh-note title="plain">雪 & + ' "`, Href: href}}}
	var want, got bytes.Buffer
	if err := referenceDirectory.Execute(&want, data); err != nil {
		t.Fatal(err)
	}
	renderDirectory(&got, data)
	if !bytes.Equal(want.Bytes(), got.Bytes()) {
		t.Fatalf("listing differs from reference\ngot: %s\nwant: %s", got.Bytes(), want.Bytes())
	}
	if strings.Contains(got.String(), "<mesh-note") {
		t.Fatal("name became markup")
	}
}
