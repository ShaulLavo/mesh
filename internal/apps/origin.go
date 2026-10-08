package apps

import (
	"container/heap"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	meshserve "github.com/shaul/mesh/internal/serve"
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
	Commit *commitIntent `json:"commit,omitempty"`
	// Candidate is an update being prepared beside this ready app. It stays
	// optional so a state written with one still loads in an older binary.
	Candidate *updateCandidate `json:"candidate,omitempty"`
	Record    Record           `json:"record"`
	Root      string           `json:"root"`
	Command   string           `json:"command,omitempty"`
	Port      int              `json:"port,omitempty"`
	Env       []string         `json:"env,omitempty"`
	Session   string           `json:"session,omitempty"`
	Phase     string           `json:"phase"`
}
type commitIntent struct {
	UploadID     string `json:"uploadId"`
	Digest       string `json:"digest"`
	PreviousRoot string `json:"previousRoot,omitempty"`
}

// updateCandidate is saved before an update's workspace exists and cleared only
// once its setup worker and workspace are gone, so a restart always knows what an
// interrupted update left behind. Recovery rolls it back and never reruns setup.
type updateCandidate struct {
	UploadID string   `json:"uploadId"`
	Digest   string   `json:"digest"`
	Root     string   `json:"root"`
	Command  string   `json:"command,omitempty"`
	Port     int      `json:"port,omitempty"`
	Env      []string `json:"env,omitempty"`
	Setup    string   `json:"setup,omitempty"`
	Session  string   `json:"session,omitempty"`
	// Replacing marks a rollback begun after the live server was stopped for
	// the swap: the app label may then run the rejected replacement, which
	// must stop before the previous server comes back.
	Replacing bool `json:"replacing,omitempty"`
}
type appRoute struct {
	Handler *appHandler
	Record  Record
	Root    string
	Port    int
	Phase   string
	// Upstream is the one address a server app may be proxied to; invalid
	// means it is not served.
	Upstream netip.AddrPort
	// upstreamInode is the verified socket at Upstream.
	upstreamInode uint32
}

// serving is what the last listener check found for a server app: the address
// the proxy may dial, or the fault that withdrew it. An app with no entry has
// not passed a check since its worker started, and is not served.
type serving struct {
	upstream netip.AddrPort
	// inode identifies the verified socket; a listener rebound at the same
	// address is a different one.
	inode uint32
	fault string
}
type upload struct {
	ID        string    `json:"id"`
	Size      int64     `json:"size"`
	ExpiresAt time.Time `json:"expiresAt"`
	// Digest is the recipe of the first update attempted with this upload. It
	// outlives that attempt's rollback, so a retry with another recipe is
	// refused until the upload is consumed or expires.
	Digest string `json:"digest,omitempty"`
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
	Failures map[string]SetupFailure  `json:"failures,omitempty"`
}

// Setup failures are kept for the owner, bounded so a looping client cannot grow
// the state: the tail of the output, a fixed number of apps, one idle period.
const (
	maxFailureOutput = 4 << 10
	maxFailures      = 32
)

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
	exchange   chan struct{}
	routes     atomic.Pointer[map[string]appRoute]
	downloadMu sync.Mutex
	// serving is guarded by mu and published with the routes.
	serving map[string]serving
	// downloads are the source snapshots CLI downloads read, by app ID;
	// guarded by mu.
	downloads   map[string][]*downloadArchive
	config      OriginConfig
	identity    string
	admissionMu sync.Mutex
	admissionAt time.Time
	admissions  map[string]*admissionCache
	// storageMounted reports whether the data SSD holding the workload root is
	// mounted. Tests replace it, since mounts cannot be faked in-process.
	storageMounted func(string) error
}

