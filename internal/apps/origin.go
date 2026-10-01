package apps

import (
	"container/heap"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	meshserve "github.com/shaul/mesh/internal/serve"
	"io"
	"net/http"
	"net/http/httputil"
	"net/netip"
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
	// Processes names the live processes of a worker's session, so the origin
	// can tell an app's own listeners from another program's.
	Processes(context.Context, string) ([]int, error)
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
	// Upstream is the one address a server app may be proxied to; invalid
	// means it is not served.
	Upstream netip.AddrPort
}

// serving is what the last listener check found for a server app: the address
// the proxy may dial, or the fault that withdrew it. An app with no entry has
// not passed a check since its worker started, and is not served.
type serving struct {
	upstream netip.AddrPort
	fault    string
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
// ("upload <id>"). Operations on the same key wait for each other. The edge's
// answer outranks an operation: Sync revokes it, which ends its context and
// forbids it from starting the app again, then does its safety work.
type appOp struct {
	name   string
	done   chan struct{}
	cancel context.CancelFunc
	// revoked names why the edge withdrew the app; set under mu.
	revoked string
	// ports are what the operation may bind or roll back onto, reserved until
	// it ends so no service or other app takes them meanwhile.
	ports []int
}

// yieldBudget is how long safety work waits for a revoked operation. The
// operation only has to notice its cancelled context and unwind.
const yieldBudget = 5 * time.Second

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
	// exchange is a one-slot lock on signed edge exchanges, because the edge
	// accepts one pending sequence per owner. Waiting for it honours the
	// caller's context. Take it before mu, never after.
	exchange chan struct{}
	routes   atomic.Pointer[map[string]appRoute]
	// serving is guarded by mu and published with the routes.
	serving     map[string]serving
	downloadMu  sync.Mutex
	config      OriginConfig
	identity    string
	admissionMu sync.Mutex
	admissionAt time.Time
	admissions  map[string]*admissionCache
}

func NewOrigin(ctx context.Context, c OriginConfig) (*Origin, error) {
	if c.Store == nil || len(c.Key) != ed25519.PrivateKeySize || c.Exchange == nil || c.Workers == nil || c.DataRoot == "" {
		return nil, errors.New("app: missing origin dependencies")
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	o := &Origin{config: c, identity: base64.RawURLEncoding.EncodeToString(c.Key.Public().(ed25519.PublicKey)), state: originState{Receipts: map[string]createReceipt{}, Apps: map[string]localApp{}, Uploads: map[string]upload{}}, ops: map[string]*appOp{}, serving: map[string]serving{}, exchange: make(chan struct{}, 1), holds: map[*serviceHold]struct{}{}, admissions: map[string]*admissionCache{}}
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
		routes[id] = appRoute{Record: a.Record, Root: a.Root, Port: a.Port, Phase: a.Phase, Upstream: o.serving[id].upstream}
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

// claimLocked requires mu and an unowned key. The returned context ends when
// the operation is released or revoked.
func (o *Origin) claimLocked(ctx context.Context, key, name string) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	op := &appOp{name: name, done: make(chan struct{}), cancel: cancel}
	o.ops[key] = op
	return ctx, func() {
		cancel()
		o.mu.Lock()
		delete(o.ops, key)
		o.mu.Unlock()
		close(op.done)
	}
}
func (o *Origin) beginOp(ctx context.Context, key, name string) (context.Context, func(), error) {
	for {
		o.mu.Lock()
		current := o.ops[key]
		if current == nil {
			opCtx, release := o.claimLocked(ctx, key, name)
			o.mu.Unlock()
			return opCtx, release, nil
		}
		o.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, nil, fmt.Errorf("%s: wait for %s: %w", key, current.name, ctx.Err())
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
	_, release := o.claimLocked(context.Background(), key, name)
	return release, true
}

// preempt claims an app for safety work, revoking any operation that owns it
// except a delete, and waiting up to yieldBudget for it to unwind. It fails
// only when the operation does not yield; the caller may then stop workers but
// not change the app's state.
func (o *Origin) preempt(ctx context.Context, id, reason string) (func(), error) {
	wait, cancel := context.WithTimeout(ctx, yieldBudget)
	defer cancel()
	for {
		if release, ok := o.tryOp("app "+id, "sync"); ok {
			return release, nil
		}
		o.mu.Lock()
		op := o.ops["app "+id]
		// A delete is already doing the safety work; revoking it would only fail
		// the owner's request.
		if op != nil && op.revoked == "" && op.name != "delete" {
			op.revoked = reason
			op.cancel()
		}
		o.mu.Unlock()
		if op == nil {
			continue
		}
		select {
		case <-op.done:
		case <-wait.Done():
			return nil, fmt.Errorf("app %s: %s did not yield after the app was %s: %w", id, op.name, reason, wait.Err())
		}
	}
}

// startRefusal names why an app may not run now: its operation was revoked, or
// its stored lease has elapsed.
func (o *Origin) startRefusal(id string) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if op := o.ops["app "+id]; op != nil && op.revoked != "" {
		return op.revoked
	}
	if a, ok := o.state.Apps[id]; ok && !o.config.Now().Before(a.Record.LeaseUntil) {
		return "left without a lease"
	}
	return ""
}

// start launches one of an app's workers unless the edge has withdrawn the app,
// and stops it again if that happens while it launches. Every worker an
// operation starts goes through here, so a revoked operation cannot restart the
// app behind the safety stop.
func (o *Origin) start(ctx context.Context, id, label, command, root string, env []string) (string, error) {
	if reason := o.startRefusal(id); reason != "" {
		return "", fmt.Errorf("app %s: not starting %s: app was %s", id, label, reason)
	}
	session, err := o.config.Workers.Start(ctx, label, command, root, env)
	if err != nil {
		return "", fmt.Errorf("app %s: start %s: %w", id, label, err)
	}
	if reason := o.startRefusal(id); reason != "" {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), maintenanceBudget)
		defer cancel()
		return "", errors.Join(fmt.Errorf("app %s: %s started after the app was %s", id, label, reason), o.config.Workers.Stop(stopCtx, session))
	}
	return session, nil
}

