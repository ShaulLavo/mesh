package apps

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shaul/mesh/internal/apppill"
	"github.com/shaul/mesh/internal/webauth"
)

type admission struct {
	Download   bool      `json:"download,omitempty"`
	ID         string    `json:"id"`
	Generation uint64    `json:"generation"`
	Method     string    `json:"method"`
	URI        string    `json:"uri"`
	Host       string    `json:"host"`
	Until      time.Time `json:"until"`
}

func (e *Edge) ServeHost(w http.ResponseWriter, r *http.Request, name string) bool {
	r = e.authenticateNetwork(r)
	if name == ManagementHost {
		e.management(w, r)
		return true
	}
	id := strings.TrimSuffix(name, "."+Domain)
	if !ValidID(id) {
		return false
	}
	app, exists, err := e.lookup(r.Context(), id, false)
	if err != nil {
		http.Error(w, "app unavailable", http.StatusServiceUnavailable)
		return true
	}
	if !exists {
		reserved, err := e.config.Store.AppNameExists(r.Context(), name)
		if err != nil {
			http.Error(w, "app unavailable", http.StatusServiceUnavailable)
			return true
		}
		if reserved {
			http.Error(w, "This temporary app has expired or was deleted.", http.StatusGone)
			return true
		}
		return false
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	privateAtLookup := app.Visibility == "private"
	if privateAtLookup {
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'self'")
	}
	if app.Status != "active" {
		http.Error(w, "This temporary app has expired or was deleted.", http.StatusGone)
		return true
	}
	if ticket := r.URL.Query().Get("mesh_view"); ticket != "" {
		if err := e.auth.ConsumeView(r.Context(), w, r, ticket, id); err != nil {
			http.Error(w, "View link expired. Open it again from Mesh.", http.StatusForbidden)
			return true
		}
		clean := r.URL.Query()
		clean.Del("mesh_view")
		r.URL.RawQuery = clean.Encode()
		http.Redirect(w, r, appReturn(id, URL(id)+r.URL.RequestURI()), http.StatusSeeOther) //nolint:gosec // appReturn fixes this consumed ticket redirect to the app host.
		return true
	}
	if app.Visibility == "private" {
		owner := networkOwns(r, app.Owner)
		if !owner {
			viewer, err := e.auth.ViewOwner(r.Context(), r, id)
			owner = err == nil && viewer == app.Owner
		}
		if !owner || !ambientOwnerAllowed(r, URL(id)) {
			origin := r.Header.Get("Origin")
			legacyNavigation := (r.Method == http.MethodGet || r.Method == http.MethodHead) && origin == "" &&
				r.Header.Get("Sec-Fetch-Site") == "" && r.Header.Get("Sec-Fetch-Mode") == "" &&
				r.Header.Get("Sec-Fetch-Dest") == "" && !strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
			if (origin == "" || origin == URL(id)) && (topLevelAppNavigation(r) || legacyNavigation) {
				http.Redirect(w, r, ManagementOrigin+"/view?id="+id, http.StatusSeeOther)
			} else {
				http.Error(w, "App is private. Open it from Mesh.", http.StatusForbidden)
			}
			return true
		}
	}
	if apppill.Asset(w, r) {
		return true
	}
	if !app.Ready {
		http.Error(w, "App is starting.", http.StatusServiceUnavailable)
		return true
	}
	if r.ContentLength > MaxArchive {
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return true
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxArchive)
	if e.config.Acquire != nil {
		release, err := e.config.Acquire(r, app.Owner)
		if err != nil {
			http.Error(w, "app busy", http.StatusServiceUnavailable)
			return true
		}
		defer release()
	} else {
		select {
		case e.slots <- struct{}{}:
			defer func() { <-e.slots }()
		default:
			http.Error(w, "app busy", http.StatusServiceUnavailable)
			return true
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	endpoint, err := e.config.Resolve(ctx, app.Owner)
	cancel()
	if err != nil {
		http.Error(w, "App origin is offline.", http.StatusServiceUnavailable)
		return true
	}
	app, admitted, release, err := e.admit(r, id)
	if err != nil {
		http.Error(w, "App access changed. Open it again from Mesh.", http.StatusForbidden)
		return true
	}
	r = admitted
	defer release()
	proof, err := Sign("mesh-app/admission/v1", app.Owner, app.Generation, admission{ID: id, Generation: app.Generation, Method: r.Method, URI: r.URL.RequestURI(), Host: name, Until: e.config.Now().Add(30 * time.Second)}, e.config.Key, e.config.Now())
	if err != nil {
		http.Error(w, "app unavailable", http.StatusServiceUnavailable)
		return true
	}
	encoded, err := json.Marshal(proof)
	if err != nil {
		http.Error(w, "app unavailable", http.StatusServiceUnavailable)
		return true
	}
	target := &url.URL{Scheme: "http", Host: endpoint.String()}
	proxy := &httputil.ReverseProxy{}
	proxy.Rewrite = func(out *httputil.ProxyRequest) {
		out.SetURL(target)
		request := out.Out
		request.URL.Path = "/.mesh-app/origin/" + id + request.URL.Path
		if request.URL.RawPath != "" {
			request.URL.RawPath = "/.mesh-app/origin/" + id + request.URL.RawPath
		}
		request.Host = name
		apppill.StripRequestCookies(request)
		request.Header.Set("X-Mesh-App-Admission", base64.RawURLEncoding.EncodeToString(encoded))
		request.Header.Set("X-Forwarded-Proto", "https")
		request.Header.Set("X-Forwarded-Host", name)
		request.Header.Del("Forwarded")
		request.Header.Del("X-Forwarded-For")
		request.Header.Del("X-Real-IP")
		if e.config.ClientIP != nil {
			ip := e.config.ClientIP(r)
			if ip.IsValid() {
				request.Header.Set("X-Forwarded-For", ip.String())
				request.Header.Set("X-Real-IP", ip.String())
			}
		}
		// A nil entry suppresses ReverseProxy's addition of the front-door address.
		if e.config.ClientIP == nil {
			request.Header["X-Forwarded-For"] = nil
		}
	}
	proxy.Transport = &http.Transport{Proxy: nil, DialContext: (&netDialer).DialContext, ResponseHeaderTimeout: 10 * time.Second, MaxResponseHeaderBytes: 1 << 20, DisableCompression: true}
	defer proxy.Transport.(*http.Transport).CloseIdleConnections()
	proxy.ModifyResponse = func(response *http.Response) error {
		apppill.StripCookies(response.Header)
		if app.Visibility == "private" {
			response.Header.Set("Cross-Origin-Resource-Policy", "same-origin")
			if !strings.EqualFold(strings.TrimSpace(response.Header.Get("X-Frame-Options")), "DENY") {
				response.Header.Set("X-Frame-Options", "SAMEORIGIN")
			}
		}
		if response.StatusCode == http.StatusSwitchingProtocols {
			if rw, ok := response.Body.(io.ReadWriteCloser); ok {
				stream := e.watchStream(app, rw)
				response.Body = stream
				context.AfterFunc(r.Context(), func() { _ = stream.Close() })
			}
		} else {
			response.Body = &activityBody{ReadCloser: response.Body, touch: func() {
				if r.Context().Err() == nil {
					_, _, _ = e.lookup(context.Background(), id, true)
				}
			}}
		}
		viewer, _ := e.auth.ViewOwner(r.Context(), r, id)
		return apppill.Inject(response, apppill.Config{AppID: id, ManagementOrigin: ManagementOrigin, Private: app.Visibility == "private", Owns: networkOwns(r, app.Owner) || viewer == app.Owner})
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		if app.Visibility == "private" {
			w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
			w.Header().Set("X-Frame-Options", "SAMEORIGIN")
			w.Header().Set("Content-Security-Policy", "frame-ancestors 'self'")
		}
		http.Error(w, "App origin is unavailable.", http.StatusServiceUnavailable)
	}
	if privateAtLookup {
		// Duplicate CORP values invalidate the browser's resource policy.
		w.Header().Del("Cross-Origin-Resource-Policy")
		w.Header().Del("X-Frame-Options")
		w.Header().Del("Content-Security-Policy")
	}
	proxy.ServeHTTP(w, r)
	return true
}

var netDialer = net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}

type activityBody struct {
	io.ReadCloser
	touch func()
	last  time.Time
}

func (b *activityBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 && time.Since(b.last) > time.Second {
		b.touch()
		b.last = time.Now()
	}
	return n, err
}

type wsActivity struct {
	header     [14]byte
	used, need int
	remaining  uint64
	data       bool
}

func (p *wsActivity) feed(b []byte) bool {
	active := false
	for len(b) > 0 {
		if p.remaining > 0 {
			take := min(uint64(len(b)), p.remaining)
			if p.data && take > 0 {
				active = true
			}
			p.remaining -= take
			b = b[take:]
			if p.remaining == 0 {
				p.used = 0
				p.need = 0
			}
			continue
		}
		if p.need == 0 {
			p.need = 2
		}
		take := min(p.need-p.used, len(b))
		copy(p.header[p.used:], b[:take])
		p.used += take
		b = b[take:]
		if p.used < p.need {
			continue
		}
		if p.need == 2 {
			length := int(p.header[1] & 127)
			extra := 0
			if length == 126 {
				extra = 2
			}
			if length == 127 {
				extra = 8
			}
			mask := 0
			if p.header[1]&128 != 0 {
				mask = 4
			}
			p.need = 2 + extra + mask
			if p.used < p.need {
				continue
			}
		}
		length := uint64(p.header[1] & 127)
		if length == 126 {
			length = uint64(p.header[2])<<8 | uint64(p.header[3])
		}
		if length == 127 {
			length = 0
			for _, v := range p.header[2:10] {
				length = length<<8 | uint64(v)
			}
		}
		opcode := p.header[0] & 15
		p.data = opcode == 0 || opcode == 1 || opcode == 2
		p.remaining = length
		if length == 0 {
			p.used = 0
			p.need = 0
		}
	}
	return active
}

type activeStream struct {
	io.ReadWriteCloser
	edge               *Edge
	app                Record
	incoming, outgoing wsActivity
	once               sync.Once
	done               chan struct{}
	last               time.Time
	mu                 sync.Mutex
}

func (e *Edge) watchStream(app Record, rw io.ReadWriteCloser) *activeStream {
	stream := &activeStream{ReadWriteCloser: rw, edge: e, app: app, done: make(chan struct{})}
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stream.done:
				return
			case <-ticker.C:
				current, _, err := e.lookup(context.Background(), app.ID, false)
				if err != nil || current.Status != "active" || current.Generation != app.Generation {
					_ = stream.Close()
					return
				}
			}
		}
	}()
	return stream
}
func (s *activeStream) touch(active bool) {
	if !active {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.last) < time.Second {
		return
	}
	_, _, _ = s.edge.lookup(context.Background(), s.app.ID, true)
	s.last = time.Now()
}
func (s *activeStream) Read(b []byte) (int, error) {
	n, err := s.ReadWriteCloser.Read(b)
	s.touch(s.outgoing.feed(b[:n]))
	return n, err
}
func (s *activeStream) Write(b []byte) (int, error) {
	n, err := s.ReadWriteCloser.Write(b)
	s.touch(s.incoming.feed(b[:n]))
	return n, err
}
func (s *activeStream) Close() error {
	var err error
	s.once.Do(func() { close(s.done); err = s.ReadWriteCloser.Close() })
	return err
}