func NewOrigin(ctx context.Context, c OriginConfig) (*Origin, error) {
	if c.Store == nil || len(c.Key) != ed25519.PrivateKeySize || c.Exchange == nil || c.Workers == nil || c.DataRoot == "" {
		return nil, errors.New("app: missing origin dependencies")
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	o := &Origin{config: c, identity: base64.RawURLEncoding.EncodeToString(c.Key.Public().(ed25519.PublicKey)), state: originState{Receipts: map[string]createReceipt{}, Apps: map[string]localApp{}, Uploads: map[string]upload{}}, ops: map[string]*appOp{}, serving: map[string]serving{}, exchange: make(chan struct{}, 1), holds: map[*serviceHold]struct{}{}, admissions: map[string]*admissionCache{}, downloads: map[string][]*downloadArchive{}, storageMounted: workloadStorageMounted}
	if err := load(ctx, c.Store, "apps.origin", &o.state); err != nil {
		return nil, err
	}
	o.publishRoutes()
	o.sweepStaging()
	context.AfterFunc(ctx, o.Close)
	return o, nil
}

// sweepStaging removes staging trees whose origin died between unpacking and
// the rename. Origins with separate state can share one workload root, so a
// tree is removed only if its lock can be taken: the kernel drops a dead
// owner's lock, and a live owner keeps it. It is best effort; whatever stays
// is retried on the next start.
func (o *Origin) sweepStaging() {
	uploads, err := o.openUploads()
	if err != nil {
		return
	}
	defer func() { _ = uploads.Close() }()
	entries, err := fs.ReadDir(uploads.FS(), ".")
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "staging-") {
			continue
		}
		dir, err := lockDir(uploads, entry.Name(), syscall.LOCK_EX|syscall.LOCK_NB)
		if err != nil {
			continue
		}
		_ = uploads.RemoveAll(entry.Name())
		_ = dir.Close()
	}
}

// openUploads opens the upload directory through the workload root and refuses
// a symlink there, so staging is never created or swept anywhere else.
func (o *Origin) openUploads() (*os.Root, error) {
	root, err := os.OpenRoot(o.config.DataRoot)
	if err != nil {
		return nil, fmt.Errorf("app: open workload root: %w", err)
	}
	defer func() { _ = root.Close() }()
	info, err := root.Lstat("uploads")
	if err != nil {
		return nil, fmt.Errorf("app: inspect uploads: %w", err)
	}
	if !info.IsDir() {
		return nil, errors.New("app: uploads is not a directory")
	}
	uploads, err := root.OpenRoot("uploads")
	if err != nil {
		return nil, fmt.Errorf("app: open uploads: %w", err)
	}
	return uploads, nil
}

// heldStaging is a staging directory this origin holds locked until release.
type heldStaging struct {
	path string
	dir  *os.File
}

func (h *heldStaging) release() {
	if h.dir != nil {
		_ = h.dir.Close()
		h.dir = nil
	}
}

// lockStaging creates a staging directory and locks it. A sweep may take a
// fresh directory before its creator locks it; the creator then finds it gone
// and tries another name.
func (o *Origin) lockStaging() (*heldStaging, error) {
	uploads, err := o.openUploads()
	if err != nil {
		return nil, err
	}
	defer func() { _ = uploads.Close() }()
	for range 8 {
		token, err := RandomToken()
		if err != nil {
			return nil, err
		}
		name := "staging-" + token[:16]
		if err := uploads.Mkdir(name, 0700); err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			return nil, fmt.Errorf("app: create staging: %w", err)
		}
		dir, err := lockDir(uploads, name, syscall.LOCK_EX)
		if errors.Is(err, errStagingGone) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return &heldStaging{path: filepath.Join(o.config.DataRoot, "uploads", name), dir: dir}, nil
	}
	return nil, errors.New("app: could not hold a staging directory")
}

var errStagingGone = errors.New("app: staging directory was removed")

// lockDir locks a directory under root by its own descriptor, then checks the
// name still refers to it: it may have been removed or replaced meanwhile.
func lockDir(root *os.Root, name string, how int) (*os.File, error) {
	dir, err := root.Open(name)
	if err != nil {
		return nil, errors.Join(errStagingGone, err)
	}
	if err := syscall.Flock(int(dir.Fd()), how); err != nil {
		return nil, errors.Join(fmt.Errorf("app: lock staging %s: %w", name, err), dir.Close())
	}
	current, err := root.Lstat(name)
	held, heldErr := dir.Stat()
	if err != nil || heldErr != nil || !os.SameFile(current, held) {
		return nil, errors.Join(errStagingGone, dir.Close())
	}
	return dir, nil
}

