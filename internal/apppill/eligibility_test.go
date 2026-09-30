package apppill

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

const (
	eligibilityAppID   = "zzzz"
	eligibilityManager = "https://control.example"
	eligibilityHTML    = "text/html; charset=UTF-8"
	eligibilityUTF16   = "text/html; charset=UTF-16"
	zstdEncoding       = "zstd"
	pillAttribute      = "data-mesh-app="
	browserEncodings   = "gzip, deflate, br, zstd"
)

type ineligibleCase struct {
	name, contentType, encoding, disposition, method string
	status                                           int
}

func TestInjectIneligibleResponsesPreserveRepresentation(t *testing.T) {
	for _, tc := range []ineligibleCase{
		{name: "attachment", disposition: `attachment; filename="app.html"`},
		{name: "compressed attachment", disposition: `Attachment; filename="app.html"`, encoding: "GZIP"},
		{name: "unknown disposition", disposition: "download"},
		{name: "malformed disposition", disposition: `inline; filename="unterminated`},
		{name: "partial", status: http.StatusPartialContent},
		{name: "compressed partial", status: http.StatusPartialContent, encoding: "br"},
		{name: "not modified", status: http.StatusNotModified},
		{name: "no content", status: http.StatusNoContent},
		{name: "HEAD", method: http.MethodHead},
		{name: "error page", status: http.StatusInternalServerError},
		{name: "zstd", encoding: zstdEncoding},
		{name: "deflate", encoding: "deflate"},
		{name: "stacked encodings", encoding: "gzip, br"},
		{name: "utf16", contentType: eligibilityUTF16},
		{name: "shift jis", contentType: "text/html; charset=shift_jis"},
	} {
		for _, private := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/private=%t", tc.name, private), func(t *testing.T) {
				document := "<html><head></head><body>original</body></html>"
				resp := ineligibleResponse(document, tc)
				defer func() { _ = resp.Body.Close() }()
				assertPassThrough(t, resp, document, private)
			})
		}
	}
}

func ineligibleResponse(document string, tc ineligibleCase) *http.Response {
	kind := tc.contentType
	if kind == "" {
		kind = eligibilityHTML
	}
	resp := response(document, kind)
	if tc.status != 0 {
		resp.StatusCode = tc.status
	}
	resp.Request = &http.Request{Method: tc.method}
	resp.Header.Set("Content-Length", fmt.Sprint(len(document)))
	resp.Header.Set("Content-Encoding", tc.encoding)
	resp.Header.Set("Content-Disposition", tc.disposition)
	resp.Header.Set("Content-Range", "bytes 0-49/100")
	resp.Header.Set("ETag", `"original"`)
	resp.Header.Set("Last-Modified", "Wed, 01 Oct 2025 12:00:00 GMT")
	resp.Header.Set("Content-MD5", "original-md5")
	resp.Header.Set("Digest", "sha-256=original")
	resp.Header.Set("Content-Digest", "sha-256=:original:")
	resp.Header.Set("Accept-Ranges", "bytes")
	resp.Header.Set("Cache-Control", "public, max-age=60")
	resp.Header.Set("Content-Security-Policy", "default-src 'self'")
	resp.Header.Set("Set-Cookie", "app=session")
	return resp
}

func assertPassThrough(t *testing.T, resp *http.Response, document string, private bool) {
	t.Helper()
	originalBody, originalLength := resp.Body, resp.ContentLength
	wantHeaders := resp.Header.Clone()
	if private {
		wantHeaders.Add("Content-Security-Policy", "frame-ancestors 'self'")
	}
	if err := Inject(resp, Config{AppID: eligibilityAppID, ManagementOrigin: eligibilityManager, Private: private}); err != nil {
		t.Fatalf("pass-through returned error: %v", err)
	}
	if resp.Body != originalBody || resp.ContentLength != originalLength || !reflect.DeepEqual(resp.Header, wantHeaders) {
		t.Fatal("mutated ineligible response representation or headers")
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != document {
		t.Fatalf("pass-through body changed: length=%d, error=%v", len(body), err)
	}
}

func TestInjectOversizedTokensPreserveEveryAppByte(t *testing.T) {
	large := strings.Repeat("a", 300<<10)
	for _, tc := range []struct{ name, document string }{
		{"head script", "<html><head><script>var data='" + large + "';</script></head><body>after</body></html>"},
		{"body script", "<html><head></head><body><script>var data='" + large + "';</script><p>after</p></body></html>"},
		{"head style", "<html><head><style>/*" + large + "*/</style></head><body>after</body></html>"},
		{"body style", "<html><head></head><body><style>/*" + large + "*/</style><p>after</p></body></html>"},
		{"before injection", "<html data-large='" + large + "'><head></head><body>after</body></html>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := response(tc.document, eligibilityHTML)
			defer func() { _ = resp.Body.Close() }()
			resp.Header.Set("Content-Length", fmt.Sprint(len(tc.document)))
			if err := Inject(resp, Config{AppID: eligibilityAppID, ManagementOrigin: eligibilityManager}); err != nil {
				t.Fatal(err)
			}
			assertOnePillPreservesDocument(t, resp, tc.document)
		})
	}
}

