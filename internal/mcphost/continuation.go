package mcphost

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"sync"
	"time"

	"github.com/shaul/mesh/internal/mcpwire"
)

// Section 6.4 bounds. References live only in memory, so a daemon restart
// invalidates every unresolved one.
const (
	continuationTTL      = 10 * time.Minute
	maxContinuations     = 1024
	maxCallerContinuance = 128
	continuationBytes    = 16 << 20
	continuationPrefix   = "mesh-c1."
)

type continuationBinding struct {
	caller     string
	grant      string
	service    string
	revision   int64
	account    string
	instance   string
	generation string
	method     string
	digest     [32]byte
}

type continuation struct {
	binding continuationBinding
	// state is the upstream's own requestState, absent when it sent none.
	state   *string
	inputs  map[string]string
	expires time.Time
	size    int
}

type continuations struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]*continuation
	bytes   int
}

func newContinuations() *continuations {
	return &continuations{now: time.Now, entries: make(map[string]*continuation)}
}

// upstreamState is what a resolved continuation sends upstream in place of
// the caller's reference.
type upstreamState struct {
	state *string
}

func (s *upstreamState) apply(raw []byte) ([]byte, error) {
	if s == nil {
		return raw, nil
	}
	if s.state == nil {
		return setMember(raw, []string{"params", "requestState"}, nil)
	}
	return setMember(raw, []string{"params", "requestState"}, jsonString(*s.state))
}

func stateInvalid(reason string) *Failure {
	return failure(codeStateInvalid, false, "continuation %s", reason)
}

// resolve validates and consumes a continuation reference. A request without
// one is a first round and needs nothing.
func (c *continuations) resolve(route Route, request mcpwire.Message) (*upstreamState, error) {
	params := request.Params()
	ref, hasRef := decodeJSONString(member(params, "requestState"))
	if member(params, "requestState") == nil && member(params, "inputResponses") == nil {
		return nil, nil
	}
	if !hasRef {
		return nil, stateInvalid("is missing its Mesh reference")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expireLocked()
	entry := c.entries[ref]
	if entry == nil {
		return nil, stateInvalid("is unknown, expired or already used")
	}
	if !entry.binding.matches(bindingFor(route, request)) {
		return nil, stateInvalid("belongs to a different caller, service, instance or request")
	}
	if err := entry.permits(request); err != nil {
		return nil, err
	}
	// Consumed at dispatch: an interrupted retry is never replayed.
	c.deleteLocked(ref)
	return &upstreamState{state: entry.state}, nil
}

func (b continuationBinding) matches(other continuationBinding) bool {
	same := b.caller == other.caller && b.grant == other.grant && b.service == other.service &&
		b.revision == other.revision && b.account == other.account && b.instance == other.instance &&
		b.generation == other.generation && b.method == other.method
	return same && subtle.ConstantTimeCompare(b.digest[:], other.digest[:]) == 1
}

func (e *continuation) permits(request mcpwire.Message) error {
	responses, err := decodeMembers(member(request.Params(), "inputResponses"))
	if err != nil && member(request.Params(), "inputResponses") != nil {
		return stateInvalid("has malformed input responses")
	}
	caps := member(request.Raw(), "params", "_meta", mcpwire.ClientCapsKey)
	for key, value := range responses {
		method, ok := e.inputs[key]
		if !ok || !responseMatches(method, value) {
			return stateInvalid("answers an input that was not requested")
		}
		if member(caps, capabilityFor(method)) == nil {
			return stateInvalid("retry lacks the client capability for " + method)
		}
	}
	return nil
}

func responseMatches(method string, value json.RawMessage) bool {
	switch method {
	case "elicitation/create":
		return member(value, "action") != nil
	case "roots/list":
		return member(value, "roots") != nil
	case "sampling/createMessage":
		return member(value, "model") != nil
	default:
		return false
	}
}

func capabilityFor(method string) string {
	switch method {
	case "elicitation/create":
		return "elicitation"
	case "roots/list":
		return "roots"
	default:
		return "sampling"
	}
}

// capture replaces the upstream's requestState in an input_required result
// with a fresh host reference bound to this caller and request.
func (c *continuations) capture(route Route, request mcpwire.Message, raw []byte) ([]byte, error) {
	if !bytes.Equal(member(raw, "result", "resultType"), []byte(`"input_required"`)) {
		return raw, nil
	}
	entry := &continuation{binding: bindingFor(route, request), inputs: map[string]string{}, size: len(raw)}
	if state, ok := decodeJSONString(member(raw, "result", "requestState")); ok {
		entry.state = &state
	}
	inputs, _ := decodeMembers(member(raw, "result", "inputRequests"))
	for key, input := range inputs {
		entry.inputs[key], _ = decodeJSONString(member(input, "method"))
	}
	ref, err := c.store(entry)
	if err != nil {
		return nil, err
	}
	return setMember(raw, []string{"result", "requestState"}, jsonString(ref))
}

func (c *continuations) store(entry *continuation) (string, error) {
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", failure(codeResourceLimit, true, "create continuation reference: %w", err)
	}
	ref := continuationPrefix + base64.RawURLEncoding.EncodeToString(token[:])
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expireLocked()
	if len(c.entries) >= maxContinuations || c.bytes+entry.size > continuationBytes || c.countLocked(entry.binding.caller) >= maxCallerContinuance {
		return "", failure(codeResourceLimit, true, "too many pending continuations")
	}
	entry.expires = c.now().Add(continuationTTL)
	c.entries[ref] = entry
	c.bytes += entry.size
	return ref, nil
}

func (c *continuations) countLocked(caller string) int {
	n := 0
	for _, entry := range c.entries {
		if entry.binding.caller == caller {
			n++
		}
	}
	return n
}

func (c *continuations) expireLocked() {
	now := c.now()
	for ref, entry := range c.entries {
		if !now.Before(entry.expires) {
			c.deleteLocked(ref)
		}
	}
}

func (c *continuations) deleteLocked(ref string) {
	if entry := c.entries[ref]; entry != nil {
		c.bytes -= entry.size
		delete(c.entries, ref)
	}
}

// retire drops every continuation of one grant, for revocation.
func (c *continuations) retire(grant string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for ref, entry := range c.entries {
		if entry.binding.grant == grant {
			c.deleteLocked(ref)
		}
	}
}

func bindingFor(route Route, request mcpwire.Message) continuationBinding {
	return continuationBinding{
		caller: route.Caller.Key(), grant: route.Caller.Grant, service: route.Service, revision: route.Revision,
		account: route.Account, instance: route.Instance, generation: route.Generation,
		method: request.Method(), digest: operationDigest(request.Params()),
	}
}

// operationDigest covers the canonical operation parameters and excludes the
// fields that change from round to round.
func operationDigest(params json.RawMessage) [32]byte {
	fields, _ := decodeMembers(params)
	delete(fields, "_meta")
	delete(fields, "requestState")
	delete(fields, "inputResponses")
	canonical, _ := canonicalJSON(fields)
	return sha256.Sum256(canonical)
}

// canonicalJSON sorts object keys at every level and keeps numbers exact.
func canonicalJSON(fields map[string]json.RawMessage) ([]byte, error) {
	encoded, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}