// persist requires mu.
func (o *Origin) persist(ctx context.Context) error {
	o.publishRoutes()
	return save(ctx, o.config.Store, "apps.origin", o.state)
}
func (o *Origin) publishRoutes() {
	previous := o.routes.Load()
	routes := make(map[string]appRoute, len(o.state.Apps))
	for id, a := range o.state.Apps {
		var old appRoute
		if previous != nil {
			old = (*previous)[id]
		}
		route := cachedAppRoute(a, old, o.serving[id])
		routes[id] = route
	}
	o.routes.Store(&routes)
	if previous != nil {
		for id, old := range *previous {
			if old.Handler != nil && old.Handler.transport != nil && old.Handler != routes[id].Handler {
				old.Handler.transport.CloseIdleConnections()
			}
		}
	}
}

// cachedAppRoute reuses the previous handler while nothing it was built from
// changed. A server app is proxied only to its verified upstream, so it has no
// handler without one. A different verified socket, even at the same address,
// gets a new transport: the old pool may hold connections another process
// accepted while it held the port. Where the platform reports no inode, reuse
// follows the address alone.
func cachedAppRoute(app localApp, old appRoute, verified serving) appRoute {
	upstream := verified.upstream
	route := appRoute{Record: app.Record, Root: app.Root, Port: app.Port, Phase: app.Phase, Upstream: upstream, upstreamInode: verified.inode}
	switch {
	case app.Phase != "ready" || app.Record.Status != "active":
	case old.Handler != nil && old.Root == app.Root && old.Port == app.Port && old.Record.Kind == app.Record.Kind && old.Record.Revision == app.Record.Revision && old.Upstream == upstream && old.upstreamInode == verified.inode:
		route.Handler = old.Handler
	case app.Record.Kind == "static":
		handler, err := meshserve.Handler(meshserve.Service{Name: "temporary-app", Kind: meshserve.Static, Target: app.Root}, "/")
		if err == nil {
			route.Handler = &appHandler{Handler: handler}
		}
	case upstream.IsValid():
		route.Handler = originHandler(route)
	}
	return route
}

