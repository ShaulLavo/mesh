package mcphost

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/shaul/mesh/internal/mcpwire"
)

const codeCatalogStale = "catalog.stale"

// Expectation is the service state a gateway resolved before it routed a
// request. The host refuses the request when the service moved on.
type Expectation struct {
	Revision int64
	Account  string
}

// serviceForTargetLocked resolves a target. An exact target names a stable
// service ID only, so a retired ID never matches another service's name.
func (s *Supervisor) serviceForTargetLocked(target Target) (Definition, error) {
	if !target.ExactID {
		return s.serviceLocked(target.Service)
	}
	d, ok := s.services[target.Service]
	if !ok {
		return Definition{}, fmt.Errorf("service %s: %w", target.Service, ErrNotFound)
	}
	return d, nil
}

func checkExpectation(d Definition, target Target) error {
	if target.Expect == nil {
		return nil
	}
	if d.Revision != target.Expect.Revision || d.AccountLabel != target.Expect.Account {
		return failure(codeCatalogStale, true, "service %s is now at revision %d; list it again", d.Name, d.Revision)
	}
	return nil
}

// precheck refuses a request whose service moved on, and a continuation
// whose reference cannot be valid for this caller, service revision and
// operation, before any instance is chosen or started. Dispatch checks the
// instance generation again and consumes the reference.
func (s *Supervisor) precheck(caller Caller, target Target, request mcpwire.Message) error {
	s.mu.Lock()
	d, err := s.serviceForTargetLocked(target)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := checkExpectation(d, target); err != nil {
		return err
	}
	return s.router.continuations.precheck(caller, d, request)
}

func (c *continuations) precheck(caller Caller, d Definition, request mcpwire.Message) error {
	params := request.Params()
	if member(params, requestStateKey) == nil && member(params, "inputResponses") == nil {
		return nil
	}
	ref, ok := decodeJSONString(member(params, requestStateKey))
	if !ok {
		return stateInvalid("is missing its Mesh reference")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expireLocked()
	entry := c.entries[ref]
	if entry == nil {
		return stateInvalid("is unknown, expired or already used")
	}
	b := entry.binding
	digest := operationDigest(params)
	same := subtle.ConstantTimeCompare([]byte(b.caller), []byte(caller.Key())) == 1 && b.grant == caller.Grant &&
		b.service == d.ID && b.revision == d.Revision && b.account == d.AccountLabel && b.method == request.Method() &&
		subtle.ConstantTimeCompare(b.digest[:], digest[:]) == 1
	if !same {
		return stateInvalid("belongs to a different caller, service revision or request")
	}
	return entry.permits(request)
}

// ListingRevision is the owning host's revision of one service's listings
// for one credential scope (PLAN section 7.3). Gateways compare tokens for
// equality only.
type ListingRevision struct {
	ServiceID string `json:"serviceId"`
	Account   string `json:"account,omitempty"`
	Token     string `json:"token"`
}

// listingRevisions advances a service's listing revision when a listing
// that passes through the router differs from the last one seen, or when an
// upstream reports a list change.
type listingRevisions struct {
	mu      sync.Mutex
	epoch   string
	entries map[[2]string]*listingState
	notify  func(ListingRevision)
}

type listingState struct {
	counter uint64
	digests map[string]string
}

func newListingRevisions(notify func(ListingRevision)) *listingRevisions {
	epoch, _ := randomID()
	if len(epoch) > 8 {
		epoch = epoch[:8]
	}
	return &listingRevisions{epoch: epoch, entries: map[[2]string]*listingState{}, notify: notify}
}

// token names the listing state: the definition revision, this daemon's
// epoch (a restart forgets observations) and the change counter.
func (l *listingRevisions) token(serviceID, account string, revision int64) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.tokenLocked(serviceID, account, revision)
}

func (l *listingRevisions) tokenLocked(serviceID, account string, revision int64) string {
	var counter uint64
	if st := l.entries[[2]string{serviceID, account}]; st != nil {
		counter = st.counter
	}
	return strconv.FormatInt(revision, 10) + "." + l.epoch + "." + strconv.FormatUint(counter, 10)
}

