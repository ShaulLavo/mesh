package apppill

import (
	"io"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func assertPillPlacement(t *testing.T, document string) {
	t.Helper()
	root, err := html.Parse(strings.NewReader(document))
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for node := range root.Descendants() {
		if node.Type != html.ElementNode || node.Data != "script" {
			continue
		}
		for _, attribute := range node.Attr {
			if attribute.Key == "src" && attribute.Val == AssetPath {
				count++
				if node.Parent.Data != "head" && node.Parent.Data != "body" {
					t.Fatalf("pill inserted under %s", node.Parent.Data)
				}
			}
		}
	}
	if count != 1 {
		t.Fatalf("parsed pill script count=%d, want 1 outside raw text", count)
	}
}

func TestInjectOverflowBeforeHeadSkipsPillInsideRawText(t *testing.T) {
	large := strings.Repeat("a", 300<<10)
	for _, element := range []string{"script", "style", "textarea", "title"} {
		t.Run(element, func(t *testing.T) {
			document := "<html><" + element + ">" + large + "</" + element + "><body>after</body></html>"
			resp := response(document, eligibilityHTML)
			defer func() { _ = resp.Body.Close() }()
			if err := Inject(resp, Config{AppID: eligibilityAppID, ManagementOrigin: eligibilityManager}); err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil || string(body) != document {
				t.Fatalf("overflow inside %s must pass through without a pill: bytes=%d, want=%d, error=%v", element, len(body), len(document), err)
			}
		})
	}
}

func TestInjectOverflowBeforeHeadCommentSkipsPill(t *testing.T) {
	document := "<!--" + strings.Repeat("a", 300<<10) + "--><html><head></head><body>after</body></html>"
	resp := response(document, eligibilityHTML)
	defer func() { _ = resp.Body.Close() }()
	if err := Inject(resp, Config{AppID: eligibilityAppID, ManagementOrigin: eligibilityManager}); err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != document {
		t.Fatalf("overflow inside comment must pass through without a pill: bytes=%d, want=%d, error=%v", len(body), len(document), err)
	}
}

func TestInjectRewritesCSPMetaAfterHead(t *testing.T) {
	document := `<html><head><title>app</title></head><meta http-equiv="Content-Security-Policy" content="default-src 'self'"><body>app</body></html>`
	resp := response(document, eligibilityHTML)
	defer func() { _ = resp.Body.Close() }()
	if err := Inject(resp, Config{AppID: eligibilityAppID, ManagementOrigin: eligibilityManager}); err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "frame-src &#39;self&#39; "+eligibilityManager) {
		t.Fatalf("hoisted CSP meta does not allow management frame: %s", body)
	}
	assertPillPlacement(t, string(body))
}
