package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/shaul/mesh/internal/domainpolicy"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type configurationFileFixture struct {
	value  FeedValue
	writes int
	fail   error
}

func (f *configurationFileFixture) Value() FeedValue       { return f.value }
func (f *configurationFileFixture) Save(v FeedValue) error { f.value = v; f.writes++; return f.fail }
func (f *configurationFileFixture) Settle() error          { return f.fail }
func (f *configurationFileFixture) writer(_ context.Context, operation func(FeedFile) error) error {
	return operation(f)
}
func configurationFixture(t *testing.T, count int) (*Coordinator, VerifiedActor, *configurationFileFixture) {
	t.Helper()
	fleet := testFleet(t, count)
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	actor := VerifiedActor{id: fleet.Members[count-1].ID, local: true}
	c := &Coordinator{ID: actor.id, Store: store, SavedFleet: func() (Fleet, error) { return fleet, nil }, Trusted: func(string) bool { return true }}
	file := &configurationFileFixture{}
	return c, actor, file
}
func configurationProposalFixture(t *testing.T, c *Coordinator, actor VerifiedActor) ConfigurationApproval {
	t.Helper()
	fleet, err := c.SavedFleet()
	if err != nil {
		t.Fatal(err)
	}
	p := ConfigurationProposal{ID: mustAutomaticID(t), CoordinatorID: c.ID, OwnerID: actor.id, Fleet: fleet, Membership: fleet.Digest()}
	for _, host := range fleet.Members {
		v := FeedValue{}
		p.Recipients = append(p.Recipients, RecipientConfiguration{Host: host, GrantRevision: 1, Feed: &FeedChange{}, Expected: &ComponentExpectation{Value: v, Digest: v.Digest()}})
	}
	p.Digest = p.approvalDigest()
	if err := c.saveConfigurationPreview(t.Context(), actor, p); err != nil {
		t.Fatal(err)
	}
	return ConfigurationApproval{mustAutomaticID(t), p}
}
func TestConfigurationApproverCASReplayAndCurrentTrust(t *testing.T) {
	c, actor, _ := configurationFixture(t, 1)
	approval := configurationProposalFixture(t, c, actor)
	first, err := c.ApproveConfiguration(t.Context(), actor, approval)
	if err != nil {
		t.Fatal(err)
	}
	again, err := c.ApproveConfiguration(t.Context(), actor, approval)
	if err != nil || again.Revision != first.Revision {
		t.Fatalf("replay: %+v %v", again, err)
	}
	other, _ := testIdentity(t)
	if _, err = c.ApproveConfiguration(t.Context(), VerifiedActor{id: other}, approval); err == nil {
		t.Fatal("another approver adopted preview")
	}
	for _, action := range []string{"retry", configurationCancel} {
		_, err = c.ChangeConfiguration(t.Context(), VerifiedActor{id: other}, action, ConfigurationCommand{ID: mustAutomaticID(t), ChangeID: approval.Proposal.ID, ExpectedRevision: first.Revision})
		if err == nil {
			t.Fatal("unowned mutation", action)
		}
	}
	c.Trusted = func(string) bool { return false }
	if _, err = c.ApproveConfiguration(t.Context(), actor, approval); err == nil {
		t.Fatal("withdrawn approver received replay success")
	}
	c.Trusted = func(string) bool { return true }
	changed := approval
	changed.Proposal.Recipients[0].Feed.URL = "https://unapproved.example/"
	changed.Proposal.Digest = changed.Proposal.approvalDigest()
	if _, err = c.ApproveConfiguration(t.Context(), actor, changed); err == nil {
		t.Fatal("same proposal identity accepted changed bytes")
	}
}
func TestConfigurationCompetingInitialApprovals(t *testing.T) {
	c, actor, _ := configurationFixture(t, 1)
	a := configurationProposalFixture(t, c, actor)
	other, _ := testIdentity(t)
	b := configurationProposalFixture(t, c, VerifiedActor{id: other})
	var passed atomic.Int32
	var wg sync.WaitGroup
	for _, entry := range []struct {
		actor    VerifiedActor
		approval ConfigurationApproval
	}{{actor, a}, {VerifiedActor{id: other}, b}} {
		wg.Go(func() { approveConfigurationFixture(t.Context(), c, entry.actor, entry.approval, &passed) })
	}
	wg.Wait()
	if passed.Load() != 1 {
		t.Fatalf("approved %d competing decisions", passed.Load())
	}
}
func TestConfigurationReceiptIntentCASRecoveryAndGrantFence(t *testing.T) {
	c, actor, file := configurationFixture(t, 1)
	host := actor.id
	grant, err := c.Store.SetConfigurationGrant(t.Context(), 0, host, host, true, false)
	if err != nil {
		t.Fatal(err)
	}
	approval := configurationProposalFixture(t, c, actor)
	request := ConfigurationPut{approval.Proposal.ID, approval.Proposal.Digest, approval.Proposal.Membership, c.ID, approval.Proposal.Recipients[0]}
	file.fail = errors.New("directory settlement failed")
	if _, err = c.Store.PutConfiguration(t.Context(), actor, host, c.Trusted, file.writer, request); !errors.Is(err, file.fail) {
		t.Fatal(err)
	}
	intent, err := c.Store.configurationReceipt()
	if err != nil || intent.State != configurationPrepared || intent.Revision != 1 {
		t.Fatalf("intent: %+v %v", intent, err)
	}
	if file.value != (FeedValue{Present: true}) || file.writes != 1 {
		t.Fatal("file publication did not happen after intent")
	}
	file.fail = nil
	saved, err := c.Store.PutConfiguration(t.Context(), actor, host, c.Trusted, file.writer, request)
	if err != nil || saved.State != configurationSaved || saved.Revision != 1 || file.writes != 1 {
		t.Fatalf("same-value recovery: %+v %v writes=%d", saved, err, file.writes)
	}
	if _, err = c.Store.SetConfigurationGrant(t.Context(), grant.Revision, host, "", false, false); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Store.PutConfiguration(t.Context(), actor, host, c.Trusted, file.writer, request); err == nil {
		t.Fatal("revoked grant acknowledged matching replay")
	}
	if file.writes != 1 {
		t.Fatal("replay changed bytes")
	}
}
func TestConfigurationSelectedEditAndThirdValueSurvive(t *testing.T) {
	c, actor, file := configurationFixture(t, 1)
	if _, err := c.Store.SetConfigurationGrant(t.Context(), 0, actor.id, actor.id, true, false); err != nil {
		t.Fatal(err)
	}
	approval := configurationProposalFixture(t, c, actor)
	request := ConfigurationPut{approval.Proposal.ID, approval.Proposal.Digest, approval.Proposal.Membership, c.ID, approval.Proposal.Recipients[0]}
	file.value = FeedValue{Present: true, URL: "http://localhost/local"}
	if _, err := c.Store.PutConfiguration(t.Context(), actor, actor.id, c.Trusted, file.writer, request); err == nil {
		t.Fatal("local edit overwritten")
	}
	if file.writes != 0 {
		t.Fatal("CAS conflict wrote bytes")
	}
	file.value = FeedValue{}
	file.fail = errors.New("write uncertain")
	if _, err := c.Store.PutConfiguration(t.Context(), actor, actor.id, c.Trusted, file.writer, request); err == nil {
		t.Fatal("injected write succeeded")
	}
	file.value = FeedValue{Present: true, URL: "http://localhost/third"}
	file.fail = nil
	receipt, err := c.Store.PutConfiguration(t.Context(), actor, actor.id, c.Trusted, file.writer, request)
	if err != nil || receipt.State != configurationConflict || file.value.URL != "http://localhost/third" {
		t.Fatalf("third value: %+v %v", receipt, err)
	}
}
func TestConfigurationPublicationBarrierCannotBecomeReplaySuccess(t *testing.T) {
	c, actor, _ := configurationFixture(t, 1)
	approval := configurationProposalFixture(t, c, actor)
	syncReal := syncOperationDirectory
	t.Cleanup(func() { syncOperationDirectory = syncReal })
	fault := errors.New("injected directory barrier")
	syncOperationDirectory = func(path string) error {
		if path == c.Store.directory {
			return fault
		}
		return syncReal(path)
	}
	if _, err := c.ApproveConfiguration(t.Context(), actor, approval); !errors.Is(err, fault) {
		t.Fatal(err)
	}
	if _, err := c.ApproveConfiguration(t.Context(), actor, approval); !errors.Is(err, fault) {
		t.Fatal("unconfirmed replay", err)
	}
	syncOperationDirectory = syncReal
	if _, err := c.ApproveConfiguration(t.Context(), actor, approval); err != nil {
		t.Fatal(err)
	}
}
func TestConfigurationWithdrawalIsStickyAndLocalRetirementDoesNotTakeOwnership(t *testing.T) {
	c, actor, _ := configurationFixture(t, 1)
	owner, _ := testIdentity(t)
	approver := VerifiedActor{id: owner}
	approval := configurationProposalFixture(t, c, approver)
	status, err := c.ApproveConfiguration(t.Context(), approver, approval)
	if err != nil {
		t.Fatal(err)
	}
	c.Trusted = func(id string) bool { return id == c.ID }
	if err = c.stepConfiguration(t.Context()); err != nil {
		t.Fatal(err)
	}
	status, err = c.Store.ConfigurationStatus(t.Context(), approval.Proposal.ID)
	if err != nil || status.Change.Blocked != "owner-withdrawn" || status.Change.Targets[0].Issued {
		t.Fatalf("withdrawal: %+v %v", status, err)
	}
	c.Trusted = func(string) bool { return true }
	if err = c.stepConfiguration(t.Context()); err != nil {
		t.Fatal(err)
	}
	status, _ = c.Store.ConfigurationStatus(t.Context(), approval.Proposal.ID)
	if status.Change.Targets[0].Issued {
		t.Fatal("restored trust resumed dispatch")
	}
	command := ConfigurationCommand{ID: mustAutomaticID(t), ChangeID: approval.Proposal.ID, ExpectedRevision: status.Revision, Retire: true}
	retired, err := c.ChangeConfiguration(t.Context(), actor, configurationCancel, command)
	if err != nil || !retired.Change.Retired || retired.Change.Proposal.OwnerID != owner {
		t.Fatalf("retirement: %+v %v", retired, err)
	}
	if _, err = os.Stat(filepath.Join(filepath.Dir(c.Store.directory), "configurations", approval.Proposal.ID+".json")); err != nil {
		t.Fatal(err)
	}
}