func (o *Origin) lockExchange(ctx context.Context) error {
	select {
	case o.exchange <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("app: wait for edge exchange: %w", ctx.Err())
	}
}
func (o *Origin) unlockExchange() { <-o.exchange }

func (o *Origin) edge(ctx context.Context, q Request) (Result, error) {
	if err := o.lockExchange(ctx); err != nil {
		return Result{}, err
	}
	defer o.unlockExchange()
	return o.edgeLocked(ctx, q)
}

// edgeLocked requires the exchange slot, which makes its holder the only writer of
// Pending and Sequence; mu covers only the writes and their persistence.
func (o *Origin) edgeLocked(ctx context.Context, q Request) (Result, error) {
	if o.state.Pending != nil {
		pending := *o.state.Pending
		result, err := o.settle(ctx, pending)
		expected, _ := json.Marshal(q)
		if digestBytes(expected) == digestBytes(pending.Body) {
			return result, err
		}
		if err != nil {
			return Result{}, err
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
	saveErr := o.persist(ctx)
	o.mu.Unlock()
	if reply.Error != "" {
		return Result{}, errors.Join(errors.New(reply.Error), saveErr)
	}
	if saveErr != nil {
		return reply.Result, &unsavedReply{err: saveErr}
	}
	return reply.Result, nil
}

// unsavedReply accompanies a verified edge result whose acknowledgement could
// not be saved. The result is still authoritative; the edge answers a resend of
// the same pending request identically.
type unsavedReply struct{ err error }

func (e *unsavedReply) Error() string { return "app: save edge acknowledgement: " + e.err.Error() }
func (e *unsavedReply) Unwrap() error { return e.err }
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
	case "list", "public", "private", "renew", "browser.inspect", "browser.approve", "browser.list", "browser.revoke":
		return o.edge(ctx, q)
	case "inspect":
		result, err := o.edge(ctx, q)
		if err != nil {
			return result, err
		}
		o.mu.Lock()
		a, ok := o.state.Apps[q.ID]
		problem := o.serving[q.ID].fault
		o.mu.Unlock()
		if ok {
			result.Runtime = &RuntimeInfo{Phase: a.Phase, SessionID: a.Session, Command: a.Command, Port: a.Port, Root: a.Root, Problem: problem}
		}
		return result, nil
	case "delete":
		ctx, release, err := o.beginOp(ctx, "app "+q.ID, "delete")
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
	_, release, err := o.beginOp(ctx, "upload "+q.UploadID, "upload.chunk")
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
	return Result{UploadID: u.ID}, o.recordChunk(ctx, u)
}

// recordChunk saves a writer's copy of its upload after the chunk is on disk.
// Activation retires an upload while holding only its app, so the record may be
// gone; writing the cached copy back would revive it without its file.
func (o *Origin) recordChunk(ctx context.Context, u upload) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, ok := o.state.Uploads[u.ID]; !ok {
		return fmt.Errorf("app: upload %s was consumed or expired while writing", u.ID)
	}
	o.state.Uploads[u.ID] = u
	return o.persist(ctx)
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

// checkPortLocked requires mu. Ports belong to the local runtime (a stored app
// with a command) or to an operation's reservation, never to the edge's view of
// the app's kind, which lags a static-to-server update. Callers repeat the check
// when the port lands in state, because services and other apps may claim ports
// while a recipe prepares.
func (o *Origin) checkPortLocked(id string, q Request) error {
	if o.config.CheckHosting != nil {
		if err := o.config.CheckHosting(q.Port, o.config.DataRoot); err != nil {
			return err
		}
	}
	if q.Kind != "server" {
		return nil
	}
	if owners := o.portOwnersLocked(q.Port, id); len(owners) != 0 {
		return fmt.Errorf("app: port %d already belongs to app %s", q.Port, owners[0])
	}
	for hold := range o.holds {
		if slices.Contains(hold.ports, q.Port) {
			return errors.New("app: port is being claimed by an ordinary service")
		}
	}
	return nil
}

// portOwnersLocked requires mu. It names, in order, every app other than self
// that runs port (a stored app with a command) or reserves it (an operation's
// ports), so no single match can hide another.
func (o *Origin) portOwnersLocked(port int, self string) []string {
	var owners []string
	for id, a := range o.state.Apps {
		if id != self && a.Command != "" && a.Port == port {
			owners = append(owners, id)
		}
	}
	for key, op := range o.ops {
		if id, ok := strings.CutPrefix(key, "app "); ok && id != self && slices.Contains(op.ports, port) {
			owners = append(owners, id)
		}
	}
	slices.Sort(owners)
	return slices.Compact(owners)
}

// admitRecipe checks the recipe's port and source upload. An update, which
// already owns its app, also reserves the candidate port and the running one
// until it ends, because until then it may still roll back onto the old port.
func (o *Origin) admitRecipe(q Request) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.checkPortLocked(q.ID, q); err != nil {
		return err
	}
	if u, ok := o.state.Uploads[q.UploadID]; !ok || !o.config.Now().Before(u.ExpiresAt) {
		return errors.New("app: missing source upload")
	}
	if q.Action != "update" {
		return nil
	}
	op, owned := o.ops["app "+q.ID]
	if !owned {
		return fmt.Errorf("app %s: update does not own the app", q.ID)
	}
	if q.Kind == "server" {
		op.ports = append(op.ports, q.Port)
	}
	if current, ok := o.state.Apps[q.ID]; ok && current.Command != "" {
		op.ports = append(op.ports, current.Port)
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
func (o *Origin) allocate(ctx context.Context, q Request, digest string) (context.Context, localApp, func(), error) {
	if err := o.lockExchange(ctx); err != nil {
		return ctx, localApp{}, nil, err
	}
	defer o.unlockExchange()
	result, err := o.edgeLocked(ctx, Request{Action: "allocate", Kind: q.Kind})
	if err != nil {
		return ctx, localApp{}, nil, err
	}
	id := result.App.ID
	local := localApp{Record: *result.App, Root: filepath.Join(o.config.DataRoot, "apps", id, "source-"+q.UploadID), Command: q.Command, Port: q.Port, Env: q.Env, Phase: "preparing", Commit: &commitIntent{UploadID: q.UploadID, Digest: digest}}
	o.mu.Lock()
	defer o.mu.Unlock()
	opCtx, release := o.claimLocked(ctx, "app "+id, q.Action)
	// Check before the record exists, so the create's own port cannot stand in
	// for an owner that reserved it meanwhile.
	if err := o.checkPortLocked(id, q); err != nil {
		return opCtx, local, release, err
	}
	o.state.Apps[id] = local
	return opCtx, local, release, o.persist(ctx)
}

// stageUpdate describes the workspace an update of a still-active app will use,
// beside the app it replaces.
func (o *Origin) stageUpdate(ctx context.Context, q Request, digest string) (localApp, localApp, error) {
	result, err := o.edge(ctx, Request{Action: "inspect", ID: q.ID})
	if err != nil {
		return localApp{}, localApp{}, err
	}
	if result.App.Status != "active" {
		return localApp{}, localApp{}, errors.New("app: cannot update expired app")
	}
	o.mu.Lock()
	previous, ok := o.state.Apps[q.ID]
	o.mu.Unlock()
	if !ok {
		return localApp{}, localApp{}, errors.New("app: managed workspace missing")
	}
	local := localApp{Record: *result.App, Root: filepath.Join(o.config.DataRoot, "apps", q.ID, "source-"+q.UploadID), Command: q.Command, Port: q.Port, Env: q.Env, Phase: "preparing"}
	local.Commit = &commitIntent{UploadID: q.UploadID, Digest: digest, PreviousRoot: previous.Root}
	return local, previous, nil
}

// stageCreate allocates the new app and always returns a release for it. An
// allocation that could not be recorded is undone at the edge.
func (o *Origin) stageCreate(ctx context.Context, q Request, digest string) (context.Context, localApp, func(), error) {
	opCtx, local, release, err := o.allocate(ctx, q, digest)
	if release == nil {
		return ctx, localApp{}, func() {}, err
	}
	if err != nil {
		err = o.failCreate(opCtx, local.Record.ID, err)
	}
	return opCtx, local, release, err
}
func (o *Origin) create(ctx context.Context, q Request) (Result, error) {
	if q.Kind == "" {
		q.Kind = "static"
	}
	if q.Action == "update" && !ValidID(q.ID) {
		return Result{}, fmt.Errorf("app: update needs a valid app id, not %q", q.ID)
	}
	normalized, _ := json.Marshal(q)
	digest := digestBytes(normalized)
	// A retry of the same upload waits for the attempt in progress rather than
	// unpacking or allocating beside it.
	ctx, releaseUpload, err := o.beginOp(ctx, "upload "+q.UploadID, q.Action)
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
		var releaseApp func()
		ctx, releaseApp, err = o.beginOp(ctx, "app "+target, q.Action)
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
	if err := o.admitRecipe(q); err != nil {
		return Result{}, err
	}
	staging := filepath.Join(o.config.DataRoot, "uploads", "source-"+q.UploadID)
	if err := os.MkdirAll(staging, 0700); err != nil {
		return Result{}, err
	}
	if err := unpack(o.uploadPath(q.UploadID), staging, q.Digest); err != nil {
		_ = os.RemoveAll(staging)
		return Result{}, err
	}
	var local, previous localApp
	releaseApp := func() {}
	if q.Action == "update" {
		local, previous, err = o.stageUpdate(ctx, q, digest)
	} else {
		ctx, local, releaseApp, err = o.stageCreate(ctx, q, digest)
	}
	defer releaseApp()
	if err != nil {
		_ = os.RemoveAll(staging)
		return Result{}, err
	}
	app, workspace := local.Record, local.Root
	fail := func(cause error) (Result, error) {
		// Undoing must finish even when the request or the operation's context
		// has ended; start still refuses to restart a revoked app.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), maintenanceBudget)
		defer cancel()
		if q.Action == "create" {
			return Result{}, o.failCreate(ctx, app.ID, cause)
		}
		_ = o.stop(ctx, app.ID)
		if previous.Command != "" {
			session, startErr := o.start(ctx, app.ID, "app "+app.ID, previous.Command, previous.Root, o.environment(previous))
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
		id, err := o.start(ctx, app.ID, "app-setup "+app.ID, q.Setup, workspace, o.environment(local))
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
		session, err := o.start(ctx, app.ID, "app "+app.ID, q.Command, workspace, o.environment(local))
		if err != nil {
			return fail(err)
		}
		local.Session = session
		if err := o.modify(ctx, app.ID, func(a *localApp) { a.Session = session }); err != nil {
			return Result{}, err
		}
		if err := o.awaitServer(ctx, app.ID, session, q.Port); err != nil {
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
		session, err = o.start(ctx, a.Record.ID, "app "+a.Record.ID, a.Command, a.Root, o.environment(*a))
		if err != nil {
			return err
		}
	}
	a.Session = session
	if err := o.modify(ctx, a.Record.ID, func(stored *localApp) { stored.Session = session }); err != nil {
		return err
	}
	if err := o.awaitServer(ctx, a.Record.ID, session, a.Port); err != nil {
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

// awaitServer waits for the app's worker to listen on its port, then records
// the one address the proxy may dial. A listener that fails the identity check
// ends the wait at once: a process that took the port after the preflight is
// not going to leave, and the app cannot bind it.
func (o *Origin) awaitServer(ctx context.Context, id, session string, port int) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		upstream, err := o.checkServer(ctx, session, port)
		if err == nil {
			o.setServing(id, serving{upstream: upstream})
			return nil
		}
		if !errors.Is(err, errNoListener) {
			return fmt.Errorf("app %s: %w", id, err)
		}
		select {
		case <-ctx.Done():
			return errors.New("app: server readiness timed out")
		case <-ticker.C:
		}
	}
}
func (o *Origin) checkServer(ctx context.Context, session string, port int) (netip.AddrPort, error) {
	return checkServerListener(ctx, port, func() ([]int, error) { return o.config.Workers.Processes(ctx, session) })
}

// setServing records a listener check and republishes the routes, so the
// proxy follows it at once.
func (o *Origin) setServing(id string, state serving) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if state == (serving{}) {
		delete(o.serving, id)
	} else {
		o.serving[id] = state
	}
	o.publishRoutes()
}
func (o *Origin) failCreate(ctx context.Context, id string, err error) error {
	_, deleteErr := o.edge(ctx, Request{Action: "delete", ID: id})
	cleanupErr := o.cleanup(ctx, id)
	return errors.Join(err, deleteErr, cleanupErr)
}

// stop ends only the app's own labelled workers; ordinary sessions never carry
// these labels.
func (o *Origin) stop(ctx context.Context, id string) error {
	// Whatever listens on the port once the worker is gone is not the app.
	o.setServing(id, serving{})
	// A worker can outlive its local record, so search by label regardless. The
	// labels are stopped side by side so one failing or slow stop cannot keep
	// the other worker running.
	labels := []string{"app " + id, "app-setup " + id}
	errs := make([]error, len(labels))
	var wg sync.WaitGroup
	for i, label := range labels {
		wg.Go(func() { errs[i] = o.stopLabel(ctx, label) })
	}
	wg.Wait()
	if errs[0] == nil {
		o.mu.Lock()
		if a, ok := o.state.Apps[id]; ok {
			a.Session = ""
			o.state.Apps[id] = a
		}
		o.mu.Unlock()
	}
	return errors.Join(errs...)
}
func (o *Origin) stopLabel(ctx context.Context, label string) error {
	session, alive, err := o.config.Workers.Find(ctx, label)
	if err != nil {
		return fmt.Errorf("find %s worker: %w", label, err)
	}
	if !alive {
		return nil
	}
	if err := o.config.Workers.Stop(ctx, session); err != nil {
		return fmt.Errorf("stop %s worker %s: %w", label, session, err)
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
	if records == nil {
		// Without an authoritative answer only lapsed leases are known, and a
		// partition is not permission to delete anything.
		ids := o.appIDs(nil)
		return errors.Join(err, o.eachApp(safety, ids, o.guardLapsed), o.eachApp(safety, ids, o.checkServing))
	}
	ids := o.appIDs(records)
	denied := o.eachApp(safety, ids, func(ctx context.Context, id string) error { return o.enforce(ctx, id, records) })
	recovered := o.eachApp(ctx, ids, func(ctx context.Context, id string) error { return o.recover(ctx, id, records) })
	checked := o.eachApp(safety, ids, o.checkServing)
	return errors.Join(err, denied, recovered, checked, o.expireUploads(ctx))
}

// checkServing repeats the readiness listener check for a ready server app, so
// one that later listens beyond loopback, or loses its port to another
// process, stops being served. Its worker keeps running: a squatter's victim
// did nothing wrong, and stopping an app that exposed itself would only have
// recover start it again. Serving resumes once the check passes.
func (o *Origin) checkServing(ctx context.Context, id string) error {
	release, ok := o.tryOp("app "+id, "check listeners")
	if !ok {
		return nil
	}
	defer release()
	o.mu.Lock()
	a, local := o.state.Apps[id]
	o.mu.Unlock()
	if !local || a.Phase != "ready" || a.Command == "" || a.Record.Status != "active" {
		return nil
	}
	session, alive, err := o.config.Workers.Find(ctx, "app "+id)
	if err != nil {
		o.setServing(id, serving{})
		return fmt.Errorf("app %s: find server: %w", id, err)
	}
	if !alive {
		o.setServing(id, serving{})
		return nil
	}
	upstream, err := o.checkServer(ctx, session, a.Port)
	if errors.Is(err, errNoListener) {
		err = listenerFault(fmt.Sprintf("nothing the app runs listens on port %d", a.Port))
	}
	if err != nil {
		return o.suspend(ctx, a, err)
	}
	o.setServing(id, serving{upstream: upstream})
	if a.Record.Ready {
		return nil
	}
	if _, err := o.edge(ctx, Request{Action: "activate", ID: id, Kind: "server", UploadID: a.Record.Revision}); err != nil {
		return fmt.Errorf("app %s: resume serving: %w", id, err)
	}
	return nil
}

// suspend withdraws an app's route here first, then at the edge, which shows
// the app as not ready. A fault already reported is not reported again.
func (o *Origin) suspend(ctx context.Context, a localApp, cause error) error {
	id := a.Record.ID
	fault := strings.TrimPrefix(cause.Error(), "app: ")
	o.mu.Lock()
	reported := o.serving[id].fault == fault
	o.mu.Unlock()
	o.setServing(id, serving{fault: fault})
	var edgeErr error
	if a.Record.Ready {
		if _, err := o.edge(ctx, Request{Action: "suspend", ID: id}); err != nil {
			edgeErr = fmt.Errorf("app %s: withdraw route at the edge: %w", id, err)
		}
	}
	if reported {
		return edgeErr
	}
	return errors.Join(fmt.Errorf("app %s: stopped serving: %w", id, cause), edgeErr)
}

// renewLeases applies the edge's records while still holding the exchange slot,
// so no allocation or activation can land between the edge's answer and its
// use. Records it applied but could not save are still returned: they are
// signed, and a revocation among them must be carried out.
func (o *Origin) renewLeases(ctx context.Context) (map[string]Record, error) {
	ctx, cancel := context.WithTimeout(ctx, maintenanceBudget)
	defer cancel()
	if err := o.lockExchange(ctx); err != nil {
		return nil, fmt.Errorf("app: sync leases with edge: %w", err)
	}
	defer o.unlockExchange()
	result, err := o.edgeLocked(ctx, Request{Action: "sync"})
	var unsaved *unsavedReply
	if err != nil && !errors.As(err, &unsaved) {
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
	if saveErr := o.persist(ctx); saveErr != nil {
		err = errors.Join(err, fmt.Errorf("app: save renewed leases: %w", saveErr))
	}
	return records, err
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

// eachApp runs step for every app, concurrently and each under its own budget.
// Steps claim the app themselves.
func (o *Origin) eachApp(ctx context.Context, ids []string, step func(context.Context, string) error) error {
	var wg sync.WaitGroup
	errs := make([]error, len(ids))
	for i, id := range ids {
		wg.Go(func() {
			stepCtx, cancel := context.WithTimeout(ctx, maintenanceBudget)
			defer cancel()
			errs[i] = step(stepCtx, id)
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

// withdrawal names why the edge no longer allows an app to run: a signed
// deletion or expiry, or a lease that has run out. Records are nil when the
// edge gave no answer.
func (o *Origin) withdrawal(id string, records map[string]Record) string {
	if record, listed := records[id]; listed && record.Status != "active" {
		return record.Status
	}
	o.mu.Lock()
	a, local := o.state.Apps[id]
	o.mu.Unlock()
	if local && !o.config.Now().Before(a.Record.LeaseUntil) {
		return "left without a lease"
	}
	return ""
}

// overrule runs safety work that outranks any operation on the app. If the
// operation does not yield, the app's workers are stopped anyway and its state
// is left for the next pass.
func (o *Origin) overrule(ctx context.Context, id, reason string, work func(context.Context) error) error {
	release, err := o.preempt(ctx, id, reason)
	if err != nil {
		return errors.Join(err, o.stop(ctx, id))
	}
	defer release()
	return work(ctx)
}
func (o *Origin) guardLapsed(ctx context.Context, id string) error {
	reason := o.withdrawal(id, nil)
	if reason == "" {
		return nil
	}
	return o.overrule(ctx, id, reason, func(ctx context.Context) error { return o.stopLapsed(ctx, id) })
}

// enforce applies the edge's answer to one app. A withdrawal overrules the
// app's operation; the other denials only concern apps no operation owns.
func (o *Origin) enforce(ctx context.Context, id string, records map[string]Record) error {
	deny := func(ctx context.Context) error { return o.deny(ctx, id, records) }
	if reason := o.withdrawal(id, records); reason != "" {
		return o.overrule(ctx, id, reason, deny)
	}
	release, ok := o.tryOp("app "+id, "sync")
	if !ok {
		return nil
	}
	defer release()
	return deny(ctx)
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
	if !listed {
		if !local {
			return nil
		}
		return o.denyUnlisted(ctx, id, a)
	}
	switch {
	case record.Status != "active":
		if err := o.cleanup(ctx, id); err != nil {
			return fmt.Errorf("app %s: clean up %s app: %w", id, record.Status, err)
		}
		if _, err := o.edge(ctx, Request{Action: "cleanup", ID: id}); err != nil {
			return fmt.Errorf("app %s: confirm cleanup: %w", id, err)
		}
	case !local:
		// Nothing here can serve it, so traffic would only extend its deadline.
		if _, err := o.edge(ctx, Request{Action: "delete", ID: id}); err != nil {
			return fmt.Errorf("app %s: delete app missing on origin: %w", id, err)
		}
	case !o.config.Now().Before(a.Record.LeaseUntil):
		// Listed but already past its lease, as when the reply outlived it.
		return o.stopLapsed(ctx, id)
	case a.Phase != "ready" && a.Phase != "activating":
		if _, err := o.edge(ctx, Request{Action: "delete", ID: id}); err != nil {
			return fmt.Errorf("app %s: delete unfinished app: %w", id, err)
		}
	}
	return nil
}

// denyUnlisted handles an app the edge no longer lists: it cannot route to it or
// renew it. The files wait one idle period past the last lease, as long as the
// edge keeps an app without traffic, so a restored edge can list it again first.
func (o *Origin) denyUnlisted(ctx context.Context, id string, a localApp) error {
	if o.config.Now().Before(a.Record.LeaseUntil.Add(IdleTTL)) {
		return o.stopLapsed(ctx, id)
	}
	if err := o.cleanup(ctx, id); err != nil {
		return fmt.Errorf("app %s: remove app the edge no longer lists: %w", id, err)
	}
	return nil
}

// recover restores what an interrupted operation or a lost worker left behind
// for an app the edge still lists as active. It decides from the stored record,
// which may be newer than the pass's records if an operation ran in between.
func (o *Origin) recover(ctx context.Context, id string, records map[string]Record) error {
	if _, listed := records[id]; !listed {
		return nil
	}
	release, ok := o.tryOp("app "+id, "sync")
	if !ok {
		return nil
	}
	defer release()
	o.mu.Lock()
	a, local := o.state.Apps[id]
	o.mu.Unlock()
	if !local || a.Record.Status != "active" || !o.config.Now().Before(a.Record.LeaseUntil) {
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
		if err := o.finishActivation(ctx, a, a.Record); err != nil {
			return fmt.Errorf("app %s: finish activation: %w", id, err)
		}
	}
	if a.Command == "" {
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
func packArchive(ctx context.Context, root, archive string) error {
	f, err := os.Create(archive) //nolint:gosec // The archive is inside the looked-up app managed workspace.
	if err != nil {
		return fmt.Errorf("app: create download archive: %w", err)
	}
	_, packErr := Pack(ctx, root, f)
	return errors.Join(packErr, f.Close())
}
func (o *Origin) download(ctx context.Context, q Request) (Result, error) {
	if q.Offset < 0 || q.Offset > MaxArchive {
		return Result{}, errors.New("app: invalid download offset")
	}
	ctx, release, err := o.beginOp(ctx, "app "+q.ID, "download")
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
		if err := packArchive(ctx, a.Root, archive); err != nil {
			return Result{}, err
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

// The edge permits 32 active apps per owner. A second generation of caches lets
// app turnover retain live proofs without taking a new app's replay capacity.
const maxAdmissionApps = 64
const maxViewAdmissions = 4096
const maxDownloadAdmissions = 64

type admissionCache struct {
	seen      map[string]struct{}
	deadlines admissionDeadlines
	views     int
	downloads int
}
type admissionDeadline struct {
	id       string
	until    time.Time
	download bool
}
type admissionDeadlines []admissionDeadline

func (d admissionDeadlines) Len() int           { return len(d) }
func (d admissionDeadlines) Less(i, j int) bool { return d[i].until.Before(d[j].until) }
func (d admissionDeadlines) Swap(i, j int)      { d[i], d[j] = d[j], d[i] }
func (d *admissionDeadlines) Push(value any)    { *d = append(*d, value.(admissionDeadline)) }
func (d *admissionDeadlines) Pop() any {
	last := len(*d) - 1
	value := (*d)[last]
	(*d)[last] = admissionDeadline{}
	*d = (*d)[:last]
	return value
}

type admissionResult uint8

const (
	admissionAccepted admissionResult = iota
	admissionReplayed
	admissionFull
)

func (c *admissionCache) expire(now time.Time) {
	for len(c.deadlines) > 0 && !now.Before(c.deadlines[0].until) {
		expired := heap.Pop(&c.deadlines).(admissionDeadline)
		delete(c.seen, expired.id)
		if expired.download {
			c.downloads--
		} else {
			c.views--
		}
	}
}

func (o *Origin) consumeAdmission(proof Signed, a admission, now time.Time) admissionResult {
	o.admissionMu.Lock()
	defer o.admissionMu.Unlock()
	// Signed deadlines have no monotonic timestamp, so their high-water mark
	// must use wall time too.
	now = now.UTC()
	if now.Before(o.admissionAt) {
		now = o.admissionAt
	}
	o.admissionAt = now
	if !now.Before(a.Until) {
		return admissionReplayed
	}
	for app, cache := range o.admissions {
		cache.expire(now)
		if len(cache.seen) == 0 {
			delete(o.admissions, app)
		}
	}
	cache := o.admissions[a.ID]
	if cache == nil {
		if len(o.admissions) >= maxAdmissionApps {
			return admissionFull
		}
		cache = &admissionCache{seen: map[string]struct{}{}}
		o.admissions[a.ID] = cache
	}
	if _, replayed := cache.seen[proof.ID]; replayed {
		return admissionReplayed
	}
	if a.Download {
		if cache.downloads >= maxDownloadAdmissions {
			return admissionFull
		}
		cache.downloads++
	} else {
		if cache.views >= maxViewAdmissions {
			return admissionFull
		}
		cache.views++
	}
	cache.seen[proof.ID] = struct{}{}
	heap.Push(&cache.deadlines, admissionDeadline{id: proof.ID, until: a.Until, download: a.Download})
	return admissionAccepted
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
	now := o.config.Now()
	var proof Signed
	if err := decode(token, &proof); err != nil || proof.Verify("mesh-app/admission/v1", o.identity, o.config.EdgeIdentity, now) != nil {
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
	if !ValidID(admission.ID) || admission.Method != r.Method || admission.URI != rawURI || !now.Before(admission.Until) || admission.Until.After(proof.IssuedAt.Add(admissionLifetime)) || admission.Host != admission.ID+"."+Domain {
		http.NotFound(w, r)
		return true
	}
	app, ok := (*o.routes.Load())[admission.ID]
	if !ok || app.Phase != "ready" || app.Record.Generation != admission.Generation || !now.Before(app.Record.LeaseUntil) {
		http.Error(w, "app unavailable", http.StatusServiceUnavailable)
		return true
	}
	switch o.consumeAdmission(proof, admission, now) {
	case admissionReplayed:
		http.NotFound(w, r)
		return true
	case admissionFull:
		w.Header().Set("Retry-After", "1")
		http.Error(w, "app busy", http.StatusServiceUnavailable)
		return true
	case admissionAccepted:
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

	if !app.Upstream.IsValid() {
		http.Error(w, "app unavailable", http.StatusServiceUnavailable)
		return true
	}
	// The verified address only: another family's loopback on the same port
	// may belong to anyone.
	target := &url.URL{Scheme: "http", Host: app.Upstream.String()}
	proxy := httputil.NewSingleHostReverseProxy(target)
	transport := &http.Transport{Proxy: nil, MaxResponseHeaderBytes: 1 << 20, ResponseHeaderTimeout: 10 * time.Second, DialContext: netDialer.DialContext}
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
	for _, port := range ports {
		if len(o.portOwnersLocked(port, "")) != 0 {
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