func (o *Origin) Close() {
	o.mu.Lock()
	for id := range o.downloads {
		o.releaseDownloadsLocked(id)
	}
	o.mu.Unlock()
	if routes := o.routes.Load(); routes != nil {
		for _, route := range *routes {
			if route.Handler != nil && route.Handler.transport != nil {
				route.Handler.transport.CloseIdleConnections()
			}
		}
	}
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
		return o.inspect(ctx, q)
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

// inspect adds what only the origin knows to the edge's record: the runtime,
// and why the last setup failed, which outlives a failed create's app.
func (o *Origin) inspect(ctx context.Context, q Request) (Result, error) {
	result, err := o.edge(ctx, q)
	if err != nil {
		return result, err
	}
	o.mu.Lock()
	a, ok := o.state.Apps[q.ID]
	failure, failed := o.state.Failures[q.ID]
	problem := o.serving[q.ID].fault
	o.mu.Unlock()
	if ok {
		result.Runtime = &RuntimeInfo{Phase: a.Phase, SessionID: a.Session, Command: a.Command, Port: a.Port, Root: a.Root, Problem: problem}
	}
	if failed && o.config.Now().Before(failure.ExpiresAt) {
		if result.Runtime == nil {
			result.Runtime = &RuntimeInfo{Phase: "failed"}
		}
		result.Runtime.Failure = &failure
	}
	return result, nil
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
	if len(q.Data) > ChunkSize || q.Offset < 0 {
		return Result{}, errors.New("app: upload chunk exceeds limit")
	}
	if q.Offset+int64(len(q.Data)) > MaxArchive {
		return Result{}, ErrArchiveTooLarge
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
	if q.Action == "update" && !storedID(q.ID) {
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
	// This operation owns the app, so a candidate here belongs to an attempt that
	// died with an earlier daemon. Retrying it is an explicit request to run it
	// again, from the start, but only with the recipe it was first given.
	if q.Action == "update" {
		o.mu.Lock()
		attempted := o.state.Uploads[q.UploadID].Digest
		o.mu.Unlock()
		if attempted != "" && attempted != digest {
			return Result{}, fmt.Errorf("app %s: update %s conflicts with the recipe of its interrupted attempt", q.ID, q.UploadID)
		}
	}
	if q.Action == "update" && exists && current.Candidate != nil {
		if err := o.resolveCandidate(ctx, q.ID); err != nil {
			return Result{}, err
		}
	}
	if err := validateRecipe(q); err != nil {
		return Result{}, err
	}
	if err := o.admitRecipe(q); err != nil {
		return Result{}, err
	}
	// An unpredictable name cannot collide with staging a crash left behind, and
	// nothing can be planted in advance where unpack writes.
	held, err := o.lockStaging()
	if err != nil {
		return Result{}, fmt.Errorf("app: stage upload %s: %w", q.UploadID, err)
	}
	staging := held.path
	// Every exit removes the staging until the rename hands it to the app, and
	// removes it before giving up the lock, so no sweep races the removal.
	defer func() {
		if staging != "" {
			_ = os.RemoveAll(staging)
		}
		held.release()
	}()
	if err := unpack(o.uploadPath(q.UploadID), staging, q.Digest); err != nil {
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
		return Result{}, err
	}
	app, workspace := local.Record, local.Root
	// swapped is set once the live server may have been stopped for the new one;
	// before that, a failure has no reason to touch it.
	swapped := false
	// candidate is this operation's own copy; state only ever gets copies of it,
	// because other apps' saves serialize whatever state points to.
	candidate := updateCandidate{UploadID: q.UploadID, Digest: digest, Root: workspace, Command: q.Command, Port: q.Port, Env: q.Env, Setup: q.Setup}
	fail := func(cause error) (Result, error) {
		// Undoing must finish even when the request or the operation's context
		// has ended; start still refuses to restart a revoked app.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), maintenanceBudget)
		defer cancel()
		if q.Action == "create" {
			return Result{}, o.failCreate(ctx, app.ID, cause)
		}
		return Result{}, fmt.Errorf("app %s: update %s: %w", app.ID, q.UploadID, o.failUpdate(ctx, previous, candidate, swapped, cause))
	}
	err = os.MkdirAll(filepath.Dir(workspace), 0700)
	if err == nil && q.Action == "update" {
		err = o.modify(ctx, app.ID, func(a *localApp) {
			published := candidate
			a.Candidate = &published
			if u, ok := o.state.Uploads[q.UploadID]; ok {
				u.Digest = digest
				o.state.Uploads[q.UploadID] = u
			}
		})
	}
	if err == nil {
		err = os.Rename(staging, workspace)
	}
	if err != nil {
		if q.Action == "create" {
			return Result{}, o.failCreate(ctx, app.ID, err)
		}
		return fail(err)
	}
	staging = ""
	held.release()
	if q.Setup != "" {
		if err := o.runSetup(ctx, q, local, &candidate); err != nil {
			return fail(err)
		}
	}
	if q.Action == "update" {
		swapped = true
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

// runSetup runs the recipe's setup in the new workspace. An update records the
// setup session on its candidate before waiting, so a crash leaves it findable.
func (o *Origin) runSetup(ctx context.Context, q Request, a localApp, candidate *updateCandidate) error {
	id := a.Record.ID
	if err := validateDataRoot(a.Root); err != nil {
		return err
	}
	session, err := o.start(ctx, id, "app-setup "+id, q.Setup, a.Root, o.environment(a))
	if err != nil {
		return err
	}
	if q.Action == "update" {
		candidate.Session = session
		published := *candidate
		if err := o.modify(ctx, id, func(a *localApp) { a.Candidate = &published }); err != nil {
			return err
		}
	}
	if err := o.waitSetup(ctx, session); err != nil {
		return o.recordSetupFailure(ctx, id, q.UploadID, session, err)
	}
	o.config.Workers.Forget(ctx, "app-setup "+id)
	return nil
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
	delete(o.state.Failures, record.ID)
	err := o.persist(ctx)
	o.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.Remove(o.uploadPath(commit.UploadID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if commit.PreviousRoot != "" && commit.PreviousRoot != a.Root {
		if err := o.removeWorkload(commit.PreviousRoot); err != nil {
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
		verified, err := o.checkServer(ctx, session, port)
		if err == nil {
			o.setServing(id, serving{upstream: verified.Address, inode: verified.Inode})
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
func (o *Origin) checkServer(ctx context.Context, session string, port int) (listenerSocket, error) {
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
	return fmt.Errorf("app %s: %w", id, errors.Join(err, deleteErr, cleanupErr))
}

// failUpdate puts the previous revision back after an update failed. The
// previous record goes back with the candidate still attached, and
// resolveCandidate saves that before it stops or removes anything. Before the
// swap only the setup worker is stopped; after it, the server now running is
// the rejected replacement, so it is stopped and the previous one restarted
// through ensureServer, which checks its port and listener like any start.
func (o *Origin) failUpdate(ctx context.Context, previous localApp, candidate updateCandidate, swapped bool, cause error) error {
	id := previous.Record.ID
	candidate.Replacing = swapped
	previous.Candidate = &candidate
	if swapped {
		previous.Session = ""
	}
	o.mu.Lock()
	// Keep the lease and expiry Sync applied while the update ran.
	if latest, ok := o.state.Apps[id]; ok {
		revision := previous.Record.Revision
		previous.Record = latest.Record
		previous.Record.Revision = revision
	}
	o.state.Apps[id] = previous
	o.mu.Unlock()
	if err := o.resolveCandidate(ctx, id); err != nil {
		return errors.Join(cause, err)
	}
	if !swapped || previous.Command == "" {
		return cause
	}
	o.mu.Lock()
	restored := o.state.Apps[id]
	o.mu.Unlock()
	return errors.Join(cause, o.ensureServer(ctx, &restored))
}

// resolveCandidate rolls back an update preparation no operation owns any more.
// The record it rolls back to is saved before anything is stopped or removed,
// so a crash part way never leaves durable state naming a deleted workspace.
// Each step must succeed before the next, and the candidate is cleared last,
// so whatever fails is retried by recovery. It never reruns setup, and leaves
// restarting the previous server to ensureServer. The caller owns the app.
func (o *Origin) resolveCandidate(ctx context.Context, id string) error {
	o.mu.Lock()
	a, ok := o.state.Apps[id]
	var saveErr error
	if ok && a.Candidate != nil {
		saveErr = o.persist(ctx)
	}
	o.mu.Unlock()
	if !ok || a.Candidate == nil {
		return nil
	}
	c := a.Candidate
	if saveErr != nil {
		return fmt.Errorf("app %s: save rollback of update %s: %w", id, c.UploadID, saveErr)
	}
	labels := []string{"app-setup " + id}
	if c.Replacing {
		labels = append(labels, "app "+id)
	}
	for _, label := range labels {
		if err := o.stopLabel(ctx, label); err != nil {
			return fmt.Errorf("app %s: roll back update %s: %w", id, c.UploadID, err)
		}
	}
	if err := o.removeWorkload(c.Root); err != nil {
		return fmt.Errorf("app %s: remove update %s: %w", id, c.UploadID, err)
	}
	return o.modify(ctx, id, func(a *localApp) { a.Candidate = nil })
}

// removeWorkload deletes workload files only while the data SSD holding them is
// mounted. On an unmounted SSD the path is just absent, and removing it would
// report success without deleting anything.
func (o *Origin) removeWorkload(path string) error {
	if err := o.storageMounted(o.config.DataRoot); err != nil {
		return err
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// recordSetupFailure keeps the setup's last output for the owner before the
// worker holding it is forgotten, and returns the error the owner sees now.
func (o *Origin) recordSetupFailure(ctx context.Context, id, uploadID, session string, cause error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), maintenanceBudget)
	defer cancel()
	var output string
	if source, ok := o.config.Workers.(interface {
		Output(context.Context, string) string
	}); ok {
		output = source.Output(ctx, session)
	}
	if len(output) > maxFailureOutput {
		output = output[len(output)-maxFailureOutput:]
	}
	output = strings.ToValidUTF8(output, "")
	o.mu.Lock()
	if o.state.Failures == nil {
		o.state.Failures = map[string]SetupFailure{}
	}
	o.state.Failures[id] = SetupFailure{UploadID: uploadID, Error: cause.Error(), Output: output, ExpiresAt: o.config.Now().Add(IdleTTL)}
	for len(o.state.Failures) > maxFailures {
		oldest := ""
		for key, failure := range o.state.Failures {
			if oldest == "" || failure.ExpiresAt.Before(o.state.Failures[oldest].ExpiresAt) {
				oldest = key
			}
		}
		delete(o.state.Failures, oldest)
	}
	saveErr := o.persist(ctx)
	o.mu.Unlock()
	if output != "" {
		cause = fmt.Errorf("%w; last setup output:\n%s", cause, output)
	}
	if saveErr != nil {
		cause = errors.Join(cause, fmt.Errorf("save setup failure: %w", saveErr))
	}
	return cause
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
	if !storedID(id) {
		return errors.New("app: invalid cleanup id")
	}
	// The snapshots hold the source being deleted.
	o.mu.Lock()
	o.releaseDownloadsLocked(id)
	o.mu.Unlock()
	if err := o.stop(ctx, id); err != nil {
		return err
	}
	if err := o.removeAppFiles(id); err != nil {
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

// removeAppFiles deletes the app's managed directory. A missing workload root
// means there is nothing left to delete, unless the data SSD that holds it is
// simply not mounted; that must stay a failure so cleanup is retried.
func (o *Origin) removeAppFiles(id string) error {
	// An unmounted SSD can leave the root absent or as an empty mountpoint;
	// either way nothing would really be deleted.
	if err := o.storageMounted(o.config.DataRoot); err != nil {
		return fmt.Errorf("app %s: workload storage: %w", id, err)
	}
	root, err := os.OpenRoot(o.config.DataRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("app %s: open workload root: %w", id, err)
	}
	defer func() { _ = root.Close() }()
	if err := root.RemoveAll(filepath.Join("apps", id)); err != nil {
		return fmt.Errorf("app %s: remove workload: %w", id, err)
	}
	return nil
}
func workloadStorageMounted(root string) error {
	parent, err := existingDirectory(root)
	if err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return fmt.Errorf("resolve workload directory: %w", err)
	}
	return validateDataMount(filepath.Clean(root), resolved)
}

// Sync renews every app's lease from the edge, then reconciles each app on its
// own: safety work first, recovery second. A failing or slow app is reported
// with the others instead of holding them back.
func (o *Origin) Sync(ctx context.Context) error {
	o.expireDownloads()
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
	verified, err := o.checkServer(ctx, session, a.Port)
	if errors.Is(err, errNoListener) {
		err = listenerFault(fmt.Sprintf("nothing the app runs listens on port %d", a.Port))
	}
	if err != nil {
		return o.suspend(ctx, a, err)
	}
	o.setServing(id, serving{upstream: verified.Address, inode: verified.Inode})
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
	if err := o.settleReady(ctx, a); err != nil {
		return err
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

// settleReady finishes what an interrupted operation left on a ready app: a
// committed activation is completed, an update candidate is rolled back. The
// rollback reads the stored record itself, so it runs after the commit writes.
func (o *Origin) settleReady(ctx context.Context, a localApp) error {
	id := a.Record.ID
	if a.Commit != nil {
		if err := o.finishActivation(ctx, a, a.Record); err != nil {
			return fmt.Errorf("app %s: finish activation: %w", id, err)
		}
	}
	if err := o.resolveCandidate(ctx, id); err != nil {
		return fmt.Errorf("app %s: roll back interrupted update: %w", id, err)
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
	for id, failure := range o.state.Failures {
		if !o.config.Now().Before(failure.ExpiresAt) {
			delete(o.state.Failures, id)
		}
	}
	return o.persist(ctx)
}

// downloadArchive is a snapshot of one app source that CLI downloads read a
// chunk per request. Requests carry no transfer identity, so every download of
// the same source shares the snapshot, and a reader finishing never closes it:
// only idle time, the app's deletion or the origin closing does.
type downloadArchive struct {
	source   string
	file     *os.File
	lastRead time.Time
}

// downloadIdle is how long a snapshot stays without a chunk read.
const downloadIdle = 5 * time.Minute

// maxAppDownloads bounds the snapshots kept per app: one for the current
// source and one for a revision a transfer started before an update. Past it
// the least recently read one is closed. A reader still on it is then served
// another snapshot's bytes, which the CLI's archive check refuses, so that
// download fails and asks to be run again rather than saving a mixed archive.
const maxAppDownloads = 2

// packUnnamed packs an app's source into a file unlinked as soon as it exists.
// The random name is created exclusively, so a planted symlink or hard link is
// never written through, and no exit or crash can leave the archive behind.
func (o *Origin) packUnnamed(ctx context.Context, id, root string) (*os.File, error) {
	f, err := os.CreateTemp(filepath.Join(o.config.DataRoot, "apps", id), ".download-*")
	if err != nil {
		return nil, fmt.Errorf("app %s: create download archive: %w", id, err)
	}
	if err := os.Remove(f.Name()); err != nil {
		return nil, errors.Join(fmt.Errorf("app %s: unlink download archive: %w", id, err), f.Close())
	}
	if _, err := Pack(ctx, root, f); err != nil {
		return nil, errors.Join(fmt.Errorf("app %s: pack download: %w", id, err), f.Close())
	}
	return f, nil
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
	if q.Offset == 0 {
		if err := o.snapshotDownload(ctx, q.ID, a.Root); err != nil {
			return Result{}, err
		}
	}
	return o.readDownload(q.ID, a.Root, q.Offset)
}

// snapshotDownload makes sure a snapshot of the app's current source exists,
// reusing one another download still reads. The caller owns the app.
func (o *Origin) snapshotDownload(ctx context.Context, id, source string) error {
	o.mu.Lock()
	for _, d := range o.downloads[id] {
		if d.source == source {
			o.mu.Unlock()
			return nil
		}
	}
	o.mu.Unlock()
	f, err := o.packUnnamed(ctx, id, source)
	if err != nil {
		return err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.downloads[id] = append(o.downloads[id], &downloadArchive{source: source, file: f, lastRead: o.config.Now()})
	kept := o.downloads[id]
	for len(kept) > maxAppDownloads {
		oldest := 0
		for i, d := range kept {
			if d.lastRead.Before(kept[oldest].lastRead) {
				oldest = i
			}
		}
		_ = kept[oldest].file.Close()
		kept = slices.Delete(kept, oldest, oldest+1)
	}
	o.downloads[id] = kept
	return nil
}

// readDownload returns one chunk from the snapshot of the app's current
// source, or from the most recently read one when the source changed since
// the transfer began. The caller owns the app.
func (o *Origin) readDownload(id, source string, offset int64) (Result, error) {
	o.mu.Lock()
	var d *downloadArchive
	for _, candidate := range o.downloads[id] {
		if candidate.source == source || d == nil || (d.source != source && candidate.lastRead.After(d.lastRead)) {
			d = candidate
		}
	}
	if d != nil {
		d.lastRead = o.config.Now()
	}
	o.mu.Unlock()
	if d == nil {
		return Result{}, fmt.Errorf("app %s: download expired; start it again", id)
	}
	b := make([]byte, ChunkSize)
	n, err := d.file.ReadAt(b, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return Result{}, fmt.Errorf("app %s: read download: %w", id, err)
	}
	return Result{Data: b[:n], Done: errors.Is(err, io.EOF) || n < ChunkSize}, nil
}

// releaseDownloadsLocked closes an app's snapshots; it requires mu.
func (o *Origin) releaseDownloadsLocked(id string) {
	for _, d := range o.downloads[id] {
		_ = d.file.Close()
	}
	delete(o.downloads, id)
}

// expireDownloads closes snapshots no download has read for downloadIdle. It
// needs no edge, so Sync runs it whether or not the edge answered.
func (o *Origin) expireDownloads() {
	o.mu.Lock()
	defer o.mu.Unlock()
	for id, list := range o.downloads {
		if o.ops["app "+id] != nil {
			continue
		}
		kept := list[:0]
		for _, d := range list {
			if o.config.Now().Before(d.lastRead.Add(downloadIdle)) {
				kept = append(kept, d)
			} else {
				_ = d.file.Close()
			}
		}
		if len(kept) == 0 {
			delete(o.downloads, id)
		} else {
			o.downloads[id] = kept
		}
	}
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
	if !storedID(admission.ID) || admission.Method != r.Method || admission.URI != rawURI || !now.Before(admission.Until) || admission.Until.After(proof.IssuedAt.Add(admissionLifetime)) || !acceptedAppHost(admission.Host, admission.ID) {
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
	}
	if app.Handler == nil {
		http.Error(w, "app unavailable", http.StatusServiceUnavailable)
		return true
	}
	app.Handler.ServeHTTP(w, request)
	return true
}

func (o *Origin) configCheckSourceDownload(w http.ResponseWriter, r *http.Request, app appRoute) error {
	f, err := o.packUnnamed(r.Context(), app.Record.ID, app.Root)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("app %s: rewind download: %w", app.Record.ID, err)
	}
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
