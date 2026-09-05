package serve

import (
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
)

var directoryTemplate = template.Must(template.New("directory").Parse(`<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><title>{{.Title}}</title></head>
<body><h1>{{.Title}}</h1><ul>{{range .Entries}}
<li><a href="{{.Href}}">{{.Name}}</a></li>{{end}}
</ul></body>
</html>
`))

type directoryPage struct {
	Title   string
	Entries []directoryEntry
}

type directoryEntry struct {
	Name string
	Href string
}

// Handler returns the HTTP handler for service mounted at prefix.
func Handler(service Service, prefix string) (http.Handler, error) {
	normalized, err := normalizeService(service)
	if err != nil {
		return nil, err
	}
	if err := validatePrefix(prefix); err != nil {
		return nil, err
	}
	return handlerForNormalizedService(normalized, prefix, nil)
}

func handlerForNormalizedService(service Service, prefix string, trustForwardedHeaders func(netip.Addr) bool) (http.Handler, error) {
	var handler http.Handler
	switch service.Kind {
	case Static:
		handler = mountedHandler(prefix, func(w http.ResponseWriter, request *http.Request, relative string) {
			serveStatic(w, request, service.Target, relative)
		})
	case Files:
		handler = mountedHandler(prefix, func(w http.ResponseWriter, request *http.Request, relative string) {
			serveFiles(w, request, service.Target, prefix, relative)
		})
	case Proxy:
		handler = proxyHandler(service.Target, prefix, trustForwardedHeaders, service.Isolate)
	default:
		return nil, fmt.Errorf("serve: service %q has unsupported kind %q", service.Name, service.Kind)
	}
	if service.Isolate && service.Kind != Proxy {
		// The proxy sets these on the way back instead, so a backend cannot
		// weaken them by sending its own.
		inner := handler
		handler = http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			setIsolationHeaders(w.Header())
			inner.ServeHTTP(w, request)
		})
	}
	return handler, nil
}

// setIsolationHeaders makes the response a cross-origin isolated context.
// Both values are the strict ones every browser accepts; the gentler
// "credentialless" embedder policy is not implemented by Safari.
func setIsolationHeaders(header http.Header) {
	header.Set("Cross-Origin-Opener-Policy", "same-origin")
	header.Set("Cross-Origin-Embedder-Policy", "require-corp")
}

func validatePrefix(prefix string) error {
	if prefix == "" || !strings.HasPrefix(prefix, "/") || path.Clean(prefix) != prefix || prefix != "/" && strings.HasSuffix(prefix, "/") {
		return fmt.Errorf("serve: mount prefix %q must be a clean absolute path", prefix)
	}
	if strings.ContainsAny(prefix, "%?#\\") || strings.IndexByte(prefix, 0) >= 0 {
		return fmt.Errorf("serve: mount prefix %q contains unsupported characters", prefix)
	}
	return nil
}

func mountedHandler(prefix string, serve func(http.ResponseWriter, *http.Request, string)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		relative, ok := relativeRequestPath(request.URL.EscapedPath(), prefix)
		if !ok {
			http.NotFound(w, request)
			return
		}
		if request.Method != http.MethodGet && request.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		serve(w, request, relative)
	})
}

func relativeRequestPath(escapedPath, prefix string) (string, bool) {
	if prefix == "/" {
		return escapedPath, true
	}
	if escapedPath == prefix {
		return "/", true
	}
	if !strings.HasPrefix(escapedPath, prefix+"/") {
		return "", false
	}
	return strings.TrimPrefix(escapedPath, prefix), true
}

func serveStatic(w http.ResponseWriter, request *http.Request, root, relative string) {
	file, info, ok := openForHTTP(w, request, root, relative)
	if !ok {
		return
	}
	if info.IsDir() {
		if !strings.HasSuffix(request.URL.Path, "/") {
			_ = file.Close()
			redirectDirectory(w, request)
			return
		}
		_ = file.Close()
		indexRelative := strings.TrimSuffix(relative, "/") + "/index.html"
		file, info, ok = openForHTTP(w, request, root, indexRelative)
		if !ok || info.IsDir() {
			if ok {
				_ = file.Close()
				http.NotFound(w, request)
			}
			return
		}
	}
	defer file.Close() //nolint:errcheck // the response owns any read failure
	// A static service is a real website, so it gets no sandbox and no
	// connect-src restriction, either of which would break ordinary pages. What
	// stops a page here from reaching the control socket is the Origin refusal
	// in internal/transport, not a header on this response.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	serveOpenedFile(w, request, file, info)
}

