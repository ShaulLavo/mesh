package apps

import (
	"bytes"
	"embed"
	"encoding/json"
	"html/template"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"
)

// Frozen before replacing Execute. This oracle is never linked into mesh.
var referencePage = template.Must(template.ParseFiles("testdata/management.tmpl"))

func pageFixtures() []struct {
	name string
	data pageData
} {
	expiry := time.Date(2026, 11, 7, 12, 34, 56, 123000000, time.UTC)
	active := Record{ID: "7k3d", Status: "active", ExpiresAt: expiry}
	returnTo := "/view?id=7k3d&return=%2Fnotes"
	return []struct {
		name string
		data pageData
	}{
		{"empty", pageData{Domain: "mesh.test"}},
		{"mixed", pageData{Domain: "mesh.test", Apps: []Record{active, {ID: "expired", Status: "expired", ExpiresAt: expiry.Add(-time.Hour)}, {ID: "deleted", Status: "deleted", Cleanup: "pending", ExpiresAt: expiry.Add(-2 * time.Hour)}}}},
		{"pair-start", pageData{PairStart: true, Return: returnTo}},
		{"pair-pending", pageData{Code: "test-pair-code", Return: returnTo, PairLifetimeMS: 543210}},
		{"pair-expired", pageData{Code: "test-pair-code", Return: "/", PairLifetimeMS: 0}},
		{"renew", pageData{Confirm: "renew", App: active, URL: "https://7k3d.mesh.test", Return: "https://7k3d.mesh.test/notes?one=1&two=2", CSRF: "test-csrf"}},
		{"delete", pageData{Confirm: "delete", App: active, URL: "https://7k3d.mesh.test", Return: "/", CSRF: "test-csrf"}},
		{"error", pageData{Error: "Could not read & list <notes>.", Domain: "mesh.test"}},
	}
}

func renderPageBytes(data pageData) []byte {
	response := httptest.NewRecorder()
	render(response, data)
	return response.Body.Bytes()
}

//go:embed testdata/*.golden.html
var managementGoldens embed.FS

func TestManagementPageGoldens(t *testing.T) {
	for _, fixture := range pageFixtures() {
		t.Run(fixture.name, func(t *testing.T) { checkPageGolden(t, fixture.name, fixture.data) })
	}
}

func checkPageGolden(t *testing.T, name string, data pageData) {
	t.Helper()
	got := renderPageBytes(data)
	want, err := managementGoldens.ReadFile("testdata/" + name + ".golden.html")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s differs from original template bytes", name)
	}
	checkReferencePage(t, data, got)
}

func checkReferencePage(t *testing.T, data pageData, got []byte) {
	t.Helper()
	var want bytes.Buffer
	if err := referencePage.Execute(&want, data); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want.Bytes()) {
		t.Fatalf("render differs from frozen template\ngot: %s\nwant: %s", got, want.Bytes())
	}
}

const rendererSpecials = `quotes " ' & + <mesh-note title="plain">雪</mesh-note>` + "\u2028\u2029"

func parsePage(t *testing.T, body []byte) *html.Node {
	t.Helper()
	node, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return node
}

func pageNodes(root *html.Node, tag string) []*html.Node {
	var found []*html.Node
	if root.Type == html.ElementNode && root.Data == tag {
		found = append(found, root)
	}
	for child := root.FirstChild; child != nil; child = child.NextSibling {
		found = append(found, pageNodes(child, tag)...)
	}
	return found
}

func pageText(node *html.Node) string {
	if node.Type == html.TextNode {
		return node.Data
	}
	var text strings.Builder
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		text.WriteString(pageText(child))
	}
	return text.String()
}

func pageAttr(node *html.Node, key string) string {
	for _, attr := range node.Attr {
		if attr.Key == key {
			return attr.Val
		}
	}
	return ""
}

func checkHiddenValues(t *testing.T, root *html.Node, want map[string]string) {
	t.Helper()
	for _, input := range pageNodes(root, "input") {
		name := pageAttr(input, "name")
		expected, ok := want[name]
		if !ok {
			continue
		}
		if pageAttr(input, "value") != expected {
			t.Errorf("hidden %s = %q, want %q", name, pageAttr(input, "value"), expected)
		}
		if len(input.Attr) != 3 {
			t.Errorf("unexpected attributes on hidden %s: %v", name, input.Attr)
		}
		delete(want, name)
	}
	if len(want) != 0 {
		t.Fatalf("missing hidden fields: %v", want)
	}
}

