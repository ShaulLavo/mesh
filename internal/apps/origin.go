package apps

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	meshserve "github.com/shaul/mesh/internal/serve"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Workers interface {
	Start(context.Context, string, string, string, []string) (string, error)
	Stop(context.Context, string) error
	Find(context.Context, string) (string, bool, error)
	Forget(context.Context, string)
}
type OriginConfig struct {
	CheckHosting func(int, string) error
	Store        StateStore
	Key          ed25519.PrivateKey
	EdgeIdentity string
	Exchange     func(context.Context, Signed) (Signed, error)
	Workers      Workers
	DataRoot     string
	Now          func() time.Time
}
type localApp struct {
	Record  Record   `json:"record"`
	Root    string   `json:"root"`
	Command string   `json:"command,omitempty"`
	Port    int      `json:"port,omitempty"`
	Env     []string `json:"env,omitempty"`
	Session string   `json:"session,omitempty"`
	Phase   string   `json:"phase"`
}
type upload struct {
	ID        string    `json:"id"`
	Size      int64     `json:"size"`
	ExpiresAt time.Time `json:"expiresAt"`
}
type createReceipt struct {
	Record Record `json:"record"`
	Digest string `json:"digest"`
}
type originState struct {
	Receipts map[string]createReceipt `json:"receipts"`
	Apps     map[string]localApp      `json:"apps"`
	Uploads  map[string]upload        `json:"uploads"`
	Sequence uint64                   `json:"sequence"`
	Pending  *Signed                  `json:"pending,omitempty"`
}
type Origin struct {
	mu          sync.Mutex
	config      OriginConfig
	state       originState
	identity    string
	admissionMu sync.Mutex
	admissions  map[string]time.Time
}

