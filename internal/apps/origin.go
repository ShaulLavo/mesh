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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	Commit  *commitIntent `json:"commit,omitempty"`
	Record  Record        `json:"record"`
	Root    string        `json:"root"`
	Command string        `json:"command,omitempty"`
	Port    int           `json:"port,omitempty"`
	Env     []string      `json:"env,omitempty"`
	Session string        `json:"session,omitempty"`
	Phase   string        `json:"phase"`
}
type commitIntent struct {
	UploadID     string `json:"uploadId"`
	Digest       string `json:"digest"`
	PreviousRoot string `json:"previousRoot,omitempty"`
}
type appRoute struct {
	Record Record
	Root   string
	Port   int
	Phase  string
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

// appOp is the long operation that owns one app ("app <id>") or one upload
// ("upload <id>"). Operations on the same key wait for each other, and Sync
// leaves an owned app to its operation.
type appOp struct {
	name string
	done chan struct{}
}

// serviceHold keeps an ordinary service's ports from new app reservations while
// that service is being committed.
type serviceHold struct{ ports []int }

// maintenanceBudget bounds each step of Sync on its own: the lease exchange, then
// each app's safety work, then each app's recovery. It is the budget the whole
// pass used to share, so no step waits longer than before, and no app can spend
// another app's share.
const maintenanceBudget = 15 * time.Second

type Origin struct {
	// mu guards state, ops and holds. It is held only to read or commit state,
	// never across an edge exchange, a worker wait or unpacking, so one slow app
	// cannot stall lease renewal, service registration or other apps.
	mu    sync.Mutex
	state originState
	ops   map[string]*appOp
	holds map[*serviceHold]struct{}
	// exchangeMu serializes signed edge exchanges, because the edge accepts one
	// pending sequence per owner. Take it before mu, never after.
	exchangeMu  sync.Mutex
	routes      atomic.Pointer[map[string]appRoute]
	downloadMu  sync.Mutex
	config      OriginConfig
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
	o := &Origin{config: c, identity: base64.RawURLEncoding.EncodeToString(c.Key.Public().(ed25519.PublicKey)), state: originState{Receipts: map[string]createReceipt{}, Apps: map[string]localApp{}, Uploads: map[string]upload{}}, ops: map[string]*appOp{}, holds: map[*serviceHold]struct{}{}, admissions: map[string]time.Time{}}
	if err := load(ctx, c.Store, "apps.origin", &o.state); err != nil {
		return nil, err
	}
	o.publishRoutes()
	return o, nil
}

// persist requires mu.
func (o *Origin) persist(ctx context.Context) error {
	o.publishRoutes()
	return save(ctx, o.config.Store, "apps.origin", o.state)
}
func (o *Origin) publishRoutes() {
	routes := make(map[string]appRoute, len(o.state.Apps))
	for id, a := range o.state.Apps {
		routes[id] = appRoute{Record: a.Record, Root: a.Root, Port: a.Port, Phase: a.Phase}
	}
	o.routes.Store(&routes)
}

// modify edits the stored app in place. Writing back a whole copy read before a
// wait would undo the lease Sync renewed meanwhile.
func (o *Origin) modify(ctx context.Context, id string, change func(*localApp)) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	a, ok := o.state.Apps[id]
	if !ok {
		return fmt.Errorf("app %s: local record missing", id)
	}
	change(&a)
	o.state.Apps[id] = a
	return o.persist(ctx)
}

// claimLocked requires mu and an unowned key.
func (o *Origin) claimLocked(key, name string) func() {
	op := &appOp{name: name, done: make(chan struct{})}
	o.ops[key] = op
	return func() {
		o.mu.Lock()
		delete(o.ops, key)
		o.mu.Unlock()
		close(op.done)
	}
}
func (o *Origin) beginOp(ctx context.Context, key, name string) (func(), error) {
	for {
		o.mu.Lock()
		current := o.ops[key]
		if current == nil {
			release := o.claimLocked(key, name)
			o.mu.Unlock()
			return release, nil
		}
		o.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%s: wait for %s: %w", key, current.name, ctx.Err())
		case <-current.done:
		}
	}
}
func (o *Origin) tryOp(key, name string) (func(), bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.ops[key] != nil {
		return nil, false
	}
	return o.claimLocked(key, name), true
}

