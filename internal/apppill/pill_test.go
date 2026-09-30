package apppill

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
)

func response(content, kind string) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{kind}}, Body: io.NopCloser(strings.NewReader(content)), ContentLength: int64(len(content))}
}

func TestInjectHTMLPreservesAppAndPermitsOnlyControlAssets(t *testing.T) {
	document := `<!doctype html><html><head><meta http-equiv="Content-Security-Policy" content="default-src 'none'; script-src 'nonce-app'; style-src 'unsafe-inline'"><script nonce="app">window.app = 1</script></head><body><div id="root">hello</div></body></html>`
	resp := response(document, "text/html; charset=utf-8")
	defer func() { _ = resp.Body.Close() }()
	resp.Header.Add("Content-Security-Policy", "default-src 'none'; script-src 'nonce-app'; connect-src https://api.example")
	resp.Header.Add("Content-Security-Policy", "script-src 'unsafe-inline'")
	resp.Header.Set("ETag", `"original"`)
	resp.Header.Set("Content-Length", "200")
	if err := Inject(resp, Config{AppID: "7k3d", ManagementOrigin: "https://apps.shaulavo.dev"}); err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, want := range []string{`<script nonce="app">window.app = 1</script>`, `<div id="root">hello</div>`, `data-mesh-app="7k3d"`, `data-mesh-manager="https://apps.shaulavo.dev"`, `script-src-elem &#39;nonce-app&#39; https://7k3d.shaulavo.dev/.mesh-app/pill.js`} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in %s", want, text)
		}
	}
	if strings.Count(text, `data-mesh-app=`) != 1 {
		t.Fatal("injected more than once")
	}
	policies := resp.Header.Values("Content-Security-Policy")
	if len(policies) != 2 {
		t.Fatal("lost multiple policies")
	}
	if !strings.Contains(policies[0], "connect-src https://api.example") || !strings.Contains(policies[0], "frame-src https://apps.shaulavo.dev") {
		t.Fatal("CSP lost original policy or control permission")
	}
	if !strings.Contains(policies[1], "script-src-elem 'unsafe-inline'") {
		t.Fatal("broke application's unsafe-inline policy")
	}
	if resp.Header.Get("ETag") != "" || resp.Header.Get("Content-Length") != "" || resp.ContentLength != -1 || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("stale cache validator or length")
	}
}

func TestInjectCompressedDocuments(t *testing.T) {
	document := `<html><head><title>Compressed</title></head><body>unchanged</body></html>`
	for _, encoding := range []string{"gzip", "br"} {
		t.Run(encoding, func(t *testing.T) {
			var compressed bytes.Buffer
			var writer io.WriteCloser
			if encoding == "gzip" {
				writer = gzip.NewWriter(&compressed)
			} else {
				writer = brotli.NewWriter(&compressed)
			}
			if _, err := io.WriteString(writer, document); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			resp := response("", "text/html")
			defer func() { _ = resp.Body.Close() }()
			resp.Body = io.NopCloser(bytes.NewReader(compressed.Bytes()))
			resp.Header.Set("Content-Encoding", encoding)
			if err := Inject(resp, Config{AppID: "7k3d", ManagementOrigin: "https://apps.shaulavo.dev"}); err != nil {
				t.Fatal(err)
			}
			content, err := io.ReadAll(resp.Body)
			if err != nil || !strings.Contains(string(content), "<body>unchanged</body>") || !strings.Contains(string(content), "data-mesh-app") {
				t.Fatalf("transformed body %q, %v", content, err)
			}
			if resp.Header.Get("Content-Encoding") != "" {
				t.Fatal("kept compressed encoding")
			}
		})
	}
}

func TestInjectNonHTMLUntouched(t *testing.T) {
	resp := response(`{"ok":true}`, "application/json")
	defer func() { _ = resp.Body.Close() }()
	original := resp.Body
	resp.Header.Set("ETag", `"json"`)
	resp.Header.Set("Content-Encoding", "gzip")
	if err := Inject(resp, Config{AppID: "7k3d", ManagementOrigin: "https://apps.shaulavo.dev"}); err != nil {
		t.Fatal(err)
	}
	if resp.Body != original || resp.Header.Get("ETag") != `"json"` || resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatal("changed non-HTML response")
	}
}