func NewOrigin(ctx context.Context, c OriginConfig) (*Origin, error) {
	if c.Store == nil || len(c.Key) != ed25519.PrivateKeySize || c.Exchange == nil || c.Workers == nil || c.DataRoot == "" {
		return nil, errors.New("app: missing origin dependencies")
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	o := &Origin{config: c, identity: base64.RawURLEncoding.EncodeToString(c.Key.Public().(ed25519.PublicKey)), state: originState{Receipts: map[string]createReceipt{}, Apps: map[string]localApp{}, Uploads: map[string]upload{}}, admissions: map[string]time.Time{}}
	if err := load(ctx, c.Store, "apps.origin", &o.state); err != nil {
		return nil, err
	}
	return o, nil
}
func (o *Origin) persist(ctx context.Context) error {
	return save(ctx, o.config.Store, "apps.origin", o.state)
}
func (o *Origin) edge(ctx context.Context, q Request) (Result, error) {
	if o.state.Pending != nil {
		pending := *o.state.Pending
		result, err := o.settle(ctx, pending)
		if err != nil {
			return Result{}, err
		}
		expected, _ := json.Marshal(q)
		if digestBytes(expected) == digestBytes(pending.Body) {
			return result, nil
		}
	}
	o.state.Sequence++
	s, err := Sign("mesh-app/request/v1", o.config.EdgeIdentity, o.state.Sequence, q, o.config.Key, o.config.Now())
	if err != nil {
		return Result{}, err
	}
	o.state.Pending = &s
	if err := o.persist(ctx); err != nil {
		return Result{}, err
	}
	return o.settle(ctx, s)
}
func (o *Origin) settle(ctx context.Context, s Signed) (Result, error) {
	s, err := refresh(s, o.config.Key, o.config.Now())
	if err != nil {
		return Result{}, err
	}
	response, err := o.config.Exchange(ctx, s)
	if err != nil {
		return Result{}, err
	}
	if err := response.Verify("mesh-app/response/v1", o.identity, o.config.EdgeIdentity, o.config.Now()); err != nil {
		return Result{}, err
	}
	var reply edgeReply
	if err := decode(response.Body, &reply); err != nil {
		return Result{}, err
	}
	if reply.RequestID != s.ID || response.Sequence != s.Sequence {
		return Result{}, errors.New("app: mismatched edge acknowledgement")
	}
	o.state.Pending = nil
	if err := o.persist(ctx); err != nil {
		return Result{}, err
	}
	if reply.Error != "" {
		return Result{}, errors.New(reply.Error)
	}
	return reply.Result, nil
}
func (o *Origin) Handle(ctx context.Context, q Request) (Result, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	switch q.Action {
	case "upload.begin":
		return o.beginUpload(ctx)
	case "upload.chunk":
		return o.appendUpload(ctx, q)
	case "create", "update":
		return o.create(ctx, q)
	case "download":
		return o.download(ctx, q)
	case "list", "public", "private", "renew", "browser.approve", "browser.list", "browser.revoke":
		return o.edge(ctx, q)
	case "inspect":
		result, err := o.edge(ctx, q)
		if err != nil {
			return result, err
		}
		if a, ok := o.state.Apps[q.ID]; ok {
			result.Runtime = &RuntimeInfo{Phase: a.Phase, SessionID: a.Session, Command: a.Command, Port: a.Port, Root: a.Root}
		}
		return result, nil
	case "delete":
		result, err := o.edge(ctx, q)
		if err != nil {
			return Result{}, err
		}
		if err := o.cleanup(ctx, q.ID); err != nil {
			return result, err
		}
		_, err = o.edge(ctx, Request{Action: "cleanup", ID: q.ID})
		return result, err
	}
	return Result{}, errors.New("app: unsupported operation")
}
func (o *Origin) ensureRoot() error {
	root := o.config.DataRoot
	if o.config.CheckHosting != nil {
		if err := o.config.CheckHosting(0, root); err != nil {
			return err
		}
	}
	if err := validateDataRoot(root); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(root, "uploads"), 0700); err != nil {
		return err
	}
	return os.MkdirAll(filepath.Join(root, "apps"), 0700)
}
func (o *Origin) uploadPath(id string) string {
	return filepath.Join(o.config.DataRoot, "uploads", id+".tar.gz")
}
func (o *Origin) beginUpload(ctx context.Context) (Result, error) {
	if err := o.ensureRoot(); err != nil {
		return Result{}, err
	}
	if len(o.state.Uploads) >= 32 {
		return Result{}, errors.New("app: upload capacity reached")
	}
	id, err := RandomToken()
	if err != nil {
		return Result{}, err
	}
	f, err := os.OpenFile(o.uploadPath(id), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return Result{}, err
	}
	if err = f.Close(); err != nil {
		return Result{}, err
	}
	o.state.Uploads[id] = upload{ID: id, ExpiresAt: o.config.Now().Add(time.Hour)}
	if err := o.persist(ctx); err != nil {
		return Result{}, err
	}
	return Result{UploadID: id}, nil
}
func (o *Origin) appendUpload(ctx context.Context, q Request) (Result, error) {
	u, ok := o.state.Uploads[q.UploadID]
	if !ok || !o.config.Now().Before(u.ExpiresAt) {
		return Result{}, errors.New("app: upload not found or expired")
	}
	if len(q.Data) > ChunkSize || q.Offset < 0 || q.Offset+int64(len(q.Data)) > MaxArchive {
		return Result{}, errors.New("app: upload chunk exceeds limit")
	}
	f, err := os.OpenFile(o.uploadPath(u.ID), os.O_RDWR, 0600)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return Result{}, err
	}
	if q.Offset < info.Size() {
		existing := make([]byte, len(q.Data))
		n, err := f.ReadAt(existing, q.Offset)
		if err != nil || n != len(existing) || digestBytes(existing) != digestBytes(q.Data) {
			return Result{}, errors.New("app: upload retry conflicts")
		}
		return Result{UploadID: u.ID}, nil
	}
	if q.Offset != info.Size() {
		return Result{}, errors.New("app: upload offset mismatch")
	}
	if _, err := f.WriteAt(q.Data, q.Offset); err != nil {
		return Result{}, err
	}
	if err := f.Sync(); err != nil {
		return Result{}, err
	}
	u.Size = q.Offset + int64(len(q.Data))
	o.state.Uploads[u.ID] = u
	return Result{UploadID: u.ID}, o.persist(ctx)
}
func validateRecipe(q Request) error {
	if q.Kind == "" {
		q.Kind = "static"
	}
	if q.Kind != "static" && q.Kind != "server" {
		return errors.New("app: unknown runtime kind")
	}
	if q.Kind == "server" && (q.Command == "" || q.Port < 1024 || q.Port > 65535) {
		return errors.New("app: server requires --run and --port 1024..65535")
	}
	if len(q.Command) > 8192 || len(q.Setup) > 8192 || len(q.Env) > 64 {
		return errors.New("app: recipe exceeds limits")
	}
	totalEnv := 0
	for _, env := range q.Env {
		totalEnv += len(env)
		if len(env) > 8192 || totalEnv > 64<<10 {
			return errors.New("app: environment exceeds limits")
		}
		key, _, ok := strings.Cut(env, "=")
		if !ok || key == "" || key == "HOME" || key == "CODEX_HOME" || strings.ContainsAny(key, "\x00\r\n") {
			return errors.New("app: invalid environment")
		}
	}
	return nil
}
func (o *Origin) create(ctx context.Context, q Request) (Result, error) {
	if prior, ok := o.state.Receipts[q.UploadID]; ok {
		normalized, _ := json.Marshal(q)
		if prior.Digest != digestBytes(normalized) {
			return Result{}, errors.New("app: commit retry conflicts with previous recipe")
		}
		latest, err := o.edge(ctx, Request{Action: "inspect", ID: prior.Record.ID})
		return latest, err
	}
	if q.Kind == "" {
		q.Kind = "static"
	}
	if err := validateRecipe(q); err != nil {
		return Result{}, err
	}
	if o.config.CheckHosting != nil {
		if err := o.config.CheckHosting(q.Port, o.config.DataRoot); err != nil {
			return Result{}, err
		}
	}
	u, ok := o.state.Uploads[q.UploadID]
	if !ok || !o.config.Now().Before(u.ExpiresAt) {
		return Result{}, errors.New("app: missing source upload")
	}
	for id, existing := range o.state.Apps {
		if id != q.ID && q.Kind == "server" && existing.Record.Kind == "server" && existing.Port == q.Port {
			return Result{}, errors.New("app: port already belongs to another app")
		}
	}
	staging := filepath.Join(o.config.DataRoot, "uploads", "source-"+q.UploadID)
	if err := os.MkdirAll(staging, 0700); err != nil {
		return Result{}, err
	}
	if err := unpack(o.uploadPath(q.UploadID), staging, q.Digest); err != nil {
		_ = os.RemoveAll(staging)
		return Result{}, err
	}
	var app Record
	var previous localApp
	if q.Action == "update" {
		result, err := o.edge(ctx, Request{Action: "inspect", ID: q.ID})
		if err != nil {
			_ = os.RemoveAll(staging)
			return Result{}, err
		}
		app = *result.App
		if app.Status != "active" {
			_ = os.RemoveAll(staging)
			return Result{}, errors.New("app: cannot update expired app")
		}
		previous, ok = o.state.Apps[q.ID]
		if !ok {
			return Result{}, errors.New("app: managed workspace missing")
		}
	} else {
		result, err := o.edge(ctx, Request{Action: "allocate", Kind: q.Kind})
		if err != nil {
			_ = os.RemoveAll(staging)
			return Result{}, err
		}
		app = *result.App
	}
	workspace := filepath.Join(o.config.DataRoot, "apps", app.ID, "source-"+q.UploadID)
	if err := os.MkdirAll(filepath.Dir(workspace), 0700); err != nil {
		return Result{}, err
	}
	if err := os.Rename(staging, workspace); err != nil {
		return Result{}, err
	}
	local := localApp{Record: app, Root: workspace, Command: q.Command, Port: q.Port, Env: q.Env, Phase: "preparing"}
	if q.Action == "create" {
		o.state.Apps[app.ID] = local
		if err := o.persist(ctx); err != nil {
			return Result{}, err
		}
	}
	fail := func(cause error) (Result, error) {
		if q.Action == "create" {
			return Result{}, o.failCreate(ctx, app.ID, cause)
		}
		_ = o.stop(ctx, app.ID)
		o.state.Apps[app.ID] = previous
		if previous.Record.Kind == "server" {
			session, startErr := o.config.Workers.Start(ctx, "app "+app.ID, previous.Command, previous.Root, o.environment(previous))
			previous.Session = session
			o.state.Apps[app.ID] = previous
			cause = errors.Join(cause, startErr)
		}
		_ = os.RemoveAll(workspace)
		return Result{}, errors.Join(cause, o.persist(ctx))
	}
	if q.Setup != "" {
		if err := validateDataRoot(workspace); err != nil {
			return fail(err)
		}
		id, err := o.config.Workers.Start(ctx, "app-setup "+app.ID, q.Setup, workspace, o.environment(local))
		if err != nil {
			return fail(err)
		}
		if err := o.waitSetup(ctx, id); err != nil {
			return fail(err)
		}
		o.config.Workers.Forget(ctx, "app-setup "+app.ID)
	}
	if q.Action == "update" {
		if err := o.stop(ctx, app.ID); err != nil {
			return fail(err)
		}
	}
	refreshed, err := o.edge(ctx, Request{Action: "inspect", ID: app.ID})
	if err != nil {
		return fail(err)
	}
	local.Record = *refreshed.App
	local.Phase = "activating"
	local.Record.Revision = q.UploadID
	o.state.Apps[app.ID] = local
	if err := o.persist(ctx); err != nil {
		return Result{}, err
	}
	if q.Kind == "server" {
		if err := ProbePortAvailable(q.Port); err != nil {
			return fail(err)
		}
		session, err := o.config.Workers.Start(ctx, "app "+app.ID, q.Command, workspace, o.environment(local))
		if err != nil {
			return fail(err)
		}
		local.Session = session
		o.state.Apps[app.ID] = local
		if err := o.persist(ctx); err != nil {
			return Result{}, err
		}
		if err := waitPort(ctx, q.Port); err != nil {
			return fail(err)
		}
		if err := checkServerListener(ctx, q.Port); err != nil {
			return fail(err)
		}
		if _, alive, err := o.config.Workers.Find(ctx, "app "+app.ID); err != nil || !alive {
			return fail(errors.Join(err, errors.New("app: server exited during startup")))
		}
	}
	result, err := o.edge(ctx, Request{Action: "activate", ID: app.ID, Kind: q.Kind, UploadID: q.UploadID})
	if err != nil {
		return Result{}, err
	}
	local.Record = *result.App
	local.Phase = "ready"
	o.state.Apps[app.ID] = local
	if o.state.Receipts == nil {
		o.state.Receipts = map[string]createReceipt{}
	}
	for token, receipt := range o.state.Receipts {
		if receipt.Record.ID == app.ID {
			delete(o.state.Receipts, token)
		}
	}
	normalized, _ := json.Marshal(q)
	o.state.Receipts[q.UploadID] = createReceipt{Record: *result.App, Digest: digestBytes(normalized)}
	delete(o.state.Uploads, q.UploadID)
	if err := o.persist(ctx); err != nil {
		return Result{}, err
	}
	_ = os.Remove(o.uploadPath(q.UploadID))
	if previous.Root != "" && previous.Root != workspace {
		_ = os.RemoveAll(previous.Root)
	}
	return result, nil
}
func (o *Origin) environment(a localApp) []string {
	env := append([]string{}, a.Env...)
	return append(env, "PORT="+fmt.Sprint(a.Port), "HOST=127.0.0.1", "XDG_CACHE_HOME="+filepath.Join(a.Root, ".cache"), "npm_config_cache="+filepath.Join(a.Root, ".cache", "npm"), "PIP_CACHE_DIR="+filepath.Join(a.Root, ".cache", "pip"), "UV_CACHE_DIR="+filepath.Join(a.Root, ".cache", "uv"))
}
func (o *Origin) waitSetup(ctx context.Context, id string) error {
	waiter, ok := o.config.Workers.(interface {
		Wait(context.Context, string) (int, error)
	})
	if !ok {
		return errors.New("app: worker completion unavailable")
	}
	code, err := waiter.Wait(ctx, id)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("app: setup exited %d", code)
	}
	return nil
}
func waitPort(ctx context.Context, port int) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		for _, address := range []string{"127.0.0.1", "::1"} {
			conn, err := (&net.Dialer{Timeout: 100 * time.Millisecond}).DialContext(ctx, "tcp", net.JoinHostPort(address, fmt.Sprint(port)))
			if err == nil {
				_ = conn.Close()
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return errors.New("app: server readiness timed out")
		case <-ticker.C:
		}
	}
}
func (o *Origin) failCreate(ctx context.Context, id string, err error) error {
	_, deleteErr := o.edge(ctx, Request{Action: "delete", ID: id})
	cleanupErr := o.cleanup(ctx, id)
	return errors.Join(err, deleteErr, cleanupErr)
}
func (o *Origin) stop(ctx context.Context, id string) error {
	a, ok := o.state.Apps[id]
	if !ok {
		return nil
	}
	for _, label := range []string{"app " + id, "app-setup " + id} {
		session, alive, err := o.config.Workers.Find(ctx, label)
		if err != nil {
			return err
		}
		if alive {
			if err := o.config.Workers.Stop(ctx, session); err != nil {
				return err
			}
		}
	}
	a.Session = ""
	o.state.Apps[id] = a
	return nil
}
func (o *Origin) cleanup(ctx context.Context, id string) error {
	if !ValidID(id) {
		return errors.New("app: invalid cleanup id")
	}
	if err := o.stop(ctx, id); err != nil {
		return err
	}
	root, err := os.OpenRoot(o.config.DataRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := root.RemoveAll(filepath.Join("apps", id)); err != nil {
		return err
	}
	o.config.Workers.Forget(ctx, "app "+id)
	o.config.Workers.Forget(ctx, "app-setup "+id)
	delete(o.state.Apps, id)
	for token, receipt := range o.state.Receipts {
		if receipt.Record.ID == id {
			delete(o.state.Receipts, token)
		}
	}
	return o.persist(ctx)
}
func (o *Origin) Sync(ctx context.Context) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	result, err := o.edge(ctx, Request{Action: "sync"})
	if err != nil {
		for id, a := range o.state.Apps {
			if !o.config.Now().Before(a.Record.LeaseUntil) {
				_ = o.stop(ctx, id)
			}
		}
		return err
	}
	for _, app := range result.Apps {
		if app.Status != "active" {
			if err := o.cleanup(ctx, app.ID); err != nil {
				return err
			}
			if _, err := o.edge(ctx, Request{Action: "cleanup", ID: app.ID}); err != nil {
				return err
			}
			continue
		}
		a, ok := o.state.Apps[app.ID]
		if !ok {
			if !app.Ready {
				_, _ = o.edge(ctx, Request{Action: "delete", ID: app.ID})
			}
			continue
		}
		a.Record = app
		o.state.Apps[app.ID] = a
		if a.Phase == "activating" {
			if a.Command != "" {
				_, alive, err := o.config.Workers.Find(ctx, "app "+app.ID)
				if err != nil {
					return err
				}
				if !alive {
					if err := ProbePortAvailable(a.Port); err != nil {
						return err
					}
					session, err := o.config.Workers.Start(ctx, "app "+app.ID, a.Command, a.Root, o.environment(a))
					if err != nil {
						return err
					}
					a.Session = session
				}
				if err := waitPort(ctx, a.Port); err != nil {
					return err
				}
				if err := checkServerListener(ctx, a.Port); err != nil {
					return err
				}
			}
			activated, err := o.edge(ctx, Request{Action: "activate", ID: app.ID, Kind: func() string {
				if a.Command != "" {
					return "server"
				}
				return "static"
			}(), UploadID: a.Record.Revision})
			if err != nil {
				return err
			}
			a.Record = *activated.App
			a.Phase = "ready"
			o.state.Apps[app.ID] = a
		}
		if a.Phase != "ready" {
			_, _ = o.edge(ctx, Request{Action: "delete", ID: app.ID})
			continue
		}
		if app.Kind == "server" {
			_, alive, err := o.config.Workers.Find(ctx, "app "+app.ID)
			if err != nil {
				return err
			}
			if !alive {
				if err := ProbePortAvailable(a.Port); err != nil {
					return err
				}
				id, err := o.config.Workers.Start(ctx, "app "+app.ID, a.Command, a.Root, o.environment(a))
				if err != nil {
					return err
				}
				a.Session = id
				if err := waitPort(ctx, a.Port); err != nil {
					_ = o.stop(ctx, app.ID)
					return err
				}
				if err := checkServerListener(ctx, a.Port); err != nil {
					_ = o.stop(ctx, app.ID)
					return err
				}
				o.state.Apps[app.ID] = a
			}
		}
	}
	for id, u := range o.state.Uploads {
		if !o.config.Now().Before(u.ExpiresAt) {
			_ = os.Remove(o.uploadPath(id))
			delete(o.state.Uploads, id)
		}
	}
	return o.persist(ctx)
}
func (o *Origin) download(ctx context.Context, q Request) (Result, error) {
	if q.Offset < 0 || q.Offset > MaxArchive {
		return Result{}, errors.New("app: invalid download offset")
	}
	a, ok := o.state.Apps[q.ID]
	if !ok {
		return Result{}, errors.New("app: app not found")
	}
	result, err := o.edge(ctx, Request{Action: "inspect", ID: q.ID})
	if err != nil || result.App.Status != "active" {
		return Result{}, errors.Join(err, errors.New("app: app expired"))
	}
	archive := filepath.Join(o.config.DataRoot, "apps", q.ID, "download.tar.gz")
	if q.Offset == 0 {
		f, err := os.Create(archive) //nolint:gosec // The archive is inside the looked-up app managed workspace.
		if err != nil {
			return Result{}, err
		}
		_, packErr := Pack(ctx, a.Root, f)
		if closeErr := f.Close(); packErr != nil || closeErr != nil {
			return Result{}, errors.Join(packErr, closeErr)
		}
	}
	f, err := os.Open(archive) //nolint:gosec // The archive is inside the looked-up app managed workspace.
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = f.Close() }()
	b := make([]byte, ChunkSize)
	n, err := f.ReadAt(b, q.Offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return Result{}, err
	}
	return Result{Data: b[:n], Done: errors.Is(err, io.EOF) || n < ChunkSize}, nil
}