func assertOnePillPreservesDocument(t *testing.T, resp *http.Response, document string) {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("HTML truncated: got %d of %d app bytes, error=%v", len(body), len(document), err)
	}
	text := string(body)
	if strings.Count(text, pillAttribute) != 1 {
		t.Fatal("expected exactly one pill")
	}
	assertPillPlacement(t, text)
	start := strings.Index(text, `<script defer charset="utf-8" src="`+AssetPath+`"`)
	if start < 0 {
		t.Fatal("missing pill script")
	}
	end := start + strings.Index(text[start:], "</script>") + len("</script>")
	if text[:start]+text[end:] != document {
		t.Fatal("app bytes changed, disappeared or moved")
	}
	if resp.Header.Get("Content-Length") != "" || resp.ContentLength != -1 {
		t.Fatal("streamed HTML retained original content length")
	}
}

func TestInjectProxyUnsupportedDocumentsRemainAvailable(t *testing.T) {
	for _, tc := range []struct{ name, contentType, encoding string }{
		{"zstd", eligibilityHTML, zstdEncoding},
		{"unsupported charset", eligibilityUTF16, ""},
		{"normal HTML", eligibilityHTML, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			document := []byte("<html><head></head><body>app</body></html>")
			if tc.encoding == zstdEncoding {
				// One single-segment zstd frame with a final raw block of 42 bytes.
				document = append([]byte{0x28, 0xb5, 0x2f, 0xfd, 0x20, 0x2a, 0x51, 0x01, 0x00}, document...)
			}
			proxy := pillProxy(t, document, tc.contentType, tc.encoding)
			request := httptest.NewRequest(http.MethodGet, "https://zzzz.shaulavo.dev/", nil)
			request.Header.Set("Accept-Encoding", browserEncodings)
			result := httptest.NewRecorder()
			proxy.ServeHTTP(result, request)
			assertProxyDocument(t, result, document, tc.contentType, tc.encoding)
		})
	}
}

func assertProxyDocument(t *testing.T, result *httptest.ResponseRecorder, document []byte, contentType, encoding string) {
	t.Helper()
	if result.Code != http.StatusOK {
		t.Fatalf("proxy status=%d, want 200", result.Code)
	}
	if contentType == eligibilityHTML && encoding == "" {
		if bytes.Count(result.Body.Bytes(), []byte(pillAttribute)) != 1 {
			t.Fatal("normal HTML lost its pill")
		}
		return
	}
	if !bytes.Equal(result.Body.Bytes(), document) || result.Header().Get("ETag") != `"origin"` || result.Header().Get("Content-Encoding") != encoding || result.Header().Get("Content-Type") != contentType {
		t.Fatal("proxy changed unsupported representation")
	}
}

func pillProxy(t *testing.T, document []byte, contentType, encoding string) *httputil.ReverseProxy {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept-Encoding") != browserEncodings {
			t.Error("changed the client's accepted encodings")
		}
		w.Header().Set("Content-Type", contentType)
		if encoding != "" {
			w.Header().Set("Content-Encoding", encoding)
		}
		w.Header().Set("ETag", `"origin"`)
		_, _ = w.Write(document)
	}))
	t.Cleanup(upstream.Close)
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{DisableCompression: true}
	t.Cleanup(transport.CloseIdleConnections)
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = transport
	proxy.ModifyResponse = func(resp *http.Response) error {
		return Inject(resp, Config{AppID: eligibilityAppID, ManagementOrigin: eligibilityManager})
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "App origin is unavailable.", http.StatusServiceUnavailable)
	}
	return proxy
}