type blockedConfigurationCaller struct {
	started      chan string
	stop         chan struct{}
	active, peak atomic.Int32
}

func (b *blockedConfigurationCaller) Call(ctx context.Context, host Host, _ string, _ any, _ any) error {
	active := b.active.Add(1)
	defer b.active.Add(-1)
	for old := b.peak.Load(); active > old; old = b.peak.Load() {
		if b.peak.CompareAndSwap(old, active) {
			break
		}
	}
	select {
	case b.started <- host.ID:
	case <-ctx.Done():
		return fmt.Errorf("fixture setup cancelled: %w", ctx.Err())
	}
	select {
	case <-b.stop:
		return errors.New("fixture response lost")
	case <-ctx.Done():
		return fmt.Errorf("fixture cancelled: %w", ctx.Err())
	}
}
func TestConfigurationFiniteCallsCancelJoinAndLeaveUncertainty(t *testing.T) {
	c, actor, _ := configurationFixture(t, 6)
	approval := configurationProposalFixture(t, c, actor)
	status, err := c.ApproveConfiguration(t.Context(), actor, approval)
	if err != nil {
		t.Fatal(err)
	}
	remote := &blockedConfigurationCaller{started: make(chan string, 6), stop: make(chan struct{})}
	c.Remote = remote
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	t.Cleanup(c.joinConfiguration)
	if err = c.stepConfiguration(ctx); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		select {
		case <-remote.started:
		case <-time.After(time.Second):
			t.Fatal("bounded call did not start")
		}
	}
	if remote.peak.Load() != 4 {
		t.Fatal(remote.peak.Load())
	}
	if _, err = c.ChangeConfiguration(ctx, actor, configurationCancel, ConfigurationCommand{ID: mustAutomaticID(t), ChangeID: approval.Proposal.ID, ExpectedRevision: status.Revision}); err != nil {
		t.Fatal("cancel waited on network", err)
	}
	cancel()
	c.joinConfiguration()
	if remote.active.Load() != 0 {
		t.Fatal("call escaped join")
	}
	reopened, err := OpenStore(c.Store.stateDir())
	if err != nil {
		t.Fatal(err)
	}
	current, err := reopened.ConfigurationStatus(t.Context(), approval.Proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	issued := 0
	for _, target := range current.Change.Targets {
		if target.Issued {
			issued++
		} else if target.State != configurationCancelled {
			t.Fatal(target)
		}
	}
	if issued != 4 {
		t.Fatal("cancel discarded issued uncertainty", issued)
	}
}