func (o *Origin) ServeHTTP(w http.ResponseWriter, r *http.Request) bool {
	const prefix = "/.mesh-app/origin/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		return false
	}
	token, err := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Mesh-App-Admission"))
	if err != nil {
		http.NotFound(w, r)
		return true
	}
	var proof Signed
	if err := decode(token, &proof); err != nil || proof.Verify("mesh-app/admission/v1", o.identity, o.config.EdgeIdentity, o.config.Now()) != nil {
		http.NotFound(w, r)
		return true
	}
	var admission admission
	if err := decode(proof.Body, &admission); err != nil {
		http.NotFound(w, r)
		return true
	}
	original := strings.TrimPrefix(r.URL.EscapedPath(), prefix+admission.ID)
	if original == "" {
		original = "/"
	}
	rawURI := original
	if r.URL.RawQuery != "" {
		rawURI += "?" + r.URL.RawQuery
	}
	if !ValidID(admission.ID) || admission.Method != r.Method || admission.URI != rawURI || !o.config.Now().Before(admission.Until) || admission.Host != admission.ID+"."+Domain {
		http.NotFound(w, r)
		return true
	}
	o.admissionMu.Lock()
	for id, until := range o.admissions {
		if !o.config.Now().Before(until) {
			delete(o.admissions, id)
		}
	}
	_, replayed := o.admissions[proof.ID]
	if len(o.admissions) >= 4096 {
		replayed = true
	}
	if !replayed {
		o.admissions[proof.ID] = admission.Until
	}
	o.admissionMu.Unlock()
	if replayed {
		http.NotFound(w, r)
		return true
	}
	o.mu.Lock()
	app, ok := o.state.Apps[admission.ID]
	o.mu.Unlock()
	if !ok || app.Phase != "ready" || app.Record.Generation != admission.Generation || !o.config.Now().Before(app.Record.LeaseUntil) {
		http.Error(w, "app unavailable", http.StatusServiceUnavailable)
		return true
	}
	if admission.Download {
		o.mu.Lock()
		defer o.mu.Unlock()
		if err := o.configCheckSourceDownload(w, r, app); err != nil {
			http.Error(w, "Download unavailable", http.StatusServiceUnavailable)
		}
		return true
	}
	request := r.Clone(r.Context())
	request.Header.Del("X-Mesh-App-Admission")
	decodedPath, pathErr := url.PathUnescape(original)
	if pathErr != nil {
		http.NotFound(w, r)
		return true
	}
	request.URL.Path = decodedPath
	request.URL.RawPath = original
	request.Host = admission.Host
	if app.Record.Kind == "static" {
		name := strings.TrimPrefix(request.URL.Path, "/")
		if excluded(name) || secretName(name) {
			http.NotFound(w, request)
			return true
		}
		handler, err := meshserve.Handler(meshserve.Service{Name: "temporary-app", Kind: meshserve.Static, Target: app.Root}, "/")
		if err != nil {
			http.Error(w, "app unavailable", http.StatusServiceUnavailable)
			return true
		}
		handler.ServeHTTP(w, request)
		return true
	}

	target := &url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", app.Port)}
	proxy := httputil.NewSingleHostReverseProxy(target)
	transport := &http.Transport{Proxy: nil, MaxResponseHeaderBytes: 1 << 20, ResponseHeaderTimeout: 10 * time.Second, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		connection, err := netDialer.DialContext(ctx, network, address)
		if err == nil {
			return connection, nil
		}
		return netDialer.DialContext(ctx, network, net.JoinHostPort("::1", fmt.Sprint(app.Port)))
	}}
	defer transport.CloseIdleConnections()
	proxy.Transport = transport
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, "app server unavailable", http.StatusServiceUnavailable)
	}
	proxy.ServeHTTP(w, request)
	return true
}

