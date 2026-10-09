package apps

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shaul/mesh/internal/domainpolicy"
	"github.com/shaul/mesh/internal/serve"
	"github.com/shaul/mesh/internal/webauth"
)

type RegistryConfig struct {
	// ViewHostReady checks the currently installed live certificate for a host.
	// An absent callback keeps private browser grants disabled.
	ViewHostReady func(string) bool
	Acquire       func(*http.Request, string) (func(), error)
	ClientIP      func(*http.Request) netip.Addr
	NetworkOwners func(context.Context, netip.Addr) ([]string, error)
	Store         NameStore
	Key           ed25519.PrivateKey
	Allowed       map[string]bool
	Resolve       func(context.Context, string) (netip.AddrPort, error)
	Now           func() time.Time
}
type registryState struct {
	Apps   map[string]Record     `json:"apps"`
	Owners map[string]ownerState `json:"owners"`
}
type ownerState struct {
	Sequence uint64 `json:"sequence"`
	ID       string `json:"id"`
	Digest   string `json:"digest"`
	Result   Result `json:"result"`
	Error    string `json:"error,omitempty"`
}
type atomicNameStore interface {
	ReserveAppNamesAndState(context.Context, []string, string, string, []byte) error
}

// ErrCapacity identifies transient concurrency saturation without retrying invalid requests.
var ErrCapacity = errors.New("app: concurrency capacity exhausted")

const activityPersistSlack = time.Minute
const maximumCapacityWaiters = 128
const capacityWaitTimeout = 5 * time.Second
const capacityRetryInterval = 25 * time.Millisecond

type appRuntime struct {
	mu        sync.Mutex
	record    atomic.Pointer[Record]
	persisted atomic.Int64
	inflight  map[string]admittedRequest
}

type registryMutation struct {
	locked       map[string]*appRuntime
	state        registryState
	pendingName  string
	pendingOwner string
	cancelAll    []string
	retireNames  []Record
}

type Registry struct {
	pendingRetire   map[string]Record
	runtime         atomic.Pointer[map[string]*appRuntime]
	transports      proxyTransports
	mu              sync.Mutex
	config          RegistryConfig
	state           registryState
	identity        string
	auth            *webauth.Service
	slots           chan struct{}
	capacityWaiters chan struct{}
}
type registryReply struct {
	RequestID string `json:"requestId"`
	Result    Result `json:"result"`
	Error     string `json:"error,omitempty"`
}