func (l *listingRevisions) state(serviceID, account string) *listingState {
	key := [2]string{serviceID, account}
	st := l.entries[key]
	if st == nil {
		st = &listingState{digests: map[string]string{}}
		l.entries[key] = st
	}
	return st
}

// observe records one complete first page of a listing.
func (l *listingRevisions) observe(route Route, method string, items json.RawMessage) {
	sum := sha256.Sum256(items)
	digest := hex.EncodeToString(sum[:])
	l.mu.Lock()
	st := l.state(route.Service, route.Account)
	previous, seen := st.digests[method]
	st.digests[method] = digest
	changed := seen && previous != digest
	if changed {
		st.counter++
	}
	token := l.tokenLocked(route.Service, route.Account, route.Revision)
	l.mu.Unlock()
	if changed && l.notify != nil {
		l.notify(ListingRevision{ServiceID: route.Service, Account: route.Account, Token: token})
	}
}

// bump records an upstream list change.
func (l *listingRevisions) bump(serviceID, account string, revision int64) {
	l.mu.Lock()
	st := l.state(serviceID, account)
	st.counter++
	clear(st.digests)
	token := l.tokenLocked(serviceID, account, revision)
	l.mu.Unlock()
	if l.notify != nil {
		l.notify(ListingRevision{ServiceID: serviceID, Account: account, Token: token})
	}
}

// published reports a new definition revision, which is a new listing
// revision by itself.
func (l *listingRevisions) published(d Definition) {
	l.mu.Lock()
	delete(l.entries, [2]string{d.ID, d.AccountLabel})
	token := l.tokenLocked(d.ID, d.AccountLabel, d.Revision)
	l.mu.Unlock()
	if l.notify != nil {
		l.notify(ListingRevision{ServiceID: d.ID, Account: d.AccountLabel, Token: token})
	}
}

func (l *listingRevisions) removed(d Definition) {
	l.mu.Lock()
	delete(l.entries, [2]string{d.ID, d.AccountLabel})
	l.mu.Unlock()
	if l.notify != nil {
		l.notify(ListingRevision{ServiceID: d.ID, Account: d.AccountLabel})
	}
}

// listingMethods maps each list method to its result field.
var listingMethods = map[string]string{
	mcpwire.MethodListTools: "tools", mcpwire.MethodListPrompts: "prompts",
	mcpwire.MethodListResources: "resources", mcpwire.MethodListResourceTemplates: "resourceTemplates",
}

// observeEvent feeds the listing revisions from one restored message.
func (l *listingRevisions) observeEvent(route Route, request, m mcpwire.Message) {
	if m.Kind() == mcpwire.Notification {
		if strings.HasSuffix(m.Method(), "/list_changed") {
			l.bump(route.Service, route.Account, route.Revision)
		}
		return
	}
	field, ok := listingMethods[request.Method()]
	if !ok || m.Kind() != mcpwire.Response || member(request.Params(), "cursor") != nil {
		return
	}
	if kind, _ := decodeJSONString(member(m.Raw(), resultKey, "resultType")); kind != "complete" {
		return
	}
	if items := member(m.Raw(), resultKey, field); items != nil {
		l.observe(route, request.Method(), items)
	}
}

// ListingRevisions returns every service's current listing revision, for a
// state.watch snapshot.
func (s *Supervisor) ListingRevisions() []ListingRevision {
	s.mu.Lock()
	defs := make([]Definition, 0, len(s.services))
	for _, d := range s.services {
		defs = append(defs, d)
	}
	s.mu.Unlock()
	out := make([]ListingRevision, 0, len(defs))
	for _, d := range defs {
		out = append(out, ListingRevision{ServiceID: d.ID, Account: d.AccountLabel, Token: s.router.listings.token(d.ID, d.AccountLabel, d.Revision)})
	}
	slices.SortFunc(out, func(a, b ListingRevision) int { return compareStrings(a.ServiceID, b.ServiceID) })
	return out
}

// CallerScope names the caller's authenticated scope, grant incarnation
// included, without revealing it. A gateway binds cached listings to it, so
// revocation and re-approval both retire them.
func CallerScope(caller Caller) string {
	sum := sha256.Sum256([]byte(caller.Key() + "\x00" + caller.Grant))
	return hex.EncodeToString(sum[:16])
}