func approveConfigurationFixture(ctx context.Context, c *Coordinator, actor VerifiedActor, approval ConfigurationApproval, passed *atomic.Int32) {
	if _, err := c.ApproveConfiguration(ctx, actor, approval); err == nil {
		passed.Add(1)
	}
}

func TestConfigurationRequiresExplicitSelectedFeed(t *testing.T) {
	for _, data := range []string{`{"targets":[]}`, `{"targets":[],"usageFeed":{}}`, `{"targets":[],"usageFeed":null}`, `{"targets":[],"usageFeed":{"url":null}}`, `{"targets":[],"usageFeed":{"url":"","headers":{}}}`} {
		var selection ConfigurationSelection
		if err := DecodeConfiguration([]byte(data), &selection); err == nil {
			t.Fatal("implicit or arbitrary feed selection accepted", data)
		}
	}
}
func TestConfigurationCancelPreservesLateIssuedAcceptance(t *testing.T) {
	c, actor, file := configurationFixture(t, 1)
	if _, err := c.Store.SetConfigurationGrant(t.Context(), 0, c.ID, c.ID, true, false); err != nil {
		t.Fatal(err)
	}
	approval := configurationProposalFixture(t, c, actor)
	status, err := c.ApproveConfiguration(t.Context(), actor, approval)
	if err != nil {
		t.Fatal(err)
	}
	dispatch, err := c.reserveConfiguration(t.Context())
	if err != nil || dispatch == nil {
		t.Fatal(err)
	}
	command := ConfigurationCommand{ID: mustAutomaticID(t), ChangeID: approval.Proposal.ID, ExpectedRevision: status.Revision}
	if _, err = c.ChangeConfiguration(t.Context(), actor, configurationCancel, command); err != nil {
		t.Fatal(err)
	}
	receipt, err := c.Store.PutConfiguration(t.Context(), actor, c.ID, c.Trusted, file.writer, dispatch.request)
	if err != nil || receipt.State != configurationSaved {
		t.Fatalf("previously issued acceptance was retracted: %+v %v", receipt, err)
	}
	if err = c.finishConfiguration(t.Context(), *dispatch, receipt, ConfigurationSnapshot{}, nil); err != nil {
		t.Fatal(err)
	}
	current, err := c.Store.ConfigurationStatus(t.Context(), approval.Proposal.ID)
	if err != nil || !current.Change.Cancelled || current.Change.Targets[0].Issued || current.Change.Targets[0].State != configurationSaved {
		t.Fatalf("late outcome discarded: %+v %v", current, err)
	}
}
func TestConfigurationSavedStatusRequiresActiveMatchingObserver(t *testing.T) {
	target := ConfigurationTarget{State: configurationSaved}
	desired := FeedChange{}
	if got := configurationConsumerState(target, &desired); got != "saved; no active dashboard observation" {
		t.Fatal(got)
	}
	target.Observers = []ConfigurationObservation{{Digest: (FeedValue{Present: true}).Digest(), State: "disabled"}}
	if got := configurationConsumerState(target, &desired); got != "loaded by active dashboard" {
		t.Fatal(got)
	}
	target.Observers[0].Digest = (FeedValue{}).Digest()
	if got := configurationConsumerState(target, &desired); got != "saved; dashboard reload pending" {
		t.Fatal(got)
	}
}

