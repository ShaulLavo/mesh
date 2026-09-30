// Package apppill serves the embedded Solid control and transforms app HTML.
package apppill

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/andybalholm/brotli"
	"golang.org/x/net/html"
)

//go:embed assets/pill.js assets/pill.css
var assets embed.FS

const AssetPath = "/.mesh-app/pill.js"
const maximumHTMLToken = 256 << 10

// Asset handles the app's reserved asset namespace without touching its runtime.
func Asset(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/.mesh-app/") {
		return false
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if (r.URL.Path != AssetPath && r.URL.Path != "/.mesh-app/pill.css") || r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.NotFound(w, r)
		return true
	}
	body, err := assets.ReadFile("assets/" + strings.TrimPrefix(r.URL.Path, "/.mesh-app/"))
	if err != nil {
		http.Error(w, "control unavailable", http.StatusServiceUnavailable)
		return true
	}
	if r.URL.Path == AssetPath {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	}
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	if r.Method != http.MethodHead {
		_, _ = w.Write(body) //nolint:gosec // Only the two fixed embedded asset paths can reach this write.
	}
	return true
}

// StripCookies removes Mesh credentials from app requests and prevents the app
// from replacing the edge's host-only authorization cookies.
func StripCookies(header http.Header) {
	values := header.Values("Set-Cookie")
	header.Del("Set-Cookie")
	for _, value := range values {
		name, _, _ := strings.Cut(value, "=")
		if !reservedCookie(strings.TrimSpace(name)) {
			header.Add("Set-Cookie", value)
		}
	}
}

func reservedCookie(name string) bool {
	return strings.HasPrefix(strings.ToLower(name), "__host-mesh") || strings.HasPrefix(strings.ToLower(name), "__secure-mesh")
}

// StripRequestCookies consumes reserved Mesh cookies before forwarding app traffic.
func StripRequestCookies(request *http.Request) {
	cookies := request.Cookies()
	request.Header.Del("Cookie")
	for _, cookie := range cookies {
		if !reservedCookie(cookie.Name) {
			request.AddCookie(cookie)
		}
	}
}

// Config contains the app state verified by the edge before serving its HTML.
type Config struct {
	AppID            string
	ManagementOrigin string
	Private          bool
	Owns             bool
}

// Inject installs a streaming HTML transformer with a bounded token buffer.
func Inject(resp *http.Response, config Config) error {
	appID, managementOrigin := config.AppID, config.ManagementOrigin
	if resp == nil || resp.Body == nil {
		return errors.New("apppill: missing response body")
	}
	if config.Private {
		// Keep the edge framing policy outside the pill's script and frame adaptation.
		defer resp.Header.Add("Content-Security-Policy", "frame-ancestors 'self'")
	}
	encoding, eligible := eligibleHTML(resp)
	if !eligible {
		return nil
	}
	manager, err := url.Parse(managementOrigin)
	if err != nil || manager.Host == "" || manager.User != nil || manager.RawQuery != "" || manager.Fragment != "" || manager.Path != "" && manager.Path != "/" || manager.Scheme != "https" && !(manager.Scheme == "http" && manager.Hostname() == "localhost") {
		return errors.New("apppill: invalid management origin")
	}
	if !validAppID(appID) {
		return errors.New("apppill: invalid app ID")
	}
	nonceBytes := make([]byte, 18)
	if _, err := rand.Read(nonceBytes); err != nil {
		return fmt.Errorf("apppill: nonce: %w", err)
	}
	nonce := base64.RawStdEncoding.EncodeToString(nonceBytes)
	managerOrigin := manager.Scheme + "://" + manager.Host
	source := resp.Body
	var reader io.Reader = source
	var decoder io.Closer
	switch encoding {
	case "", "identity":
	case "gzip":
		zipped, err := gzip.NewReader(source)
		if err != nil {
			return fmt.Errorf("apppill: decode gzip: %w", err)
		}
		reader, decoder = zipped, zipped
	case "br":
		reader = brotli.NewReader(source)
	}
	script := "<script defer charset=\"utf-8\" src=\"" + AssetPath + "\" data-mesh-app=\"" + html.EscapeString(appID) + "\" data-mesh-manager=\"" + html.EscapeString(managerOrigin) + "\" data-mesh-private=\"" + fmt.Sprint(config.Private) + "\" data-mesh-owns=\"" + fmt.Sprint(config.Owns) + "\" nonce=\"" + nonce + "\"></script>"
	for _, key := range []string{"Content-Security-Policy", "Content-Security-Policy-Report-Only"} {
		policies := resp.Header.Values(key)
		resp.Header.Del(key)
		for _, policy := range policies {
			resp.Header.Add(key, permitControl(policy, appID+".shaulavo.dev", managerOrigin, nonce))
		}
	}
	for _, key := range []string{"Content-Length", "Content-Encoding", "ETag", "Last-Modified", "Content-MD5", "Digest", "Content-Digest", "Accept-Ranges"} {
		resp.Header.Del(key)
	}
	resp.Header.Set("Cache-Control", "no-store")
	resp.Header.Set("Pragma", "no-cache")
	StripCookies(resp.Header)
	resp.ContentLength = -1
	pipe, writer := io.Pipe()
	resp.Body = &transformedBody{PipeReader: pipe, source: source, decoder: decoder}
	go func() {
		err := transform(reader, writer, script, appID+".shaulavo.dev", managerOrigin, nonce)
		if decoder != nil {
			_ = decoder.Close()
		}
		_ = source.Close()
		_ = writer.CloseWithError(err)
	}()
	return nil
}