var page = template.Must(template.New("page").Parse(`<!doctype html><html><head><meta name="viewport" content="width=device-width,initial-scale=1"><title>Mesh temporary apps</title><style>body{font:16px system-ui;background:#101214;color:#eef0f2;margin:0;padding:24px}main{max-width:680px;margin:auto}a{color:#9ddcff}button,a.button{border:0;border-radius:999px;background:#e9edf2;color:#101214;padding:10px 16px;cursor:pointer;display:inline-block;text-decoration:none}code{font-size:16px;overflow-wrap:anywhere}.pill{display:flex;align-items:center;gap:10px;padding:8px;flex-wrap:wrap}small{color:#bcc4cb}form{display:inline}body.frame{padding:0;border-radius:999px}.error{color:#ffbab4}</style></head><body class="{{if .Frame}}frame{{end}}"><main>{{if .Error}}<p class="error">{{.Error}}</p>{{end}}{{if .Code}}<h1>Pair this browser</h1><p>Approve from the Mesh host that owns the app:</p><code>mesh app browser approve HOST {{.Code}}</code><p>Code: <strong>{{.Code}}</strong></p><p>Expires in 10 minutes.</p><p id="pair-status" role="status">Waiting for approval. This page will continue automatically.</p><button id="check-approval" type="button">Check now</button><p><a id="restart-pair" href="/pair" hidden>Get a new code</a></p>{{else if .PairStart}}<h1>Pair this browser</h1><p>Start pairing, then approve the code on the Mesh host that owns your app.</p><form method="post" action="/pair"><input type="hidden" name="return" value="{{.Return}}"><button>Start pairing</button></form>{{else if .Frame}}<div class="pill"><strong>Mesh</strong><small>{{.App.Visibility}}</small><a href="{{.URL}}" target="_blank" rel="noopener">Share</a>{{if .Owns}}<a href="/confirm?id={{.App.ID}}&action={{.Toggle}}" target="_blank" rel="noopener">Make {{.Toggle}}</a><a href="/confirm?id={{.App.ID}}&action=renew" target="_blank" rel="noopener">Renew</a><a href="/download?id={{.App.ID}}" target="_blank" rel="noopener">Download</a><a href="/confirm?id={{.App.ID}}&action=delete" target="_blank" rel="noopener">Delete</a>{{else}}<a href="/pair" target="_blank" rel="noopener">Pair browser</a>{{end}}</div>{{else if .Confirm}}<h1>{{.Confirm}} {{.App.ID}}</h1><p>Owner controls for <a href="{{.URL}}">{{.URL}}</a>.</p><form id="confirm-form" method="post" action="/action"><input type="hidden" name="id" value="{{.App.ID}}"><input type="hidden" name="action" value="{{.Confirm}}"><input type="hidden" name="csrf" value="{{.CSRF}}"><input type="hidden" name="return" value="{{.Return}}">{{if or (eq .Confirm "public") (eq .Confirm "delete")}}<p><label>Type {{.App.ID}} to confirm <input id="confirm-id" name="confirmation" autocomplete="off" required></label></p>{{end}}<p><small>Read the change, then move the pointer or use the keyboard to enable confirmation.</small></p><button id="confirm-action" disabled>Confirm {{.Confirm}}</button></form>{{else}}<h1>Temporary apps</h1><p>Apps expire after 24 hours without app traffic. Expiry deletes their managed source, data and server.</p>{{range .Apps}}<p><a href="/view?id={{.ID}}">{{.ID}}.shaulavo.dev</a> · {{.Visibility}} · {{.Status}}<br><small>Expires {{.ExpiresAt}}</small></p>{{end}}<a href="/pair">Pair another owner</a>{{end}}</main>{{if .Frame}}<script>parent.postMessage({type:'mesh-app-status',visibility:{{.App.Visibility}},owns:{{.Owns}},deadline:{{.App.ExpiresAt.Format "2006-01-02T15:04:05Z07:00"}}},{{.Parent}})</script>{{end}}{{if .Code}}<script>
const checkButton = document.getElementById('check-approval');
const status = document.getElementById('pair-status');
const expiresAt = performance.now() + {{.PairLifetimeMS}};
let checking = false;
let timer;
let finished = false;
function expired() {
  finished = true;
  status.textContent = 'This code expired. Get a new code to try again.';
  checkButton.hidden = true;
  document.getElementById('restart-pair').hidden = false;
}
async function checkApproval() {
  if (checking || finished) return;
  clearTimeout(timer);
  if (performance.now() >= expiresAt) { expired(); return; }
  checking = true;
  checkButton.disabled = true;
  const abort = new AbortController();
  const timeout = setTimeout(() => abort.abort(), 10000);
  try {
    const response = await fetch('/pair/status', {credentials: 'same-origin', cache: 'no-store', signal: abort.signal});
    if (response.status === 200) { finished = true; location.replace({{.Return}}); return; }
    if (response.status === 410) { expired(); return; }
    status.textContent = response.status === 202
      ? 'Waiting for approval. This page will continue automatically.'
      : 'Could not check approval. Retrying automatically.';
  } catch {
    status.textContent = 'Could not check approval. Retrying automatically.';
  } finally {
    clearTimeout(timeout);
    checking = false;
    checkButton.disabled = false;
    if (!finished) timer = setTimeout(checkApproval, 1500);
  }
}
checkButton.addEventListener('click', checkApproval);
document.addEventListener('visibilitychange', () => { if (!document.hidden) checkApproval(); });
checkApproval();
</script>{{end}}{{if .Confirm}}<script>
const confirmButton = document.getElementById('confirm-action');
const confirmForm = document.getElementById('confirm-form');
const confirmID = document.getElementById('confirm-id');
let focusedAt = null;
let deliberate = false;
let activationTimer;
function activePage() { return document.visibilityState === 'visible' && document.hasFocus(); }
function updateActivation() {
  confirmButton.disabled = !activePage() || focusedAt === null || performance.now() - focusedAt < 750 || !deliberate || (confirmID && confirmID.value !== {{.App.ID}});
}
function resetActivation() {
  clearTimeout(activationTimer);
  deliberate = false;
  focusedAt = activePage() ? performance.now() : null;
  confirmButton.disabled = true;
  if (focusedAt !== null) activationTimer = setTimeout(updateActivation, 750);
}
function deliberateInput(event) {
  if (event.isTrusted && activePage()) {
    deliberate = true;
    updateActivation();
  }
}
window.addEventListener('focus', resetActivation);
window.addEventListener('blur', resetActivation);
window.addEventListener('pageshow', resetActivation);
document.addEventListener('visibilitychange', resetActivation);
document.addEventListener('pointermove', deliberateInput);
document.addEventListener('keydown', deliberateInput);
if (confirmID) confirmID.addEventListener('input', updateActivation);
confirmForm.addEventListener('submit', event => {
  updateActivation();
  if (confirmButton.disabled) event.preventDefault();
});
resetActivation();
</script>{{end}}</body></html>`))