func TestInjectStreamsBeforeUpstreamCompletes(t *testing.T) {
	input, output := io.Pipe()
	resp := response("", "text/html")
	defer func() { _ = resp.Body.Close() }()
	resp.Body = input
	if err := Inject(resp, Config{AppID: "7k3d", ManagementOrigin: "https://apps.shaulavo.dev"}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = output.Close() }()
	go func() { _, _ = io.WriteString(output, "<!doctype html><html><head>") }()
	done := make(chan string, 1)
	go func() {
		var content strings.Builder
		buffer := make([]byte, 512)
		for !strings.Contains(content.String(), "data-mesh-app") {
			count, err := resp.Body.Read(buffer)
			content.Write(buffer[:count])
			if err != nil {
				break
			}
		}
		done <- content.String()
	}()
	select {
	case content := <-done:
		if !strings.Contains(content, "data-mesh-app") {
			t.Fatal("missing streamed bootstrap")
		}
	case <-time.After(time.Second):
		t.Fatal("waited for entire upstream document")
	}
}

func TestInjectPassesUnsupportedEncodingThrough(t *testing.T) {
	for _, kind := range []string{"text/html; charset=utf-16", "text/html"} {
		resp := response("original", kind)
		defer func() { _ = resp.Body.Close() }()
		if kind == "text/html" {
			resp.Header.Set("Content-Encoding", "zstd")
		}
		original := resp.Body
		if err := Inject(resp, Config{AppID: "7k3d", ManagementOrigin: "https://apps.shaulavo.dev"}); err != nil {
			t.Fatalf("unsupported encoding blocked the response: %v", err)
		}
		if resp.Body != original {
			t.Fatal("mutated body on rejection")
		}
	}
}

func TestStripMeshCookiesPreservesApplicationCookies(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "https://7k3d.shaulavo.dev/", nil)
	request.Header.Set("Cookie", "__Host-mesh-view=secret; app=session; __Host-mesh-session=owner")
	StripRequestCookies(request)
	if request.Header.Get("Cookie") != "app=session" {
		t.Fatalf("request cookies %q", request.Header.Get("Cookie"))
	}
	header := http.Header{}
	header.Add("Set-Cookie", "__Host-mesh-view=forged; Secure; Path=/")
	header.Add("Set-Cookie", "app=session; Secure")
	StripCookies(header)
	if got := header.Values("Set-Cookie"); len(got) != 1 || got[0] != "app=session; Secure" {
		t.Fatalf("upstream cookies %#v", got)
	}
}

func TestAssetServesEmbeddedControlAndReservesPrefix(t *testing.T) {
	for _, path := range []string{AssetPath, "/.mesh-app/pill.css"} {
		result := httptest.NewRecorder()
		if !Asset(result, httptest.NewRequest(http.MethodGet, path, nil)) || result.Code != http.StatusOK || result.Body.Len() == 0 {
			t.Fatalf("failed asset %s", path)
		}
		head := httptest.NewRecorder()
		Asset(head, httptest.NewRequest(http.MethodHead, path, nil))
		if head.Body.Len() != 0 {
			t.Fatal("HEAD wrote body")
		}
	}
	result := httptest.NewRecorder()
	if Asset(result, httptest.NewRequest(http.MethodGet, "/api", nil)) {
		t.Fatal("intercepted API")
	}
	if !Asset(result, httptest.NewRequest(http.MethodGet, "/.mesh-app/missing", nil)) || result.Code != 404 {
		t.Fatal("reserved prefix leaked to app")
	}
}

func TestStrictDynamicAllowsInjectedNonceWithoutRemovingPolicy(t *testing.T) {
	policy := permitControl("default-src 'none'; script-src 'nonce-app' 'strict-dynamic'; style-src 'unsafe-inline'", "7k3d.shaulavo.dev", "https://apps.shaulavo.dev", "mesh")
	if !strings.Contains(policy, "script-src-elem 'nonce-app' 'strict-dynamic' https://7k3d.shaulavo.dev/.mesh-app/pill.js 'nonce-mesh'") {
		t.Fatal(policy)
	}
	if !strings.Contains(policy, "style-src-elem 'unsafe-inline' https://7k3d.shaulavo.dev/.mesh-app/pill.css") {
		t.Fatal(policy)
	}
}

func TestCommaSeparatedPoliciesEachPermitControl(t *testing.T) {
	policy := permitControl("default-src 'none', default-src 'self'; script-src 'nonce-app'", "7k3d.shaulavo.dev", "https://apps.shaulavo.dev", "mesh")
	if strings.Count(policy, "https://7k3d.shaulavo.dev/.mesh-app/pill.js") != 2 || strings.Count(policy, "frame-src") != 2 {
		t.Fatal(policy)
	}
}
