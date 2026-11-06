package serve

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileHandlersResolveOnlyEscapedPath(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"hello world.html":    "space",
		"hello%20world.html":  "percent escape",
		"100%.html":           "plain percent",
		"question?.html":      "question",
		"hash#.html":          "hash",
		"nested%2fvalue.html": "literal separator",
	}
	writeListingFiles(t, root, files)
	for _, directory := range []string{"sub dir", "sub%20dir", "100%"} {
		writeListingFiles(t, filepath.Join(root, directory), map[string]string{"index.html": directory})
	}
	for _, kind := range []Kind{Static, Files} {
		checkLiteralHandlerQueries(t, root, kind, files)
	}
}

func checkLiteralHandlerQueries(t *testing.T, root string, kind Kind, files map[string]string) {
	t.Helper()
	for _, prefix := range []string{"/site", "/nested/site", "/"} {
		handler, err := Handler(Service{Name: "site", Kind: kind, Target: root}, prefix)
		if err != nil {
			t.Fatal(err)
		}
		checkLiteralFileQueries(t, handler, prefix, root, files)
		for _, directory := range []string{"sub dir", "sub%20dir", "100%"} {
			checkLiteralDirectoryQuery(t, handler, prefix, root, directory, kind)
		}
	}
}

func checkLiteralFileQueries(t *testing.T, handler http.Handler, prefix, root string, files map[string]string) {
	t.Helper()
	for name, contents := range files {
		for _, query := range []string{"", "?", "?a=1", "?file=../secret&escape=%zz&name=%2520%25%00"} {
			target := joinURLPrefix(prefix, url.PathEscape(name)) + query
			checkListedDownload(t, handler, target, root, contents)
		}
	}
}

func checkLiteralDirectoryQuery(t *testing.T, handler http.Handler, prefix, root, directory string, kind Kind) {
	t.Helper()
	target := joinURLPrefix(prefix, url.PathEscape(directory))
	query := "?file=../secret&escape=%zz&name=%2520%25%00"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target+query, nil))
	location := response.Header().Get("Location")
	if response.Code != http.StatusPermanentRedirect || location != target+"/"+query {
		t.Fatalf("redirect %q = %d %q, want 308 %q", target, response.Code, location, target+"/"+query)
	}
	if kind == Files {
		links := listedLinks(t, handler, location)
		checkListedDownload(t, handler, links["index.html"], filepath.Join(root, directory), directory)
		return
	}
	checkListedDownload(t, handler, location, filepath.Join(root, directory), directory)
}

func TestFileHandlersValidateEveryEncodingLayer(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"%2e%2e":                    "refused",
		"value%00.txt":              "refused",
		"value%5c.txt":              "refused",
		"100%value%00.txt":          "refused",
		"100%value%5c.txt":          "refused",
		"100%value%252f..%252f.txt": "refused",
	}
	encoded := "value%20.txt"
	for range maxPathDecodings {
		encoded = strings.ReplaceAll(encoded, "%", "%25")
	}
	files[encoded] = "refused"
	writeListingFiles(t, root, files)
	for _, kind := range []Kind{Static, Files} {
		checkHTTPEncodingRefusals(t, root, kind, files)
	}
}

func checkHTTPEncodingRefusals(t *testing.T, root string, kind Kind, files map[string]string) {
	t.Helper()
	handler, err := Handler(Service{Name: "site", Kind: kind, Target: root}, "/site")
	if err != nil {
		t.Fatal(err)
	}
	for name := range files {
		target := "/site/" + url.PathEscape(name)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
		if response.Code != http.StatusBadRequest && response.Code != http.StatusNotFound {
			t.Errorf("%s GET %q = %d %q, want 400 or 404", kind, target, response.Code, response.Body.String())
		}
	}
}