func scriptString(t *testing.T, body []byte, pattern string) string {
	t.Helper()
	match := regexp.MustCompile(pattern).FindSubmatch(body)
	if len(match) != 2 {
		t.Fatalf("missing script string %s", pattern)
	}
	var value string
	if err := json.Unmarshal(match[1], &value); err != nil {
		t.Fatal(err)
	}
	if bytes.ContainsAny(match[1], "<>&\u2028\u2029") {
		t.Fatal("script literal contains HTML or line separator bytes")
	}
	return value
}

func TestManagementRendererEscaping(t *testing.T) {
	values := []string{rendererSpecials, "", "plain", "a\tline\nnext", "100% and %2f", strings.Join(strings.Fields("a b c"), "+")}
	for _, value := range values {
		t.Run(value, func(t *testing.T) { checkManagementContexts(t, value) })
	}
}

func checkManagementContexts(t *testing.T, value string) {
	t.Helper()
	data := pageData{Confirm: "delete", App: Record{ID: value}, URL: "https://app.mesh.test/a b?note=" + url.QueryEscape(value), Return: value, CSRF: value, Error: value}
	body := renderPageBytes(data)
	checkReferencePage(t, data, body)
	root := parsePage(t, body)
	checkHiddenValues(t, root, map[string]string{"id": value, "action": "delete", "csrf": value, "return": value})
	if pageText(pageNodes(root, "h1")[0]) != "delete "+value {
		t.Fatal("confirmation text changed")
	}
	if len(pageNodes(root, "mesh-note")) != 0 || len(pageNodes(root, "script")) != 1 {
		t.Fatal("data became markup")
	}
	if scriptString(t, body, `confirmID.value !== ("(?:\\.|[^"\\])*")`) != value {
		t.Fatal("script ID changed")
	}
	start := renderPageBytes(pageData{PairStart: true, Return: value})
	checkHiddenValues(t, parsePage(t, start), map[string]string{"return": value})
	pending := renderPageBytes(pageData{Code: "test-code", Return: value, PairLifetimeMS: 12345})
	checkReferencePage(t, pageData{Code: "test-code", Return: value, PairLifetimeMS: 12345}, pending)
	if scriptString(t, pending, `location.replace\(("(?:\\.|[^"\\])*")\)`) != value {
		t.Fatal("pair return differs from hidden return")
	}
	list := pageData{Domain: value, Apps: []Record{{ID: value, Status: "active"}, {ID: value, Status: value}}}
	checkReferencePage(t, list, renderPageBytes(list))
	links := pageNodes(parsePage(t, renderPageBytes(list)), "a")
	for _, link := range links[:4] {
		checkQueryID(t, link, value)
	}
	if pageText(links[0]) != "https://"+value+"."+value {
		t.Fatal("list label changed")
	}
}

func checkQueryID(t *testing.T, link *html.Node, want string) {
	t.Helper()
	parsed, err := url.Parse(pageAttr(link, "href"))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Query().Get("id") != want {
		t.Fatalf("query ID = %q, want %q", parsed.Query().Get("id"), want)
	}
	if len(link.Attr) != 1 {
		t.Fatal("ID introduced an attribute")
	}
}

func TestManagementRendererWholeHrefs(t *testing.T) {
	for _, target := range []string{"https://app.mesh.test/a b?x=1&y=2", "HTTP://app.mesh.test/a'()雪%20%zz", "mailto:notes@example.test", "/local/a b?x='雪'", "#note", "notes:plain", "data:text/plain,notes"} {
		t.Run(target, func(t *testing.T) { checkManagementHref(t, target) })
	}
}

func checkManagementHref(t *testing.T, target string) {
	t.Helper()
	data := pageData{Confirm: "renew", URL: target, App: Record{ID: "7k3d"}}
	body := renderPageBytes(data)
	checkReferencePage(t, data, body)
	link := pageNodes(parsePage(t, body), "a")[0]
	if pageText(link) != target || len(link.Attr) != 1 {
		t.Fatal("whole URL changed text or attribute structure")
	}
	if strings.HasPrefix(target, "notes:") || strings.HasPrefix(target, "data:") {
		if pageAttr(link, "href") != "#ZgotmplZ" {
			t.Fatal("unsupported scheme was not filtered")
		}
	}
}
