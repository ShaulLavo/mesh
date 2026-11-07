package daemon

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/shaul/mesh/internal/hostprofile"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/update"
)

const fleetAttemptTimeout = 15 * time.Second
const fleetPollInterval = 30 * time.Second

type fleetRetry struct {
	digest string
	next   time.Time
	delay  time.Duration
}
type fleetProfileController struct {
	store          *hostprofile.FleetStore
	membershipPath string
	id             string
	trusted        func(string) bool
	call           func(context.Context, update.Host, string, any, any) error
	now            func() time.Time
	after          func(time.Duration) <-chan time.Time
	jitter         func(time.Duration) time.Duration
	poke           chan struct{}
	retries        map[string]fleetRetry
	report         func(error)
}

func newFleetProfileController(stateDir, membershipPath, id string, client update.Client, trusted func(string) bool, report func(error)) *fleetProfileController {
	return &fleetProfileController{store: hostprofile.OpenFleetStore(stateDir), membershipPath: membershipPath, id: id, trusted: trusted, call: client.Call, now: time.Now, after: time.After, jitter: fleetJitter, poke: make(chan struct{}, 1), retries: map[string]fleetRetry{}, report: report}
}
func (c *fleetProfileController) scope() (hostprofile.FleetScope, []update.Host, error) {
	scope := hostprofile.FleetScope{Members: []string{}, Trusted: c.trusted}
	if c.membershipPath == "" {
		return scope, nil, nil
	}
	fleet, err := update.ReadFleet(c.membershipPath)
	if errors.Is(err, os.ErrNotExist) {
		return scope, nil, nil
	}
	if err != nil {
		return scope, nil, fmt.Errorf("read fleet profile membership: %w", err)
	}
	members := make([]update.Host, 0, len(fleet.Members))
	for _, member := range fleet.Members {
		if member.ID == c.id {
			continue
		}
		scope.Members = append(scope.Members, member.ID)
		members = append(members, member)
	}
	return scope, members, nil
}
func (c *fleetProfileController) Put(ctx context.Context, data json.RawMessage) (any, error) {
	request, err := hostprofile.ParsePutRequest(data)
	if err != nil {
		return nil, fmt.Errorf("fleet profile request: %w", err)
	}
	scope, _, err := c.scope()
	if err != nil {
		return nil, err
	}
	from := c.id
	if peer, ok := transport.Peer(ctx); ok {
		from = peer.Identity
	}
	result, err := c.store.Receive(ctx, request.Document, from, scope)
	if err == nil && result.Outcome == hostprofile.Applied {
		c.signal()
	}
	if err != nil {
		return result, fmt.Errorf("receive fleet profile: %w", err)
	}
	return result, nil
}
func (c *fleetProfileController) signal() {
	select {
	case c.poke <- struct{}{}:
	default:
	}
}
func (c *fleetProfileController) HandleControl(ctx context.Context, request protocol.Control) (protocol.Control, bool, error) {
	if request.Type != protocol.TypeFleetProfileSync {
		return protocol.Control{}, false, nil
	}
	if _, remote := transport.Peer(ctx); remote {
		return protocol.Control{}, true, errors.New("fleet profile sync is local-only")
	}
	// A sync is a signal, not an alternate document or authority carrier.
	encoded, err := json.Marshal(request)
	if err != nil {
		return protocol.Control{}, true, fmt.Errorf("validate fleet sync: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return protocol.Control{}, true, fmt.Errorf("validate fleet sync: %w", err)
	}
	delete(fields, "type")
	delete(fields, "requestId")
	if len(fields) != 0 {
		return protocol.Control{}, true, errors.New("fleet profile sync accepts no payload")
	}
	c.signal()
	return protocol.Control{Type: protocol.TypeOK, RequestID: request.RequestID}, true, nil
}
func (c *fleetProfileController) Run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		if err := c.cycle(ctx); err != nil && ctx.Err() == nil && c.report != nil {
			c.report(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-c.poke:
			clear(c.retries)
		case <-c.after(fleetPollInterval):
		}
	}
}
func (c *fleetProfileController) cycle(ctx context.Context) error {
	snapshot, err := c.store.ReadReplica(c.trusted)
	if err != nil {
		return fmt.Errorf("reconcile fleet profile delivery: %w", err)
	}
	if snapshot.State != hostprofile.StateValid {
		return nil
	}
	scope, hosts, err := c.scope()
	if err != nil {
		return fmt.Errorf("reconcile fleet profile delivery: %w", err)
	}
	result, err := c.store.Reconcile(ctx, scope)
	if err != nil {
		return fmt.Errorf("reconcile fleet profile delivery: %w", err)
	}
	// Reconciliation may have observed a newer publication than the initial read.
	snapshot, err = c.store.ReadReplica(c.trusted)
	if err != nil {
		return fmt.Errorf("reconcile fleet profile delivery: %w", err)
	}
	if snapshot.State != hostprofile.StateValid || snapshot.Value.Digest() != result.Digest {
		return nil
	}
	c.deliver(ctx, *snapshot.Value, result.Outbox, scope, hosts)
	return nil
}
func (c *fleetProfileController) deliver(ctx context.Context, doc hostprofile.FleetDocument, out hostprofile.Outbox, scope hostprofile.FleetScope, hosts []update.Host) {
	pending := map[string]bool{}
	for _, member := range out.Members {
		pending[member.HostID] = member.State == hostprofile.Pending
	}
	attempts := make(chan update.Host, len(hosts))
	for _, host := range hosts {
		retry := c.retries[host.ID]
		if !pending[host.ID] || (retry.digest == doc.Digest() && c.now().Before(retry.next)) {
			continue
		}
		attempts <- host
	}
	close(attempts)
	var joined sync.WaitGroup
	var mu sync.Mutex
	for range 8 {
		joined.Go(func() { c.deliverMembers(ctx, attempts, doc, scope, &mu) })
	}
	joined.Wait()
	c.pruneRetries(pending, doc.Digest())
}
func (c *fleetProfileController) deliverMembers(ctx context.Context, attempts <-chan update.Host, doc hostprofile.FleetDocument, scope hostprofile.FleetScope, mu *sync.Mutex) {
	for host := range attempts {
		if ctx.Err() != nil {
			return
		}
		c.attempt(ctx, host, doc, scope, mu)
	}
}
func (c *fleetProfileController) attempt(ctx context.Context, host update.Host, doc hostprofile.FleetDocument, scope hostprofile.FleetScope, mu *sync.Mutex) {
	attempt, cancel := context.WithTimeout(ctx, fleetAttemptTimeout)
	defer cancel()
	var result hostprofile.PutResult
	err := c.call(attempt, host, hostprofile.ActionFleetPut, hostprofile.PutRequest{Document: doc}, &result)
	if ctx.Err() != nil {
		return
	}
	outcome := fleetDeliveryOutcome(doc, result, err, c.trusted)
	settled, settleErr := c.store.Settle(ctx, doc.Digest(), host.ID, outcome, scope)
	// Adopting a newer Current queues a new document without passing through Put.
	if settled.Changed {
		c.signal()
	}
	mu.Lock()
	defer mu.Unlock()
	if settleErr != nil && c.report != nil {
		c.report(fmt.Errorf("settle fleet profile member %s: %w", host.ID, settleErr))
	}
	retry := c.retries[host.ID]
	if retry.digest != doc.Digest() {
		retry = fleetRetry{digest: doc.Digest()}
	}
	retry.delay = nextFleetDelay(retry.delay)
	retry.next = c.now().Add(c.jitter(retry.delay))
	c.retries[host.ID] = retry
	c.reportStale(settled.Outbox, host.ID)
}
func nextFleetDelay(previous time.Duration) time.Duration {
	if previous == 0 {
		return 30 * time.Second
	}
	return min(previous*2, 30*time.Minute)
}
func (c *fleetProfileController) reportStale(out hostprofile.Outbox, id string) {
	for _, member := range out.Members {
		if member.HostID != id || member.State != hostprofile.Stale || c.report == nil {
			continue
		}
		c.report(fmt.Errorf("fleet profile member %s is stale after a failed delivery beyond seven days", id))
	}
}
func (c *fleetProfileController) pruneRetries(pending map[string]bool, digest string) {
	for id, retry := range c.retries {
		if !pending[id] || retry.digest != digest {
			delete(c.retries, id)
		}
	}
}
func fleetDeliveryOutcome(doc hostprofile.FleetDocument, result hostprofile.PutResult, err error, trusted func(string) bool) hostprofile.Outcome {
	if err != nil {
		return fleetTransportFailure(err)
	}
	if err := result.Validate(doc); err != nil {
		return fleetFailedOutcome(hostprofile.DeliveryRejected, err)
	}
	if result.Current != nil && (trusted == nil || !trusted(result.Current.Author)) {
		return fleetFailedOutcome(hostprofile.DeliveryRejected, errors.New("returned fleet document author is not a current administrator"))
	}
	return hostprofile.Outcome{Result: &result}
}
func fleetTransportFailure(err error) hostprofile.Outcome {
	code := hostprofile.DeliveryUnreachable
	var remote *update.RemoteError
	var syntax *json.SyntaxError
	var shape *json.UnmarshalTypeError
	var invalid *hostprofile.ValidationError
	if errors.Is(err, transport.ErrAuthentication) {
		code = hostprofile.DeliveryAuthenticationFailed
	}
	if errors.As(err, &remote) || errors.As(err, &syntax) || errors.As(err, &shape) || errors.As(err, &invalid) {
		code = hostprofile.DeliveryRejected
	}
	return fleetFailedOutcome(code, err)
}
func fleetFailedOutcome(code hostprofile.DeliveryCode, err error) hostprofile.Outcome {
	return hostprofile.Outcome{Error: &hostprofile.DeliveryError{Code: code, Message: err.Error(), Retryable: true}}
}

func fleetMembershipPath(configDir string) string {
	if configDir == "" {
		return ""
	}
	return filepath.Join(configDir, "fleet.json")
}

func fleetJitter(delay time.Duration) time.Duration {
	n, err := rand.Int(rand.Reader, big.NewInt(4001))
	if err != nil {
		return delay
	}
	return delay * time.Duration(8000+n.Int64()) / 10000
}