type pageData struct {
	Error, Code, Return, URL, Toggle, Confirm, CSRF, Parent string
	Frame, Owns, PairStart                                  bool
	PairLifetimeMS                                          int64
	App                                                     Record
	Apps                                                    []Record
}

func render(w http.ResponseWriter, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = page.Execute(w, data)
}
func (e *Edge) management(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; connect-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	if r.URL.Path == "/pair/status" {
		e.pairStatus(w, r)
		return
	}
	if r.URL.Path == "/pair" {
		e.pairPage(w, r)
		return
	}
	if r.URL.Path == "/frame" {
		e.frame(w, r)
		return
	}
	if _, cookieErr := r.Cookie(webauth.PairCookie); cookieErr == nil {
		if _, promoteErr := e.auth.Promote(r.Context(), w, r); promoteErr == nil {
			http.Redirect(w, r, ManagementOrigin+managementReturn(r), http.StatusSeeOther)
			return
		}
	}
	session, err := e.browser(r)
	if err != nil {
		if r.Method != "GET" {
			http.Error(w, "Pair your browser first", http.StatusUnauthorized)
			return
		}
		pair, pairErr := e.auth.Pending(r.Context(), r)
		if pairErr == nil {
			render(w, pageData{Code: pair.Code, Return: managementReturn(r), PairLifetimeMS: max(0, pair.ExpiresAt.Sub(e.config.Now()).Milliseconds())})
		} else {
			render(w, pageData{PairStart: true, Return: managementReturn(r)})
		}
		return
	}
	if r.URL.Path == "/action" {
		e.mutate(w, r)
		return
	}
	id := r.URL.Query().Get("id")
	if id != "" {
		app, exists, err := e.lookup(r.Context(), id, false)
		if err != nil || !exists || !session.Owns(app.Owner) {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/download" && r.Method == "GET" {
			e.downloadSource(w, r, app)
			return
		}
		if r.URL.Path == "/view" {
			if app.Status != "active" {
				http.Error(w, "App expired", http.StatusGone)
				return
			}
			destination := appReturn(id, r.URL.Query().Get("return"))
			if app.Visibility == "public" || networkOwns(r, app.Owner) {
				http.Redirect(w, r, destination, http.StatusSeeOther) //nolint:gosec // appReturn fixes the destination to this registry-owned app host.
				return
			}
			ticket, err := e.auth.IssueView(r.Context(), r, app.Owner, id)
			if err != nil {
				http.Error(w, "Cannot issue private view", http.StatusForbidden)
				return
			}
			target, _ := url.Parse(destination)
			query := target.Query()
			query.Set("mesh_view", ticket)
			target.RawQuery = query.Encode()
			http.Redirect(w, r, target.String(), http.StatusSeeOther) //nolint:gosec // appReturn fixes the host; URL.Query escapes the view ticket.
			return
		}
		if r.URL.Path == "/confirm" {
			action := r.URL.Query().Get("action")
			if action != "public" && action != "private" && action != "delete" && action != "renew" {
				http.NotFound(w, r)
				return
			}
			policy := strings.Replace(w.Header().Get("Content-Security-Policy"), "form-action 'self'", "form-action 'self' "+URL(id), 1)
			w.Header().Set("Content-Security-Policy", policy)
			render(w, pageData{Confirm: action, App: app, CSRF: session.CSRF, URL: URL(id), Return: appReturn(id, r.URL.Query().Get("return"))})
			return
		}
	}
	if r.URL.Path != "/" || r.Method != "GET" {
		http.NotFound(w, r)
		return
	}
	e.mu.Lock()
	apps := []Record{}
	for _, app := range e.state.Apps {
		if session.Owns(app.Owner) && app.Cleanup != "complete" {
			apps = append(apps, app)
		}
	}
	e.mu.Unlock()
	render(w, pageData{Apps: apps})
}
func (e *Edge) pairPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.Method == http.MethodPost {
		e.startPairing(w, r)
		return
	}
	destination := pairingReturn(r.URL.Query().Get("return"))
	if _, err := e.auth.Promote(r.Context(), w, r); err == nil {
		http.Redirect(w, r, ManagementOrigin+destination, http.StatusSeeOther) //nolint:gosec // pairingReturn permits only local paths on this fixed management origin.
		return
	}
	pair, err := e.auth.Pending(r.Context(), r)
	if err != nil {
		render(w, pageData{PairStart: true, Return: destination})
		return
	}
	render(w, pageData{Code: pair.Code, Return: destination, PairLifetimeMS: max(0, pair.ExpiresAt.Sub(e.config.Now()).Milliseconds())})
}