func eligibleHTML(resp *http.Response) (string, bool) {
	if resp.StatusCode != http.StatusOK || resp.Body == http.NoBody || resp.Request != nil && resp.Request.Method == http.MethodHead {
		return "", false
	}
	contentType, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || contentType != "text/html" {
		return "", false
	}
	switch strings.ToLower(params["charset"]) {
	case "", "utf-8", "utf8", "us-ascii", "iso-8859-1", "windows-1252":
	default:
		return "", false
	}
	if disposition := resp.Header.Get("Content-Disposition"); disposition != "" {
		kind, _, err := mime.ParseMediaType(disposition)
		if err != nil || kind != "inline" {
			return "", false
		}
	}
	encoding := strings.ToLower(strings.TrimSpace(strings.Join(resp.Header.Values("Content-Encoding"), ",")))
	switch encoding {
	case "", "identity", "gzip", "br":
		return encoding, true
	default:
		return "", false
	}
}

type transformedBody struct {
	*io.PipeReader
	source  io.Closer
	decoder io.Closer
}

func (body *transformedBody) Close() error {
	// Closing the pipe first unblocks a writer waiting for a disconnected reader.
	err := body.PipeReader.Close()
	return errors.Join(err, body.source.Close())
}

func transform(reader io.Reader, writer io.Writer, script, appHost, manager, nonce string) error {
	tokenizer := html.NewTokenizer(reader)
	tokenizer.SetMaxBuf(maximumHTMLToken)
	injected := false
	safeInsertion := true
	for {
		kind := tokenizer.Next()
		if kind == html.ErrorToken {
			if injected {
				script = ""
			}
			return finishHTML(reader, writer, tokenizer, script, safeInsertion)
		}
		raw := append([]byte(nil), tokenizer.Raw()...)
		var name string
		switch kind {
		case html.StartTagToken, html.SelfClosingTagToken, html.EndTagToken:
			token := tokenizer.Token()
			name = token.Data
			if name == "meta" {
				raw = rewriteMeta(token, raw, appHost, manager, nonce)
			}
		case html.ErrorToken, html.TextToken, html.CommentToken, html.DoctypeToken:
		}
		safeInsertion = safeHTMLInsertion(kind, name)
		if !injected && pastHTMLHead(kind, name) {
			if _, err := io.WriteString(writer, script); err != nil {
				return err
			}
			injected = true
		}
		if _, err := writer.Write(raw); err != nil {
			return err
		}
		if injected && pastHTMLHead(kind, name) {
			return copyHTML(reader, writer, tokenizer.Buffered())
		}
		if !injected && name == "head" {
			if _, err := io.WriteString(writer, script); err != nil {
				return err
			}
			injected = true
		}
	}
}

func pastHTMLHead(kind html.TokenType, name string) bool {
	return kind == html.StartTagToken && name == "body"
}

func safeHTMLInsertion(kind html.TokenType, name string) bool {
	return kind == html.DoctypeToken || kind == html.CommentToken || kind == html.StartTagToken && name == "html"
}