func (o *Origin) configCheckSourceDownload(w http.ResponseWriter, r *http.Request, app localApp) error {
	filename := filepath.Join(o.config.DataRoot, "apps", app.Record.ID, "browser-download.tar.gz")
	f, err := os.OpenFile(filename, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600) //nolint:gosec // Fixed owner-download archive inside the looked-up app workspace.
	if err != nil {
		return err
	}
	_, packErr := Pack(r.Context(), app.Root, f)
	if closeErr := f.Close(); packErr != nil || closeErr != nil {
		return errors.Join(packErr, closeErr)
	}
	defer func() { _ = os.Remove(filename) }()
	f, err = os.Open(filename) //nolint:gosec // Fixed owner-download archive inside the looked-up app workspace.
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Cache-Control", "no-store")
	_, err = io.Copy(w, f)
	return err
}

// GuardService holds the origin mutation lock through the ordinary service commit.
// Holding it prevents a checked service from racing with a new app reservation.
func (o *Origin) GuardService(ctx context.Context, ports []int, root string) (func(), error) {
	o.mu.Lock()
	if err := ctx.Err(); err != nil {
		o.mu.Unlock()
		return nil, err
	}
	managedRoot, err := canonicalAppRoot(o.config.DataRoot)
	if err != nil {
		o.mu.Unlock()
		return nil, err
	}
	if root != "" && (within(root, managedRoot) || within(managedRoot, root)) {
		o.mu.Unlock()
		return nil, errors.New("app: ordinary services cannot expose managed app directories")
	}
	for _, app := range o.state.Apps {
		if app.Command == "" {
			continue
		}
		for _, port := range ports {
			if app.Port == port {
				o.mu.Unlock()
				return nil, errors.New("app: ordinary services cannot proxy an app-owned port")
			}
		}
	}
	return o.mu.Unlock, nil
}
func within(parent, child string) bool {
	relative, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(child))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func canonicalAppRoot(path string) (string, error) {
	parent, err := existingDirectory(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(parent, path)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, relative), nil
}
