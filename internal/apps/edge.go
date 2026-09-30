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
	"time"

	"github.com/shaul/mesh/internal/webauth"
)

type EdgeConfig struct {
	Acquire       func(*http.Request, string) (func(), error)
	ClientIP      func(*http.Request) netip.Addr
	NetworkOwners func(context.Context, netip.Addr) ([]string, error)
	Store         NameStore
	Key           ed25519.PrivateKey
	Allowed       map[string]bool
	Resolve       func(context.Context, string) (netip.AddrPort, error)
	Now           func() time.Time
}
type edgeState struct {
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
	ReserveAppNameAndState(context.Context, string, string, string, []byte) error
}
type edgeMutation struct {
	state          edgeState
	pendingName    string
	pendingOwner   string
	cancelAll      []string
	cancelVisitors []string
	retireNames    []Record
}

type Edge struct {
	pendingRetire map[string]Record
	inflight      map[string]map[string]admittedRequest
	mu            sync.Mutex
	config        EdgeConfig
	state         edgeState
	identity      string
	auth          *webauth.Service
	slots         chan struct{}
}
type edgeReply struct {
	RequestID string `json:"requestId"`
	Result    Result `json:"result"`
	Error     string `json:"error,omitempty"`
}

func NewEdge(ctx context.Context, c EdgeConfig) (*Edge, error) {
	if c.Store == nil || len(c.Key) != ed25519.PrivateKeySize || c.Resolve == nil {
		return nil, errors.New("app: missing edge dependencies")
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	e := &Edge{config: c, identity: base64.RawURLEncoding.EncodeToString(c.Key.Public().(ed25519.PublicKey)), slots: make(chan struct{}, 128), inflight: map[string]map[string]admittedRequest{}, state: edgeState{Apps: map[string]Record{}, Owners: map[string]ownerState{}}}
	if err := load(ctx, c.Store, "apps.edge", &e.state); err != nil {
		return nil, err
	}
	if e.state.Apps == nil || e.state.Owners == nil || len(e.state.Apps) > 16384 {
		return nil, errors.New("app: invalid durable edge state")
	}
	if err := c.Store.ReserveAppName(ctx, ManagementHost, e.identity); err != nil {
		return nil, fmt.Errorf("app: reserve management host: %w", err)
	}
	auth, err := webauth.New(c.Store, c.Now)
	if err != nil {
		return nil, err
	}
	e.auth = auth
	e.pendingRetire = map[string]Record{}
	for id, app := range e.state.Apps {
		if app.Status != "active" {
			e.pendingRetire[id] = app
		}
	}
	return e, nil
}
func (e *Edge) next() *edgeMutation {
	// Cached replies are immutable. Mutations replace map entries, not their contents.
	return &edgeMutation{state: edgeState{Apps: maps.Clone(e.state.Apps), Owners: maps.Clone(e.state.Owners)}}
}

func (e *Edge) persist(ctx context.Context, next *edgeMutation) error {
	b, err := json.Marshal(next.state)
	if err != nil {
		return fmt.Errorf("app: encode edge state: %w", err)
	}
	if next.pendingName != "" {
		err = e.config.Store.(atomicNameStore).ReserveAppNameAndState(ctx, next.pendingName, next.pendingOwner, "apps.edge", b)
	} else {
		err = e.config.Store.SaveAppState(ctx, "apps.edge", b)
	}
	if err != nil {
		return fmt.Errorf("app: persist edge state: %w", err)
	}
	e.state = next.state
	for _, app := range next.retireNames {
		e.pendingRetire[app.ID] = app
	}
	for _, id := range next.cancelAll {
		e.cancelLocked(id)
	}
	for _, id := range next.cancelVisitors {
		for token, request := range e.inflight[id] {
			if !request.owner {
				request.cancel()
				delete(e.inflight[id], token)
			}
		}
	}
	return nil
}

// Retirement follows the state commit so a failed save cannot revoke a live app.
// Sweep reports failures and retries them without blocking another app's request.
func (e *Edge) retireLocked(ctx context.Context, apps []Record) error {
	var errs []error
	for _, app := range apps {
		if err := e.config.Store.SetAppNameInactive(ctx, app.ID+"."+Domain, app.Owner); err != nil {
			errs = append(errs, fmt.Errorf("app %s: retire name: %w", app.ID, err))
		} else {
			delete(e.pendingRetire, app.ID)
		}
	}
	return errors.Join(errs...)
}

func (e *Edge) Exchange(ctx context.Context, s Signed) (Signed, error) {
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
		return Sign("mesh-app/response/v1", s.Owner, s.Sequence, edgeReply{RequestID: s.ID, Result: owner.Result, Error: owner.Error}, e.config.Key, e.config.Now())
	}
	if s.Sequence <= owner.Sequence {
		return Signed{}, errors.New("app: stale owner sequence")
	}
	next := e.next()
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
	return Sign("mesh-app/response/v1", s.Owner, s.Sequence, edgeReply{RequestID: s.ID, Result: result, Error: errorText}, e.config.Key, e.config.Now())
}
func (e *Edge) apply(ctx context.Context, next *edgeMutation, owner string, q Request) (Result, error) {
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
	case "browser.approve":
		return Result{}, e.auth.Approve(ctx, q.Code, owner)
	case "browser.list":
		browsers, err := e.auth.List(ctx, owner)
		b, _ := json.Marshal(browsers)
		return Result{Browsers: b}, err
	case "browser.revoke":
		err := e.auth.Revoke(ctx, owner, q.BrowserID)
		if err == nil {
			// Browser revocation commits through webauth independently of edge state.
			for _, app := range next.state.Apps {
				if app.Owner == owner {
					e.cancelLocked(app.ID)
				}
			}
		}
		return Result{}, err
	}
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
		if app.Ready && app.Revision != q.UploadID {
			next.cancelAll = append(next.cancelAll, app.ID)
			app.Generation++
			app.ExpiresAt = e.config.Now().Add(IdleTTL)
		}
		app.Ready = true
		app.Revision = q.UploadID
		if q.Kind != "" {
			app.Kind = q.Kind
		}
	case "renew":
		if app.Status != "active" {
			return Result{}, errors.New("app: app expired")
		}
		app.ExpiresAt = e.config.Now().UTC().Add(IdleTTL)
	case "public", "private":
		if app.Status != "active" {
			return Result{}, errors.New("app: app expired")
		}
		if app.Visibility != q.Action && q.Action == "private" {
			next.cancelVisitors = append(next.cancelVisitors, app.ID)
		}
		app.Visibility = q.Action
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
		return Result{}, errors.New("app: unknown edge operation")
	}
	next.state.Apps[app.ID] = app
	app.LeaseUntil = minTime(app.ExpiresAt, e.config.Now().Add(LeaseTTL))
	return Result{App: &app}, nil
}
func (e *Edge) allocate(ctx context.Context, next *edgeMutation, owner, kind string) (Result, error) {
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
		if exists, err := e.config.Store.AppNameExists(ctx, id+"."+Domain); err != nil {
			return Result{}, err
		} else if exists {
			continue
		}
		next.pendingName = id + "." + Domain
		next.pendingOwner = owner
		app := Record{ID: id, Owner: owner, Kind: kind, Visibility: "private", Status: "active", Cleanup: "pending", ExpiresAt: e.config.Now().UTC().Add(IdleTTL), Generation: 1, LeaseUntil: e.config.Now().Add(LeaseTTL)}
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
func (e *Edge) expireLocked(next *edgeMutation) {
	now := e.config.Now()
	for id, app := range next.state.Apps {
		if app.Status != "active" || now.Before(app.ExpiresAt) {
			continue
		}
		next.cancelAll = append(next.cancelAll, app.ID)
		app.Generation++
		app.Status = "expired"
		app.Ready = false
		app.Cleanup = "pending"
		next.state.Apps[id] = app
		next.retireNames = append(next.retireNames, app)
	}
}
func (e *Edge) Sweep(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	next := e.next()
	for _, app := range e.pendingRetire {
		next.retireNames = append(next.retireNames, app)
	}
	e.expireLocked(next)
	if err := e.persist(ctx, next); err != nil {
		return err
	}
	return e.retireLocked(ctx, next.retireNames)
}
func (e *Edge) lookup(ctx context.Context, id string, touch bool) (Record, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	next := e.next()
	e.expireLocked(next)
	app, ok := next.state.Apps[id]
	if touch && ok && app.Status == "active" {
		app.ExpiresAt = e.config.Now().UTC().Add(IdleTTL)
		next.state.Apps[id] = app
	}
	if len(next.cancelAll) > 0 || (touch && ok && app.Status == "active") {
		if err := e.persist(ctx, next); err != nil {
			return Record{}, ok, err
		}
		_ = e.retireLocked(ctx, next.retireNames)
	}
	return app, ok, nil
}

type admittedRequest struct {
	cancel context.CancelFunc
	owner  bool
}

func (e *Edge) cancelLocked(id string) {
	for _, request := range e.inflight[id] {
		request.cancel()
	}
	delete(e.inflight, id)
}
func (e *Edge) admit(r *http.Request, id string) (Record, *http.Request, func(), error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	next := e.next()
	e.expireLocked(next)
	app, exists := next.state.Apps[id]
	if !exists || app.Status != "active" || !app.Ready {
		return Record{}, nil, nil, errors.New("app unavailable")
	}
	if app.Visibility == "private" && !ambientOwnerAllowed(r, URL(id)) {
		return Record{}, nil, nil, errors.New("app private")
	}
	if app.Visibility == "private" && !networkOwns(r, app.Owner) {
		owner, err := e.auth.ViewOwner(r.Context(), r, id)
		if err != nil || owner != app.Owner {
			return Record{}, nil, nil, errors.New("app private")
		}
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		app.ExpiresAt = e.config.Now().UTC().Add(IdleTTL)
		next.state.Apps[id] = app
	}
	if err := e.persist(r.Context(), next); err != nil {
		return Record{}, nil, nil, err
	}
	_ = e.retireLocked(r.Context(), next.retireNames)
	token, err := RandomToken()
	if err != nil {
		return Record{}, nil, nil, err
	}
	ctx, cancel := context.WithCancel(r.Context())
	if e.inflight[id] == nil {
		e.inflight[id] = map[string]admittedRequest{}
	}
	viewer, _ := e.auth.ViewOwner(r.Context(), r, id)
	e.inflight[id][token] = admittedRequest{cancel: cancel, owner: ambientOwnerAllowed(r, URL(id)) && (viewer == app.Owner || networkOwns(r, app.Owner))}
	release := func() { cancel(); e.mu.Lock(); delete(e.inflight[id], token); e.mu.Unlock() }
	return app, r.WithContext(ctx), release, nil
}