func finishHTML(reader io.Reader, writer io.Writer, tokenizer *html.Tokenizer, script string, safeInsertion bool) error {
	if errors.Is(tokenizer.Err(), html.ErrBufferExceeded) {
		// A partial text token may already be inside a script, style, or comment.
		if !safeInsertion || len(tokenizer.Raw()) == 0 {
			script = ""
		}
		return copyHTML(reader, writer, []byte(script), tokenizer.Raw(), tokenizer.Buffered())
	}
	if err := tokenizer.Err(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("apppill: HTML token: %w", err)
	}
	if script == "" {
		return nil
	}
	if _, err := io.WriteString(writer, script); err != nil {
		return fmt.Errorf("apppill: write bootstrap: %w", err)
	}
	return nil
}

func copyHTML(reader io.Reader, writer io.Writer, pending ...[]byte) error {
	readers := make([]io.Reader, 0, len(pending)+1)
	for _, prefix := range pending {
		readers = append(readers, bytes.NewReader(prefix))
	}
	readers = append(readers, reader)
	if _, err := io.Copy(writer, io.MultiReader(readers...)); err != nil {
		return fmt.Errorf("apppill: copy HTML remainder: %w", err)
	}
	return nil
}

func rewriteMeta(token html.Token, raw []byte, appHost, manager, nonce string) []byte {
	policy := false
	for _, attribute := range token.Attr {
		if attribute.Key == "http-equiv" && strings.EqualFold(attribute.Val, "content-security-policy") {
			policy = true
		}
	}
	if !policy {
		return raw
	}
	for index := range token.Attr {
		if token.Attr[index].Key == "content" {
			token.Attr[index].Val = permitControl(token.Attr[index].Val, appHost, manager, nonce)
		}
	}
	return []byte(token.String())
}

func permitControl(policy, appHost, manager, nonce string) string {
	policies := strings.Split(policy, ",")
	for index, value := range policies {
		policies[index] = permitPolicy(value, appHost, manager, nonce)
	}
	return strings.Join(policies, ", ")
}

func permitPolicy(policy, appHost, manager, nonce string) string {
	directives := make(map[string][]string)
	order := []string{}
	for _, part := range strings.Split(policy, ";") {
		fields := strings.Fields(part)
		if len(fields) == 0 {
			continue
		}
		name := strings.ToLower(fields[0])
		if _, exists := directives[name]; exists {
			continue
		}
		directives[name] = fields[1:]
		order = append(order, name)
	}
	fallback := func(name string) []string {
		if values, exists := directives[name]; exists {
			return append([]string(nil), values...)
		}
		if name == "script-src-elem" {
			if values, exists := directives["script-src"]; exists {
				return append([]string(nil), values...)
			}
		}
		if name == "style-src-elem" {
			if values, exists := directives["style-src"]; exists {
				return append([]string(nil), values...)
			}
		}
		if name == "frame-src" {
			if values, exists := directives["child-src"]; exists {
				return append([]string(nil), values...)
			}
		}
		if values, exists := directives["default-src"]; exists {
			return append([]string(nil), values...)
		}
		return []string{"*", "data:", "blob:", "'unsafe-inline'", "'unsafe-eval'"}
	}
	for _, permit := range []struct{ name, value string }{{"script-src-elem", "https://" + appHost + AssetPath}, {"style-src-elem", "https://" + appHost + "/.mesh-app/pill.css"}, {"frame-src", manager}} {
		values := fallback(permit.name)
		next := []string{}
		for _, value := range values {
			if value != "'none'" {
				next = append(next, value)
			}
		}
		next = append(next, permit.value)
		if permit.name == "script-src-elem" && containsSource(values, "'strict-dynamic'") {
			next = append(next, "'nonce-"+nonce+"'")
		}
		if _, exists := directives[permit.name]; !exists {
			order = append(order, permit.name)
		}
		directives[permit.name] = next
	}
	parts := make([]string, 0, len(order))
	for _, name := range order {
		parts = append(parts, strings.Join(append([]string{name}, directives[name]...), " "))
	}
	return strings.Join(parts, "; ")
}

func validAppID(id string) bool {
	if len(id) != 4 {
		return false
	}
	for _, value := range id {
		if !strings.ContainsRune("0123456789abcdefghjkmnpqrstvwxyz", value) {
			return false
		}
	}
	return true
}

func containsSource(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