func NewRegistry(ctx context.Context, c RegistryConfig) (*Registry, error) {
	if domainpolicy.Primary() == "" {
		return nil, errors.New("app: deployment domain is required")
	}
	if c.Store == nil || len(c.Key) != ed25519.PrivateKeySize || c.Resolve == nil {
		return nil, errors.New("app: missing registry dependencies")
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	e := &Registry{config: c, identity: base64.RawURLEncoding.EncodeToString(c.Key.Public().(ed25519.PublicKey)), slots: make(chan struct{}, 128), capacityWaiters: make(chan struct{}, maximumCapacityWaiters), state: registryState{Apps: map[string]Record{}, Owners: map[string]ownerState{}}}
	if err := load(ctx, c.Store, "apps.edge", &e.state); err != nil {
		return nil, err
	}
	if e.state.Apps == nil || e.state.Owners == nil || len(e.state.Apps) > 16384 {
		return nil, errors.New("app: invalid durable registry state")
	}
	if err := c.Store.ReserveAppNames(ctx, appNames(ManagementHost()), e.identity); err != nil {
		return nil, fmt.Errorf("app: reserve management hosts: %w", err)
	}
	auth, err := webauth.New(c.Store, c.Now)
	if err != nil {
		return nil, err
	}
	e.auth = auth
	e.pendingRetire = map[string]Record{}
	if err := e.reserveExistingApps(ctx); err != nil {
		return nil, err
	}
	e.publishRuntime(e.state, nil)
	context.AfterFunc(ctx, e.Close)
	return e, nil
}

// Cached replies and published records are immutable. Activity replaces only its
// app's record; cold mutations merge those deadlines into the durable snapshot.
func (e *Registry) next() *registryMutation {
	next := &registryMutation{state: registryState{Apps: maps.Clone(e.state.Apps), Owners: maps.Clone(e.state.Owners)}, locked: map[string]*appRuntime{}}
	for id, app := range next.state.Apps {
		if rt := (*e.runtime.Load())[id]; rt != nil {
			current := rt.record.Load()
			if app.Status == "active" && current.Generation == app.Generation {
				app.ExpiresAt = maxTime(app.ExpiresAt, current.ExpiresAt)
				next.state.Apps[id] = app
			}
		}
	}
	return next
}
func (next *registryMutation) lockApp(e *Registry, id string) {
	if next.locked[id] != nil {
		return
	}
	rt := (*e.runtime.Load())[id]
	if rt == nil {
		return
	}
	rt.mu.Lock()
	next.locked[id] = rt
	app, ok := next.state.Apps[id]
	current := rt.record.Load()
	if ok && app.Status == "active" && current.Generation == app.Generation {
		app.ExpiresAt = maxTime(app.ExpiresAt, current.ExpiresAt)
		next.state.Apps[id] = app
	}
}
func (next *registryMutation) unlock() {
	for _, rt := range next.locked {
		rt.mu.Unlock()
	}
}

func (e *Registry) publishRuntime(state registryState, locked map[string]*appRuntime) {
	runtimes := map[string]*appRuntime{}
	if previous := e.runtime.Load(); previous != nil {
		runtimes = maps.Clone(*previous)
	}
	for id, app := range state.Apps {
		rt := runtimes[id]
		if rt == nil {
			rt = &appRuntime{}
			runtimes[id] = rt
		}
		if locked[id] == nil {
			rt.mu.Lock()
		}
		rt.persisted.Store(app.ExpiresAt.UnixNano())
		// Other apps keep admitting traffic during this save. Publishing must not
		// erase an extension that is newer than the snapshot written to storage.
		if current := rt.record.Load(); current != nil && app.Status == "active" && current.Generation == app.Generation {
			app.ExpiresAt = minTime(maxTime(app.ExpiresAt, current.ExpiresAt), app.ExpiresAt.Add(activityPersistSlack))
		}
		rt.record.Store(&app)
		if locked[id] == nil {
			rt.mu.Unlock()
		}
	}
	for id := range runtimes {
		if _, ok := state.Apps[id]; !ok {
			delete(runtimes, id)
		}
	}
	e.runtime.Store(&runtimes)
}

func (e *Registry) persist(ctx context.Context, next *registryMutation) error {
	b, err := json.Marshal(next.state)
	if err != nil {
		return fmt.Errorf("app: encode registry state: %w", err)
	}
	if next.pendingName != "" {
		err = e.config.Store.(atomicNameStore).ReserveAppNamesAndState(ctx, appNames(next.pendingName), next.pendingOwner, "apps.edge", b)
	} else {
		err = e.config.Store.SaveAppState(ctx, "apps.edge", b)
	}
	if err != nil {
		return fmt.Errorf("app: persist registry state: %w", err)
	}
	e.state = next.state
	e.publishRuntime(next.state, next.locked)
	for _, app := range next.retireNames {
		e.pendingRetire[app.ID] = app
	}
	for _, id := range next.cancelAll {
		e.cancelLocked(id)
	}
	return nil
}

// Retirement follows the state commit so a failed save cannot revoke a live app.
// Sweep reports failures and retries them without blocking another app's request.
func (e *Registry) retireLocked(ctx context.Context, apps []Record) error {
	var errs []error
	for _, app := range apps {
		var failed bool
		for _, domain := range domainpolicy.Domains() {
			if err := e.config.Store.SetAppNameInactive(ctx, app.ID+"."+domain, app.Owner); err != nil {
				errs = append(errs, fmt.Errorf("app %s: retire name: %w", app.ID, err))
				failed = true
			}
		}
		if !failed {
			delete(e.pendingRetire, app.ID)
		}
	}
	return errors.Join(errs...)
}

func (e *Registry) Exchange(ctx context.Context, s Signed) (Signed, error) {
	if !e.config.Allowed[s.Owner] {
		return Signed{}, errors.New("app: owner is not allowed")
	}
	if err := s.Verify("mesh-app/request/v1", e.identity, s.Owner, e.config.Now()); err != nil {
		return Signed{}, err
	}
	var request Request
	if err := decode(s.Body, &request); err != nil {
		return Signed{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	owner := e.state.Owners[s.Owner]
	digest := digestBytes(s.Body)
	if s.Sequence == owner.Sequence && s.ID == owner.ID && digest == owner.Digest {
		return Sign("mesh-app/response/v1", s.Owner, s.Sequence, registryReply{RequestID: s.ID, Result: owner.Result, Error: owner.Error}, e.config.Key, e.config.Now())
	}
	if s.Sequence <= owner.Sequence {
		return Signed{}, errors.New("app: stale owner sequence")
	}
	next := e.next()
	defer next.unlock()
	result, opErr := e.apply(ctx, next, s.Owner, request)
	errorText := ""
	if opErr != nil {
		errorText = opErr.Error()
	}
	next.state.Owners[s.Owner] = ownerState{Sequence: s.Sequence, ID: s.ID, Digest: digest, Result: result, Error: errorText}
	if err := e.persist(ctx, next); err != nil {
		return Signed{}, err
	}
	_ = e.retireLocked(ctx, next.retireNames)
	return Sign("mesh-app/response/v1", s.Owner, s.Sequence, registryReply{RequestID: s.ID, Result: result, Error: errorText}, e.config.Key, e.config.Now())
}
func (e *Registry) apply(ctx context.Context, next *registryMutation, owner string, q Request) (Result, error) {
	e.expireLocked(next)
	switch q.Action {
	case "allocate":
		return e.allocate(ctx, next, owner, q.Kind)
	case "list", "sync":
		result := Result{Apps: []Record{}}
		for _, app := range next.state.Apps {
			if app.Owner == owner && app.Cleanup != "complete" {
				app.LeaseUntil = minTime(app.ExpiresAt, e.config.Now().Add(LeaseTTL))
				result.Apps = append(result.Apps, app)
			}
		}
		sort.Slice(result.Apps, func(i, j int) bool { return result.Apps[i].ID < result.Apps[j].ID })
		return result, nil
	case "browser.inspect":
		return e.inspectPairing(ctx, q.Code)
	case "browser.approve":
		return Result{}, e.auth.Approve(ctx, q.Code, owner)
	case "browser.list":
		browsers, err := e.auth.List(ctx, owner)
		b, _ := json.Marshal(browsers)
		return Result{Browsers: b}, err
	case "browser.revoke":
		for id, app := range next.state.Apps {
			if app.Owner == owner {
				next.lockApp(e, id)
			}
		}
		return Result{}, e.revokeBrowser(ctx, owner, q.BrowserID)
	}
	next.lockApp(e, q.ID)
	app, ok := next.state.Apps[q.ID]
	if !ok || app.Owner != owner {
		return Result{}, errors.New("app: app not found for this owner")
	}
	switch q.Action {
	case "inspect":
	case "activate":
		if app.Status != "active" {
			return Result{}, errors.New("app: app expired")
		}
		// A different revision replaces whatever was activated before, even
		// while suspended; the same one only resumes it.
		if app.Revision != "" && app.Revision != q.UploadID {
			next.cancelAll = append(next.cancelAll, app.ID)
			app.Generation++
			app.ExpiresAt = e.config.Now().Add(IdleTTL)
		}
		app.Ready = true
		app.Revision = q.UploadID
		if q.Kind != "" {
			app.Kind = q.Kind
		}
	case "suspend":
		// The origin found the app's listeners unsafe; activate restores it.
		if app.Status != "active" {
			return Result{}, errors.New("app: app expired")
		}
		if app.Ready {
			next.cancelAll = append(next.cancelAll, app.ID)
		}
		app.Ready = false
	case "renew":
		if app.Status != "active" {
			return Result{}, errors.New("app: app expired")
		}
		app.ExpiresAt = e.config.Now().UTC().Add(IdleTTL)
	case "delete":
		next.cancelAll = append(next.cancelAll, app.ID)
		app.Status = "deleted"
		app.Ready = false
		app.Cleanup = "pending"
		next.retireNames = append(next.retireNames, app)
	case "cleanup":
		if app.Status == "active" {
			return Result{}, errors.New("app: cannot clean active app")
		}
		app = Record{ID: app.ID, Owner: app.Owner, Status: app.Status, Cleanup: "complete"}
	default:
		return Result{}, errors.New("app: unknown registry operation")
	}
	next.state.Apps[app.ID] = app
	app.LeaseUntil = minTime(app.ExpiresAt, e.config.Now().Add(LeaseTTL))
	return Result{App: &app}, nil
}
func (e *Registry) allocate(ctx context.Context, next *registryMutation, owner, kind string) (Result, error) {
	if _, ok := e.config.Store.(atomicNameStore); !ok {
		return Result{}, errors.New("app: allocation requires atomic name and state storage")
	}
	if kind != "static" && kind != "server" {
		return Result{}, errors.New("app: kind must be static or server")
	}
	count := 0
	active := 0
	for _, a := range next.state.Apps {
		if a.Owner == owner && a.Status == "active" {
			count++
		}
		if a.Status == "active" {
			active++
		}
	}
	if count >= 32 || active >= 4096 || len(next.state.Apps) >= 16384 {
		return Result{}, errors.New("app: app capacity exceeded")
	}
	for range 64 {
		var b [4]byte
		if _, err := rand.Read(b[:]); err != nil {
			return Result{}, err
		}
		const alphabet = "0123456789abcdefghjkmnpqrstvwxyz"
		for i := range b {
			b[i] = alphabet[int(b[i])%len(alphabet)]
		}
		id := string(b[:])
		if !ValidID(id) {
			continue
		}
		if _, exists := next.state.Apps[id]; exists {
			continue
		}
		if exists, err := e.nameAllocated(ctx, id); err != nil {
			return Result{}, err
		} else if exists {
			continue
		}
		next.pendingName = id + "." + Domain()
		next.pendingOwner = owner
		app := Record{ID: id, Owner: owner, Kind: kind, Status: "active", Cleanup: "pending", ExpiresAt: e.config.Now().UTC().Add(IdleTTL), Generation: 1, LeaseUntil: e.config.Now().Add(LeaseTTL)}
		next.state.Apps[id] = app
		return Result{App: &app}, nil
	}
	return Result{}, errors.New("app: short name allocation exhausted")
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
func (e *Registry) expireLocked(next *registryMutation) {
	for id := range next.state.Apps {
		e.expireAppLocked(next, id)
	}
}
func (e *Registry) expireAppLocked(next *registryMutation, id string) {
	app, ok := next.state.Apps[id]
	if !ok || app.Status != "active" || e.config.Now().Before(app.ExpiresAt) {
		return
	}
	next.lockApp(e, id)
	app = next.state.Apps[id]
	if e.config.Now().Before(app.ExpiresAt) {
		return
	}
	next.cancelAll = append(next.cancelAll, app.ID)
	app.Generation++
	app.Status = "expired"
	app.Ready = false
	app.Cleanup = "pending"
	next.state.Apps[id] = app
	next.retireNames = append(next.retireNames, app)
}

func (e *Registry) Sweep(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	next := e.next()
	defer next.unlock()
	for _, app := range e.pendingRetire {
		next.retireNames = append(next.retireNames, app)
	}
	e.expireLocked(next)
	if err := e.persist(ctx, next); err != nil {
		return err
	}
	return e.retireLocked(ctx, next.retireNames)
}
func (e *Registry) lookup(ctx context.Context, id string, touch bool) (Record, bool, error) {
	rt := (*e.runtime.Load())[id]
	if rt == nil {
		return Record{}, false, nil
	}
	app := *rt.record.Load()
	if app.Status == "active" && !e.config.Now().Before(app.ExpiresAt) {
		return e.flushActivity(ctx, app, true)
	}
	if touch && app.Status == "active" {
		return e.activity(ctx, app)
	}
	return app, true, nil
}

func (e *Registry) activity(ctx context.Context, admitted Record) (Record, bool, error) {
	rt := (*e.runtime.Load())[admitted.ID]
	if rt == nil {
		return Record{}, false, nil
	}
	rt.mu.Lock()
	app := *rt.record.Load()
	now := e.config.Now()
	expired := app.Status == "active" && !now.Before(app.ExpiresAt)
	if !expired && app.Status == "active" && app.Generation == admitted.Generation {
		app = rt.proposeActivity(app, now.UTC().Add(IdleTTL))
	}
	rt.mu.Unlock()
	if expired {
		return e.flushActivity(ctx, app, true)
	}
	return e.flushIfDue(ctx, rt, app)
}

func (rt *appRuntime) proposeActivity(app Record, deadline time.Time) Record {
	app.ExpiresAt = maxTime(app.ExpiresAt, deadline)
	bounded := app
	// A failed or pending save must not publish more activity than restart can lose.
	bounded.ExpiresAt = minTime(app.ExpiresAt, time.Unix(0, rt.persisted.Load()).Add(activityPersistSlack))
	rt.record.Store(&bounded)
	return app
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func (e *Registry) flushIfDue(ctx context.Context, rt *appRuntime, app Record) (Record, bool, error) {
	if app.Status == "active" && app.ExpiresAt.Sub(time.Unix(0, rt.persisted.Load())) >= activityPersistSlack {
		return e.flushActivity(ctx, app, false)
	}
	return app, true, nil
}

func (e *Registry) flushActivity(ctx context.Context, proposed Record, expire bool) (Record, bool, error) {
	id := proposed.ID
	e.mu.Lock()
	defer e.mu.Unlock()
	rt := (*e.runtime.Load())[id]
	if rt == nil {
		return Record{}, false, nil
	}
	current := *rt.record.Load()
	if !expire && (current.Status != "active" || current.Generation != proposed.Generation || maxTime(current.ExpiresAt, proposed.ExpiresAt).Sub(time.Unix(0, rt.persisted.Load())) < activityPersistSlack) {
		return current, true, nil
	}
	next := e.next()
	defer next.unlock()
	next.lockApp(e, id)
	e.expireAppLocked(next, id)
	app := next.state.Apps[id]
	if !expire && app.Status == "active" && app.Generation == proposed.Generation {
		app.ExpiresAt = maxTime(app.ExpiresAt, proposed.ExpiresAt)
		next.state.Apps[id] = app
	}
	if err := e.persist(ctx, next); err != nil {
		return Record{}, true, err
	}
	_ = e.retireLocked(ctx, next.retireNames)
	app, ok := next.state.Apps[id]
	return app, ok, nil
}

type admittedRequest struct {
	cancel    context.CancelFunc
	browserID string
}

func (rt *appRuntime) cancelWhere(match func(admittedRequest) bool) {
	for token, request := range rt.inflight {
		if match(request) {
			request.cancel()
			delete(rt.inflight, token)
		}
	}
	if len(rt.inflight) == 0 {
		rt.inflight = nil
	}
}
func (e *Registry) cancelLocked(id string) {
	if rt := (*e.runtime.Load())[id]; rt != nil {
		rt.cancelWhere(func(admittedRequest) bool { return true })
	}
}
func (e *Registry) revokeBrowser(ctx context.Context, owner, browserID string) error {
	if err := e.auth.Revoke(ctx, owner, browserID); err != nil {
		return fmt.Errorf("app: revoke browser %s for owner %s: %w", browserID, owner, err)
	}
	// Revocation is already durable in webauth even if the subsequent registry save fails.
	for _, rt := range *e.runtime.Load() {
		if rt.record.Load().Owner == owner {
			rt.cancelWhere(func(request admittedRequest) bool { return request.browserID == browserID })
		}
	}
	return nil
}

func (e *Registry) admit(r *http.Request, id string) (Record, *http.Request, func(), error) {
	rt := (*e.runtime.Load())[id]
	if rt == nil {
		return Record{}, nil, nil, errors.New("app unavailable")
	}
	token, err := RandomToken()
	if err != nil {
		return Record{}, nil, nil, err
	}
	rt.mu.Lock()
	app := *rt.record.Load()
	if app.Status != "active" || !app.Ready || !e.config.Now().Before(app.ExpiresAt) {
		rt.mu.Unlock()
		return Record{}, nil, nil, errors.New("app unavailable")
	}
	viewer, _ := e.auth.ViewSession(r.Context(), r, id)
	owner := serve.AmbientOwnerAllowed(r, appOrigin(r, id), serve.RequireWebSocketOrigin) && (viewer.Owner == app.Owner || networkOwns(r, app.Owner))
	if !owner {
		rt.mu.Unlock()
		return Record{}, nil, nil, errors.New("app private")
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		app = rt.proposeActivity(app, e.config.Now().UTC().Add(IdleTTL))
	}
	ctx, cancel := context.WithCancel(r.Context())
	if rt.inflight == nil {
		rt.inflight = map[string]admittedRequest{}
	}
	rt.inflight[token] = admittedRequest{cancel: cancel, browserID: viewer.BrowserID}
	rt.mu.Unlock()
	release := func() {
		cancel()
		rt.mu.Lock()
		delete(rt.inflight, token)
		if len(rt.inflight) == 0 {
			rt.inflight = nil
		}
		rt.mu.Unlock()
	}
	if _, _, err := e.flushIfDue(ctx, rt, app); err != nil {
		release()
		return Record{}, nil, nil, err
	}
	return app, r.WithContext(ctx), release, nil
}

func (e *Registry) HasHost(host string) bool {
	label, _, accepted := domainpolicy.Label(host, false)
	if !accepted {
		return false
	}
	if label == managementLabel {
		return true
	}
	runtimes := e.runtime.Load()
	if runtimes == nil {
		return false
	}
	return (*runtimes)[label] != nil
}

func (e *Registry) inspectPairing(ctx context.Context, code string) (Result, error) {
	info, err := e.auth.Inspect(ctx, code)
	if err != nil {
		return Result{}, fmt.Errorf("app: inspect browser pairing: %w", err)
	}
	return Result{Pairing: &info}, nil
}

func appNames(primaryName string) []string {
	label, _, accepted := domainpolicy.Label(primaryName, false)
	if !accepted {
		return nil
	}
	var names []string
	for _, domain := range domainpolicy.Domains() {
		names = append(names, label+"."+domain)
	}
	return names
}

func (e *Registry) nameAllocated(ctx context.Context, id string) (bool, error) {
	for _, domain := range domainpolicy.Domains() {
		exists, err := e.config.Store.AppNameExists(ctx, id+"."+domain)
		if err != nil {
			return false, fmt.Errorf("app: check deployment reservation: %w", err)
		}
		if exists {
			return true, nil
		}
	}
	return false, nil
}

func (e *Registry) reserveExistingApps(ctx context.Context) error {
	for id, app := range e.state.Apps {
		if app.Status == "active" {
			if err := e.config.Store.ReserveAppNames(ctx, appNames(id+"."+Domain()), app.Owner); err != nil {
				return fmt.Errorf("app: reserve deployment aliases: %w", err)
			}
		}
		if app.Status != "active" {
			e.pendingRetire[id] = app
		}
	}
	return nil
}