type configurationDomainFixture struct {
	value      DomainValue
	generation domainpolicy.CatalogToken
	fail       error
}

func (f *configurationDomainFixture) Value() DomainValue { return f.value }
func (f *configurationDomainFixture) Prepare(digest string) (domainpolicy.CatalogToken, error) {
	f.generation = domainpolicy.CatalogToken{Digest: digest, Generation: 2}
	return f.generation, nil
}
func (f *configurationDomainFixture) Save(value DomainValue) error { f.value = value; return f.fail }
func (f *configurationDomainFixture) Settle() error                { return f.fail }
func (f *configurationDomainFixture) writer(_ context.Context, operation func(DomainFile) error) error {
	return operation(f)
}

func TestConfigurationDomainReceiptRecoveryPartialAndFeedHistory(t *testing.T) {
	c, actor, feed := configurationFixture(t, 1)
	if _, err := c.Store.SetConfigurationGrant(t.Context(), 0, actor.id, actor.id, true, true); err != nil {
		t.Fatal(err)
	}
	approval := configurationProposalFixture(t, c, actor)
	feedRequest := ConfigurationPut{approval.Proposal.ID, approval.Proposal.Digest, approval.Proposal.Membership, c.ID, approval.Proposal.Recipients[0]}
	first, err := c.Store.PutConfiguration(t.Context(), actor, actor.id, c.Trusted, feed.writer, feedRequest)
	if err != nil || first.Revision != 1 {
		t.Fatal(first, err)
	}
	domain := &configurationDomainFixture{value: DomainValue{Present: true, Policy: domainpolicy.Current()}}
	expected := &DomainExpectation{Value: domain.value, Digest: domain.value.Digest()}
	recipient := RecipientConfiguration{Host: approval.Proposal.Recipients[0].Host, GrantRevision: 1, Domain: &DomainChange{Primary: "new.test", Aliases: domain.value.Policy.Domains()}, DomainExpected: expected}
	request := ConfigurationPut{mustAutomaticID(t), strings.Repeat("c", 64), approval.Proposal.Membership, c.ID, recipient}
	domain.fail = errors.New("domain directory barrier failed after rename")
	if _, err := c.Store.PutConfiguration(t.Context(), actor, actor.id, c.Trusted, feed.writer, request, domain.writer); !errors.Is(err, domain.fail) {
		t.Fatal(err)
	}
	intent, err := c.Store.configurationReceipt()
	if err != nil || intent.Domain.State != configurationPrepared || intent.Domain.Generation.Generation != 2 {
		t.Fatal(intent, err)
	}
	domain.fail = nil
	result, err := c.Store.PutConfiguration(t.Context(), actor, actor.id, c.Trusted, feed.writer, request, domain.writer)
	if err != nil || result.Domain.State != configurationSaved || result.Domain.Revision != 1 || result.Revision != 1 {
		t.Fatal(result, err)
	}
	if domain.value.Policy.LegacyCertificateDomain != expected.Value.Policy.LegacyCertificateDomain {
		t.Fatal("legacy certificate owner changed")
	}
	snapshot, err := c.Store.ConfigurationSnapshot(t.Context(), actor.id, feed.writer, nil, domain.writer)
	if err != nil || snapshot.Feed.Revision != 1 || snapshot.Domain.Revision != 1 {
		t.Fatal(snapshot, err)
	}
	// A later combined change retains the saved domain even when the feed CAS conflicts.
	request.ChangeID = mustAutomaticID(t)
	request.Recipient.DomainExpected = snapshot.Domain
	request.Recipient.Domain = &DomainChange{Primary: "next.test", Aliases: domain.value.Policy.Domains()}
	request.Recipient.Expected, request.Recipient.Feed = &snapshot.Feed, &FeedChange{}
	feed.value = FeedValue{Present: true, URL: "http://localhost/local-edit"}
	result, err = c.Store.PutConfiguration(t.Context(), actor, actor.id, c.Trusted, feed.writer, request, domain.writer)
	if err != nil || result.State != configurationConflict || result.Domain.State != configurationSaved || feed.value.URL != "http://localhost/local-edit" {
		t.Fatalf("partial domain/feed receipt: %+v %v", result, err)
	}
}