func serveFiles(w http.ResponseWriter, request *http.Request, root, prefix, relative string) {
	file, info, ok := openForHTTP(w, request, root, relative)
	if !ok {
		return
	}
	defer file.Close() //nolint:errcheck // the response owns any read failure
	if !info.IsDir() {
		// A files root is a file server, not a site. Anything in it is handed
		// over as a download rather than rendered in this origin, because a
		// served .html would otherwise run with the origin of the host itself.
		w.Header().Set("Content-Disposition", "attachment")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "sandbox")
		serveOpenedFile(w, request, file, info)
		return
	}
	if !strings.HasSuffix(request.URL.Path, "/") {
		redirectDirectory(w, request)
		return
	}
	entries, err := file.ReadDir(-1)
	if err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	decoded, err := decodeRequestPath(relative)
	if err != nil {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	page := directoryPage{Title: request.URL.Path, Entries: make([]directoryEntry, 0, len(entries)+1)}
	logicalDirectory := strings.Trim(decoded, "/")
	if logicalDirectory != "" {
		parent := path.Dir(logicalDirectory)
		if parent == "." {
			parent = ""
		}
		page.Entries = append(page.Entries, directoryEntry{Name: "../", Href: mountedURL(prefix, parent, true)})
	}
	for _, entry := range entries {
		isDirectory := entry.IsDir()
		name := entry.Name()
		if isDirectory {
			name += "/"
		}
		page.Entries = append(page.Entries, directoryEntry{
			Name: name,
			Href: mountedURL(prefix, path.Join(logicalDirectory, entry.Name()), isDirectory),
		})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if request.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	if err := directoryTemplate.Execute(w, page); err != nil {
		return
	}
}

func openForHTTP(w http.ResponseWriter, request *http.Request, root, relative string) (*os.File, fs.FileInfo, bool) {
	file, info, err := OpenRootEntry(root, relative)
	if err != nil {
		switch {
		case errors.Is(err, ErrRootUnavailable):
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		case errors.Is(err, ErrInvalidPath):
			http.Error(w, "bad path", http.StatusBadRequest)
		default:
			http.NotFound(w, request)
		}
		return nil, nil, false
	}
	return file, info, true
}

func serveOpenedFile(w http.ResponseWriter, request *http.Request, file *os.File, info fs.FileInfo) {
	http.ServeContent(w, request, info.Name(), info.ModTime(), file)
}

func redirectDirectory(w http.ResponseWriter, request *http.Request) {
	target := "/" + strings.TrimLeft(request.URL.EscapedPath(), "/")
	if !strings.HasSuffix(target, "/") {
		target += "/"
	}
	if request.URL.RawQuery != "" {
		target += "?" + request.URL.RawQuery
	}
	http.Redirect(w, request, target, http.StatusPermanentRedirect) //nolint:gosec // target is forced to one leading slash; the scheme-relative regression is tested
}

func mountedURL(prefix, logicalPath string, directory bool) string {
	parts := strings.Split(strings.Trim(logicalPath, "/"), "/")
	escaped := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			escaped = append(escaped, url.PathEscape(part))
		}
	}
	result := prefix
	if result != "/" {
		result += "/"
	}
	result += strings.Join(escaped, "/")
	if directory && !strings.HasSuffix(result, "/") {
		result += "/"
	}
	if result == "" {
		return "/"
	}
	return result
}

func proxyHandler(port, prefix string, trustForwardedHeaders func(netip.Addr) bool, isolate bool) http.Handler {
	target := &url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", port)}
	var modifyResponse func(*http.Response) error
	if isolate {
		modifyResponse = func(response *http.Response) error {
			setIsolationHeaders(response.Header)
			return nil
		}
	}
	proxy := &httputil.ReverseProxy{
		ModifyResponse: modifyResponse,
		Rewrite: func(request *httputil.ProxyRequest) {
			forwardedFor, forwardedProto, trusted := trustedForwardingMetadata(request.In, trustForwardedHeaders)
			request.SetURL(target)
			request.Out.Host = request.In.Host
			removeForwardedHeaders(request.Out.Header)
			if trusted {
				request.Out.Header.Set("X-Forwarded-For", forwardedFor)
				request.Out.Header.Set("X-Forwarded-Host", request.In.Host)
				request.Out.Header.Set("X-Forwarded-Proto", forwardedProto)
			} else {
				request.SetXForwarded()
			}
			request.Out.Header.Set("X-Forwarded-Prefix", prefix)
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "bad gateway", http.StatusBadGateway)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		// A request for the bare mount point gets the trailing slash first,
		// exactly as a static directory does. Otherwise the backend answers a
		// page whose relative URLs resolve one level above the mount, and
		// every app would need to learn the prefix to survive that.
		if prefix != "/" && request.URL.EscapedPath() == prefix {
			redirectDirectory(w, request)
			return
		}
		relative, ok := relativeRequestPath(request.URL.EscapedPath(), prefix)
		if !ok {
			http.NotFound(w, request)
			return
		}
		decoded, err := url.PathUnescape(relative)
		if err != nil || strings.IndexByte(decoded, 0) >= 0 {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		proxied := request.Clone(request.Context())
		proxied.URL.Path = decoded
		proxied.URL.RawPath = relative
		proxy.ServeHTTP(w, proxied)
	})
}

func trustedForwardingMetadata(request *http.Request, trust func(netip.Addr) bool) (string, string, bool) {
	if trust == nil {
		return "", "", false
	}
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err != nil {
		return "", "", false
	}
	immediate, err := netip.ParseAddr(host)
	if err != nil || !trust(immediate.Unmap()) {
		return "", "", false
	}
	forwardedForValues := request.Header.Values("X-Forwarded-For")
	forwardedProtoValues := request.Header.Values("X-Forwarded-Proto")
	if len(forwardedForValues) != 1 || len(forwardedProtoValues) != 1 || strings.Contains(forwardedForValues[0], ",") {
		return "", "", false
	}
	forwardedFor, err := netip.ParseAddr(forwardedForValues[0])
	if err != nil || forwardedFor.Zone() != "" || forwardedFor.Unmap().String() != forwardedForValues[0] {
		return "", "", false
	}
	forwardedProto := forwardedProtoValues[0]
	if forwardedProto != "http" && forwardedProto != "https" {
		return "", "", false
	}
	return forwardedForValues[0], forwardedProto, true
}

func removeForwardedHeaders(header http.Header) {
	header.Del("Forwarded")
	header.Del("X-Forwarded-For")
	header.Del("X-Forwarded-Host")
	header.Del("X-Forwarded-Proto")
	header.Del("X-Forwarded-Port")
	header.Del("X-Real-IP")
}