func (e *Edge) startPairing(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") != ManagementOrigin {
		http.Error(w, "Start pairing from Mesh", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid pairing request", http.StatusBadRequest)
		return
	}
	destination := pairingReturn(r.PostForm.Get("return"))
	var ip netip.Addr
	if e.config.ClientIP != nil {
		ip = e.config.ClientIP(r)
	} else if address, parseErr := netip.ParseAddrPort(r.RemoteAddr); parseErr == nil {
		ip = address.Addr()
	}
	pair, err := e.auth.Begin(r.Context(), w, r, ip)
	if err != nil {
		http.Error(w, "Pairing unavailable. Try again in a minute.", http.StatusTooManyRequests)
		return
	}
	render(w, pageData{Code: pair.Code, Return: destination, PairLifetimeMS: max(0, pair.ExpiresAt.Sub(e.config.Now()).Milliseconds())})
}

func pairingReturn(value string) string {
	target, err := url.Parse(value)
	if err != nil || target.IsAbs() || target.Host != "" || target.Opaque != "" {
		return "/"
	}
	return managementReturn(&http.Request{URL: target})
}

func (e *Edge) pairStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	_, err := e.auth.Promote(r.Context(), w, r)
	if err == nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	if errors.Is(err, webauth.ErrApprovalPending) {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if errors.Is(err, webauth.ErrPairing) {
		http.Error(w, "Pairing expired. Start again.", http.StatusGone)
		return
	}
	http.Error(w, "Approval check unavailable", http.StatusServiceUnavailable)
}
func (e *Edge) frame(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.NotFound(w, r)
		return
	}
	id := r.URL.Query().Get("id")
	app, exists, err := e.lookup(r.Context(), id, false)
	if err != nil || !exists {
		http.NotFound(w, r)
		return
	}
	session, _ := e.browser(r)
	owns := session.Owns(app.Owner)
	if app.Visibility == "private" && !owns {
		http.NotFound(w, r)
		return
	}
	toggle := "private"
	if app.Visibility == "private" {
		toggle = "public"
	}
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; form-action 'none'; base-uri 'none'; frame-ancestors "+URL(id))
	render(w, pageData{Frame: true, App: app, Owns: owns, URL: URL(id), Toggle: toggle, Parent: URL(id)})
}
func (e *Edge) mutate(w http.ResponseWriter, r *http.Request) {
	session, err := e.browser(r)
	if err == nil {
		err = webauth.ValidateSessionMutation(r, ManagementOrigin, session)
	}
	if err != nil {
		http.Error(w, "Owner authorization required", http.StatusForbidden)
		return
	}
	if r.Header.Get("Sec-Fetch-Dest") == "iframe" {
		http.Error(w, "Open owner controls in their own tab", http.StatusForbidden)
		return
	}
	id := r.FormValue("id")
	action := r.FormValue("action")
	if action != "public" && action != "private" && action != "delete" && action != "renew" {
		http.Error(w, "Invalid action", http.StatusBadRequest)
		return
	}
	e.mu.Lock()
	session, err = e.browser(r)
	if err != nil {
		e.mu.Unlock()
		http.Error(w, "Owner authorization required", http.StatusForbidden)
		return
	}
	app, exists := e.state.Apps[id]
	if !exists || !session.Owns(app.Owner) {
		e.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	before, _ := json.Marshal(e.state)
	_, err = e.apply(r.Context(), app.Owner, Request{Action: action, ID: id})
	if err == nil {
		err = e.persist(r.Context())
	}
	if err != nil {
		_ = json.Unmarshal(before, &e.state)
	}
	e.mu.Unlock()
	if err != nil {
		http.Error(w, "Change failed", http.StatusServiceUnavailable)
		return
	}
	destination := "/view?" + url.Values{"id": {id}, "return": {appReturn(id, r.PostForm.Get("return"))}}.Encode()
	if action == "delete" {
		destination = "/"
	}
	http.Redirect(w, r, destination, http.StatusSeeOther)
}

func (e *Edge) downloadSource(w http.ResponseWriter, r *http.Request, app Record) {
	if app.Status != "active" {
		http.Error(w, "App expired", http.StatusGone)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	endpoint, err := e.config.Resolve(ctx, app.Owner)
	if err != nil {
		http.Error(w, "App origin offline", http.StatusServiceUnavailable)
		return
	}
	session, err := e.browser(r)
	if err != nil || !session.Owns(app.Owner) {
		http.Error(w, "Owner authorization required", http.StatusForbidden)
		return
	}
	proof, err := Sign("mesh-app/admission/v1", app.Owner, app.Generation, admission{ID: app.ID, Generation: app.Generation, Method: "GET", URI: "/", Host: app.ID + "." + Domain, Until: e.config.Now().Add(30 * time.Second), Download: true}, e.config.Key, e.config.Now())
	if err != nil {
		http.Error(w, "Download unavailable", http.StatusServiceUnavailable)
		return
	}
	encoded, _ := json.Marshal(proof)
	request, err := http.NewRequestWithContext(ctx, "GET", "http://"+endpoint.String()+"/.mesh-app/origin/"+app.ID+"/", nil)
	if err != nil {
		http.Error(w, "Download unavailable", http.StatusServiceUnavailable)
		return
	}
	request.Header.Set("X-Mesh-App-Admission", base64.RawURLEncoding.EncodeToString(encoded))
	transport := &http.Transport{Proxy: nil, DialContext: netDialer.DialContext, ResponseHeaderTimeout: 30 * time.Second}
	defer transport.CloseIdleConnections()
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		http.Error(w, "Download unavailable", http.StatusServiceUnavailable)
		return
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != 200 {
		http.Error(w, "Download unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+app.ID+`.tar.gz"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.Copy(w, io.LimitReader(response.Body, MaxArchive+1))
}

func managementReturn(r *http.Request) string {
	var path string
	switch r.URL.Path {
	case "/":
		path = "/"
	case "/view":
		path = "/view"
	case "/confirm":
		path = "/confirm"
	default:
		return "/"
	}
	if len(r.URL.RawQuery) > 4096 {
		return "/"
	}
	query := r.URL.Query()
	keys := make([]string, 0, len(query))
	for key := range query {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		for _, value := range query[key] {
			parts = append(parts, url.QueryEscape(key)+"="+url.QueryEscape(value))
		}
	}
	if len(parts) == 0 {
		return path
	}
	return path + "?" + strings.Join(parts, "&")
}

// appReturn accepts only a page belonging to the app being managed.
func appReturn(id, value string) string {
	fallback := URL(id) + "/"
	if len(value) > 4096 || strings.ContainsAny(value, "\r\n\\") {
		return fallback
	}
	target, err := url.Parse(value)
	if err != nil || target.Scheme != "https" || target.Host != id+"."+Domain || target.User != nil || target.Opaque != "" {
		return fallback
	}
	if target.Path == "" {
		target.Path = "/"
	}
	return target.String()
}