func (o *Origin) edge(ctx context.Context, q Request) (Result, error) {
	o.exchangeMu.Lock()
	defer o.exchangeMu.Unlock()
	return o.edgeLocked(ctx, q)
}

// edgeLocked requires exchangeMu, which makes its holder the only writer of
// Pending and Sequence; mu covers only the writes and their persistence.
func (o *Origin) edgeLocked(ctx context.Context, q Request) (Result, error) {
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
	o.mu.Lock()
	o.state.Sequence++
	s, err := Sign("mesh-app/request/v1", o.config.EdgeIdentity, o.state.Sequence, q, o.config.Key, o.config.Now())
	if err == nil {
		o.state.Pending = &s
		err = o.persist(ctx)
	}
	o.mu.Unlock()
	if err != nil {
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
	o.mu.Lock()
	o.state.Pending = nil
	err = o.persist(ctx)
	o.mu.Unlock()
	if err != nil {
		return Result{}, err
	}
	if reply.Error != "" {
		return Result{}, errors.New(reply.Error)
	}
	return reply.Result, nil
}
func (o *Origin) Handle(ctx context.Context, q Request) (Result, error) {
	switch q.Action {
	case "upload.begin":
		o.mu.Lock()
		defer o.mu.Unlock()
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
		o.mu.Lock()
		a, ok := o.state.Apps[q.ID]
		o.mu.Unlock()
		if ok {
			result.Runtime = &RuntimeInfo{Phase: a.Phase, SessionID: a.Session, Command: a.Command, Port: a.Port, Root: a.Root}
		}
		return result, nil
	case "delete":
		release, err := o.beginOp(ctx, "app "+q.ID, "delete")
		if err != nil {
			return Result{}, err
		}
		defer release()
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

// beginUpload requires mu; it only creates an empty file.
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
	release, err := o.beginOp(ctx, "upload "+q.UploadID, "upload.chunk")
	if err != nil {
		return Result{}, err
	}
	defer release()
	o.mu.Lock()
	u, ok := o.state.Uploads[q.UploadID]
	o.mu.Unlock()
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
	o.mu.Lock()
	defer o.mu.Unlock()
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

// checkPortLocked requires mu. Callers repeat it when the port lands in state,
// because services and other apps may claim ports while a recipe prepares.
func (o *Origin) checkPortLocked(id string, q Request) error {
	if o.config.CheckHosting != nil {
		if err := o.config.CheckHosting(q.Port, o.config.DataRoot); err != nil {
			return err
		}
	}
	if q.Kind != "server" {
		return nil
	}
	for other, existing := range o.state.Apps {
		if other != id && existing.Record.Kind == "server" && existing.Port == q.Port {
			return errors.New("app: port already belongs to another app")
		}
	}
	for hold := range o.holds {
		if slices.Contains(hold.ports, q.Port) {
			return errors.New("app: port is being claimed by an ordinary service")
		}
	}
	return nil
}
func (o *Origin) pendingCommitLocked(uploadID string) (localApp, bool) {
	for _, a := range o.state.Apps {
		if a.Commit != nil && a.Commit.UploadID == uploadID {
			return a, true
		}
	}
	return localApp{}, false
}

// allocate reserves a new app at the edge and records it locally, owned by the
// caller, before any other exchange can run. Otherwise a Sync between the two
// would find the edge record without a local app and delete it as abandoned.
func (o *Origin) allocate(ctx context.Context, q Request, digest string) (localApp, func(), error) {
	o.exchangeMu.Lock()
	defer o.exchangeMu.Unlock()
	result, err := o.edgeLocked(ctx, Request{Action: "allocate", Kind: q.Kind})
	if err != nil {
		return localApp{}, nil, err
	}
	id := result.App.ID
	local := localApp{Record: *result.App, Root: filepath.Join(o.config.DataRoot, "apps", id, "source-"+q.UploadID), Command: q.Command, Port: q.Port, Env: q.Env, Phase: "preparing", Commit: &commitIntent{UploadID: q.UploadID, Digest: digest}}
	o.mu.Lock()
	defer o.mu.Unlock()
	release := o.claimLocked("app "+id, q.Action)
	o.state.Apps[id] = local
	return local, release, errors.Join(o.checkPortLocked(id, q), o.persist(ctx))
}
func (o *Origin) create(ctx context.Context, q Request) (Result, error) {
	if q.Kind == "" {
		q.Kind = "static"
	}
	normalized, _ := json.Marshal(q)
	digest := digestBytes(normalized)
	// A retry of the same upload waits for the attempt in progress rather than
	// unpacking or allocating beside it.
	releaseUpload, err := o.beginOp(ctx, "upload "+q.UploadID, q.Action)
	if err != nil {
		return Result{}, err
	}
	defer releaseUpload()
	o.mu.Lock()
	target := q.ID
	if a, ok := o.pendingCommitLocked(q.UploadID); ok {
		target = a.Record.ID
	}
	o.mu.Unlock()
	if target != "" {
		releaseApp, err := o.beginOp(ctx, "app "+target, q.Action)
		if err != nil {
			return Result{}, err
		}
		defer releaseApp()
	}
	o.mu.Lock()
	prior, receipted := o.state.Receipts[q.UploadID]
	pending, hasPending := o.pendingCommitLocked(q.UploadID)
	current, exists := o.state.Apps[q.ID]
	o.mu.Unlock()
	if receipted {
		if prior.Digest != digest {
			return Result{}, errors.New("app: commit retry conflicts with previous recipe")
		}
		return o.edge(ctx, Request{Action: "inspect", ID: prior.Record.ID})
	}
	if hasPending {
		if pending.Commit.Digest != digest {
			return Result{}, errors.New("app: commit retry conflicts with pending recipe")
		}
		if pending.Phase != "activating" {
			return Result{}, errors.New("app: source preparation requires reconciliation")
		}
		return o.activate(ctx, pending)
	}
	if q.Action == "update" && exists && current.Phase == "ready" && current.Commit != nil {
		if err := o.finishActivation(ctx, current, current.Record); err != nil {
			return Result{}, err
		}
	}
	if err := validateRecipe(q); err != nil {
		return Result{}, err
	}
	o.mu.Lock()
	err = o.checkPortLocked(q.ID, q)
	u, ok := o.state.Uploads[q.UploadID]
	o.mu.Unlock()
	if err != nil {
		return Result{}, err
	}
	if !ok || !o.config.Now().Before(u.ExpiresAt) {
		return Result{}, errors.New("app: missing source upload")
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
	var local, previous localApp
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
		o.mu.Lock()
		previous, ok = o.state.Apps[q.ID]
		o.mu.Unlock()
		if !ok {
			return Result{}, errors.New("app: managed workspace missing")
		}
		local = localApp{Record: app, Root: filepath.Join(o.config.DataRoot, "apps", app.ID, "source-"+q.UploadID), Command: q.Command, Port: q.Port, Env: q.Env, Phase: "preparing"}
		local.Commit = &commitIntent{UploadID: q.UploadID, Digest: digest, PreviousRoot: previous.Root}
	} else {
		allocated, releaseApp, err := o.allocate(ctx, q, digest)
		if releaseApp == nil {
			_ = os.RemoveAll(staging)
			return Result{}, err
		}
		defer releaseApp()
		local, app = allocated, allocated.Record
		if err != nil {
			_ = os.RemoveAll(staging)
			return Result{}, o.failCreate(ctx, app.ID, err)
		}
	}
	workspace := local.Root
	fail := func(cause error) (Result, error) {
		if q.Action == "create" {
			return Result{}, o.failCreate(ctx, app.ID, cause)
		}
		_ = o.stop(ctx, app.ID)
		if previous.Record.Kind == "server" {
			session, startErr := o.config.Workers.Start(ctx, "app "+app.ID, previous.Command, previous.Root, o.environment(previous))
			previous.Session = session
			cause = errors.Join(cause, startErr)
		}
		_ = os.RemoveAll(workspace)
		o.mu.Lock()
		defer o.mu.Unlock()
		// Keep the lease and expiry Sync applied while the update ran.
		if latest, ok := o.state.Apps[app.ID]; ok {
			revision := previous.Record.Revision
			previous.Record = latest.Record
			previous.Record.Revision = revision
		}
		o.state.Apps[app.ID] = previous
		return Result{}, errors.Join(cause, o.persist(ctx))
	}
	err = os.MkdirAll(filepath.Dir(workspace), 0700)
	if err == nil {
		err = os.Rename(staging, workspace)
	}
	if err != nil && q.Action == "create" {
		_ = os.RemoveAll(staging)
		return Result{}, o.failCreate(ctx, app.ID, err)
	}
	if err != nil {
		return Result{}, err
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
	o.mu.Lock()
	reserveErr := o.checkPortLocked(app.ID, q)
	if reserveErr == nil {
		o.state.Apps[app.ID] = local
		err = o.persist(ctx)
	}
	o.mu.Unlock()
	if reserveErr != nil {
		return fail(reserveErr)
	}
	if err != nil {
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
		if err := o.modify(ctx, app.ID, func(a *localApp) { a.Session = session }); err != nil {
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
	return result, o.finishActivation(ctx, local, *result.App)
}
func (o *Origin) finishActivation(ctx context.Context, a localApp, record Record) error {
	if a.Commit == nil {
		return errors.New("app: activation commit missing")
	}
	commit := a.Commit
	a.Record = record
	a.Phase = "ready"
	o.mu.Lock()
	o.state.Apps[record.ID] = a
	if o.state.Receipts == nil {
		o.state.Receipts = map[string]createReceipt{}
	}
	for token, receipt := range o.state.Receipts {
		if receipt.Record.ID == record.ID {
			delete(o.state.Receipts, token)
		}
	}
	o.state.Receipts[commit.UploadID] = createReceipt{Record: record, Digest: commit.Digest}
	delete(o.state.Uploads, commit.UploadID)
	err := o.persist(ctx)
	o.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.Remove(o.uploadPath(commit.UploadID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if commit.PreviousRoot != "" && commit.PreviousRoot != a.Root {
		if err := os.RemoveAll(commit.PreviousRoot); err != nil {
			return err
		}
	}
	return o.modify(ctx, record.ID, func(a *localApp) { a.Commit = nil })
}
func (o *Origin) activate(ctx context.Context, a localApp) (Result, error) {
	if a.Commit == nil {
		return Result{}, errors.New("app: activation commit missing")
	}
	if a.Command != "" {
		if err := o.ensureServer(ctx, &a); err != nil {
			return Result{}, err
		}
	}
	kind := "static"
	if a.Command != "" {
		kind = "server"
	}
	result, err := o.edge(ctx, Request{Action: "activate", ID: a.Record.ID, Kind: kind, UploadID: a.Commit.UploadID})
	if err != nil {
		return Result{}, err
	}
	return result, o.finishActivation(ctx, a, *result.App)
}
func (o *Origin) ensureServer(ctx context.Context, a *localApp) error {
	session, alive, err := o.config.Workers.Find(ctx, "app "+a.Record.ID)
	if err != nil {
		return err
	}
	if !alive {
		if err := ProbePortAvailable(a.Port); err != nil {
			return err
		}
		session, err = o.config.Workers.Start(ctx, "app "+a.Record.ID, a.Command, a.Root, o.environment(*a))
		if err != nil {
			return err
		}
	}
	a.Session = session
	if err := o.modify(ctx, a.Record.ID, func(stored *localApp) { stored.Session = session }); err != nil {
		return err
	}
	if err := waitPort(ctx, a.Port); err != nil {
		_ = o.stop(ctx, a.Record.ID)
		return err
	}
	if err := checkServerListener(ctx, a.Port); err != nil {
		_ = o.stop(ctx, a.Record.ID)
		return err
	}
	return nil
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

// stop ends only the app's own labelled workers; ordinary sessions never carry
// these labels.
func (o *Origin) stop(ctx context.Context, id string) error {
	o.mu.Lock()
	_, ok := o.state.Apps[id]
	o.mu.Unlock()
	if !ok {
		return nil
	}
	for _, label := range []string{"app " + id, "app-setup " + id} {
		session, alive, err := o.config.Workers.Find(ctx, label)
		if err != nil {
			return fmt.Errorf("find %s worker: %w", label, err)
		}
		if alive {
			if err := o.config.Workers.Stop(ctx, session); err != nil {
				return fmt.Errorf("stop %s worker %s: %w", label, session, err)
			}
		}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if a, ok := o.state.Apps[id]; ok {
		a.Session = ""
		o.state.Apps[id] = a
	}
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
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.state.Apps, id)
	for token, receipt := range o.state.Receipts {
		if receipt.Record.ID == id {
			delete(o.state.Receipts, token)
		}
	}
	return o.persist(ctx)
}

// Sync renews every app's lease from the edge, then reconciles each app on its
// own: safety work first, recovery second. A failing or slow app is reported
// with the others instead of holding them back.
func (o *Origin) Sync(ctx context.Context) error {
	records, err := o.renewLeases(ctx)
	// Safety work must run even when the caller's budget is spent, because that
	// is exactly when a silent edge lets leases lapse.
	safety := context.WithoutCancel(ctx)
	if err != nil {
		return errors.Join(err, o.eachApp(safety, o.appIDs(nil), o.stopLapsed))
	}
	ids := o.appIDs(records)
	denied := o.eachApp(safety, ids, func(ctx context.Context, id string) error { return o.deny(ctx, id, records) })
	recovered := o.eachApp(ctx, ids, func(ctx context.Context, id string) error { return o.recover(ctx, id, records) })
	return errors.Join(denied, recovered, o.expireUploads(ctx))
}

// renewLeases applies the edge's records while still holding exchangeMu, so no
// allocation or activation can land between the edge's answer and its use.
func (o *Origin) renewLeases(ctx context.Context) (map[string]Record, error) {
	ctx, cancel := context.WithTimeout(ctx, maintenanceBudget)
	defer cancel()
	o.exchangeMu.Lock()
	defer o.exchangeMu.Unlock()
	result, err := o.edgeLocked(ctx, Request{Action: "sync"})
	if err != nil {
		return nil, fmt.Errorf("app: sync leases with edge: %w", err)
	}
	records := make(map[string]Record, len(result.Apps))
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, record := range result.Apps {
		records[record.ID] = record
		if a, ok := o.state.Apps[record.ID]; ok {
			a.Record = record
			o.state.Apps[record.ID] = a
		}
	}
	return records, o.persist(ctx)
}
func (o *Origin) appIDs(records map[string]Record) []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	ids := make([]string, 0, len(o.state.Apps)+len(records))
	for id := range o.state.Apps {
		ids = append(ids, id)
	}
	for id := range records {
		if _, ok := o.state.Apps[id]; !ok {
			ids = append(ids, id)
		}
	}
	return ids
}

// eachApp runs step for every app no operation owns, concurrently and each
// under its own budget.
func (o *Origin) eachApp(ctx context.Context, ids []string, step func(context.Context, string) error) error {
	var wg sync.WaitGroup
	errs := make([]error, len(ids))
	for i, id := range ids {
		release, ok := o.tryOp("app "+id, "sync")
		if !ok {
			continue
		}
		wg.Go(func() {
			defer release()
			stepCtx, cancel := context.WithTimeout(ctx, maintenanceBudget)
			defer cancel()
			errs[i] = step(stepCtx, id)
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

// stopLapsed stops an app whose last lease has run out; its files stay until
// the edge says otherwise.
func (o *Origin) stopLapsed(ctx context.Context, id string) error {
	o.mu.Lock()
	a, ok := o.state.Apps[id]
	o.mu.Unlock()
	if !ok || o.config.Now().Before(a.Record.LeaseUntil) {
		return nil
	}
	if err := o.stop(ctx, id); err != nil {
		return fmt.Errorf("app %s: stop after lease lapsed: %w", id, err)
	}
	return nil
}

// deny carries out what the edge's answer forbids: serving a deleted or expired
// app, keeping an app the origin cannot serve, or running an app the edge no
// longer knows.
func (o *Origin) deny(ctx context.Context, id string, records map[string]Record) error {
	record, listed := records[id]
	o.mu.Lock()
	a, local := o.state.Apps[id]
	o.mu.Unlock()
	switch {
	case listed && record.Status != "active":
		if err := o.cleanup(ctx, id); err != nil {
			return fmt.Errorf("app %s: clean up %s app: %w", id, record.Status, err)
		}
		if _, err := o.edge(ctx, Request{Action: "cleanup", ID: id}); err != nil {
			return fmt.Errorf("app %s: confirm cleanup: %w", id, err)
		}
	case listed && !local:
		// Nothing here can serve it, so traffic would only extend its deadline.
		if _, err := o.edge(ctx, Request{Action: "delete", ID: id}); err != nil {
			return fmt.Errorf("app %s: delete app missing on origin: %w", id, err)
		}
	case listed && a.Phase != "ready" && a.Phase != "activating":
		if _, err := o.edge(ctx, Request{Action: "delete", ID: id}); err != nil {
			return fmt.Errorf("app %s: delete unfinished app: %w", id, err)
		}
	case !listed && local && !o.config.Now().Before(a.Record.LeaseUntil.Add(IdleTTL)):
		// An edge that lost the app cannot route to it or renew it. The files
		// wait one idle period past the last lease, as long as the edge keeps an
		// app without traffic, so a restored edge can list it again first.
		if err := o.cleanup(ctx, id); err != nil {
			return fmt.Errorf("app %s: remove app the edge no longer lists: %w", id, err)
		}
	case !listed && local:
		return o.stopLapsed(ctx, id)
	}
	return nil
}

// recover restores what an interrupted operation or a lost worker left behind
// for an app the edge still lists as active.
func (o *Origin) recover(ctx context.Context, id string, records map[string]Record) error {
	record, listed := records[id]
	o.mu.Lock()
	a, local := o.state.Apps[id]
	o.mu.Unlock()
	if !listed || !local || record.Status != "active" {
		return nil
	}
	if a.Phase == "activating" {
		if _, err := o.activate(ctx, a); err != nil {
			return fmt.Errorf("app %s: resume activation: %w", id, err)
		}
		return nil
	}
	if a.Phase != "ready" {
		return nil
	}
	if a.Commit != nil {
		if err := o.finishActivation(ctx, a, record); err != nil {
			return fmt.Errorf("app %s: finish activation: %w", id, err)
		}
	}
	if record.Kind != "server" {
		return nil
	}
	_, alive, err := o.config.Workers.Find(ctx, "app "+id)
	if err != nil {
		return fmt.Errorf("app %s: find server: %w", id, err)
	}
	if alive {
		return nil
	}
	o.mu.Lock()
	a = o.state.Apps[id]
	o.mu.Unlock()
	if err := o.ensureServer(ctx, &a); err != nil {
		return fmt.Errorf("app %s: restart server: %w", id, err)
	}
	return nil
}
func (o *Origin) expireUploads(ctx context.Context) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	for id, u := range o.state.Uploads {
		if o.ops["upload "+id] == nil && !o.config.Now().Before(u.ExpiresAt) {
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
	release, err := o.beginOp(ctx, "app "+q.ID, "download")
	if err != nil {
		return Result{}, err
	}
	defer release()
	o.mu.Lock()
	a, ok := o.state.Apps[q.ID]
	o.mu.Unlock()
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
	app, ok := (*o.routes.Load())[admission.ID]
	if !ok || app.Phase != "ready" || app.Record.Generation != admission.Generation || !o.config.Now().Before(app.Record.LeaseUntil) {
		http.Error(w, "app unavailable", http.StatusServiceUnavailable)
		return true
	}
	if admission.Download {
		o.downloadMu.Lock()
		defer o.downloadMu.Unlock()
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

func (o *Origin) configCheckSourceDownload(w http.ResponseWriter, r *http.Request, app appRoute) error {
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

// GuardService checks an ordinary service against app-owned ports and roots,
// then holds its ports until release, so no app reserves one before the service
// reaches the registry. It never waits for an app operation.
func (o *Origin) GuardService(ctx context.Context, ports []int, root string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	managedRoot, err := canonicalAppRoot(o.config.DataRoot)
	if err != nil {
		return nil, err
	}
	if root != "" && (within(root, managedRoot) || within(managedRoot, root)) {
		return nil, errors.New("app: ordinary services cannot expose managed app directories")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, app := range o.state.Apps {
		if app.Command != "" && slices.Contains(ports, app.Port) {
			return nil, errors.New("app: ordinary services cannot proxy an app-owned port")
		}
	}
	hold := &serviceHold{ports: ports}
	o.holds[hold] = struct{}{}
	return func() {
		o.mu.Lock()
		defer o.mu.Unlock()
		delete(o.holds, hold)
	}, nil
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