func TestConfigurationDomainConsumerUsesActualProcessObservations(t *testing.T) {
	before := DomainValue{Present: true, Policy: domainpolicy.Current()}
	desired := domainDesired(DomainChange{Primary: "new.test", Aliases: before.Policy.Domains()}, before)
	token := domainpolicy.CatalogToken{Digest: desired.Digest(), Generation: 3}
	recipient := RecipientConfiguration{Domain: &DomainChange{Primary: desired.Policy.Primary, Aliases: desired.Policy.Aliases}}
	target := ConfigurationTarget{State: configurationSaved, Receipt: &ConfigurationReceipt{Domain: &DomainReceipt{State: configurationSaved, Desired: desired, Generation: token}}, Daemon: &DomainObservation{Digest: before.Digest(), InstanceID: mustAutomaticID(t), ObservedAt: time.Now()}}
	if got := configurationDomainConsumer(target, recipient); got != "saved; daemon restart required" {
		t.Fatal(got)
	}
	target.Daemon.Digest = desired.Digest()
	target.Observers = []ConfigurationObservation{{DomainDigest: before.Digest(), CatalogGeneration: 2}}
	if got := configurationDomainConsumer(target, recipient); got != "daemon loaded; dashboard reopen required" {
		t.Fatal(got)
	}
	target.Observers[0].DomainDigest, target.Observers[0].CatalogGeneration = desired.Digest(), 3
	if got := configurationDomainConsumer(target, recipient); !strings.Contains(got, "service HTTPS verification unknown") {
		t.Fatal(got)
	}
}

func TestConfigurationGrantReportsBothSelectedGroupsAfterRevoke(t *testing.T) {
	c, actor, _ := configurationFixture(t, 1)
	granted, err := c.Store.SetConfigurationGrant(t.Context(), 0, actor.id, actor.id, false, true)
	if err != nil || !granted.DomainPolicy || granted.UsageFeed {
		t.Fatal(granted, err)
	}
	revoked, err := c.Store.SetConfigurationGrant(t.Context(), 1, actor.id, "", false, false)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(revoked)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["domainPolicy"]) != "false" || string(fields["usageFeed"]) != "false" {
		t.Fatalf("revoked groups are not explicit: %s", data)
	}
}

func TestConfigurationGrantReadOnlyAbsentRevokedMalformedAndContended(t *testing.T) {
	c, _, _ := configurationFixture(t, 1)
	grant, err := c.Store.ConfigurationGrant(t.Context())
	if err != nil || grant.Revision != 0 {
		t.Fatal(grant, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(c.Store.directory), "config-grant.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("read created grant", err)
	}
	for _, groups := range [][2]bool{{true, false}, {false, true}, {false, false}} {
		grant, err = c.Store.SetConfigurationGrant(t.Context(), grant.Revision, c.ID, c.ID, groups[0], groups[1])
		if err != nil {
			t.Fatal(err)
		}
		read, err := c.Store.ConfigurationGrant(t.Context())
		if err != nil || read != grant {
			t.Fatal(read, grant, err)
		}
	}
	unlock, err := c.Store.lockContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = c.Store.ConfigurationGrant(ctx)
	unlock()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read ignored lock: %v", err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(c.Store.directory), "config-grant.json"), []byte(`{"schema":1,"revision":9,"usageFeed":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Store.ConfigurationGrant(t.Context()); err == nil {
		t.Fatal("malformed grant became absence")
	}
}
