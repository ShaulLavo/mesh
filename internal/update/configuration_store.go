package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/shaul/mesh/internal/domainpolicy"
	"golang.org/x/sys/unix"
	"io"
	"math"
	"os"
	"reflect"
	"slices"
	"sync"
	"time"
)

func (s *Store) readConfiguration() (ConfigurationRecord, error) {
	var record ConfigurationRecord
	err := s.readAuthority("configuration.json", &record)
	if errors.Is(err, os.ErrNotExist) {
		return ConfigurationRecord{Schema: 1}, nil
	}
	if err != nil {
		return record, err
	}
	if record.Schema != 1 || len(record.Previews) > 16 {
		return record, errors.New("configuration: invalid authority record")
	}
	for _, proposal := range record.Previews {
		if err := validateConfigurationProposal(proposal); err != nil {
			return record, err
		}
	}
	if record.Current != nil {
		if err := validateConfigurationChange(*record.Current); err != nil {
			return record, err
		}
	}
	return record, nil
}
func validateConfigurationChange(change ConfigurationChange) error {
	if err := validateConfigurationProposal(change.Proposal); err != nil {
		return err
	}
	if change.Attempt == 0 || len(change.Targets) != len(change.Proposal.Recipients) {
		return errors.New("configuration: invalid delivery progress")
	}
	for index, target := range change.Targets {
		if target.HostID != change.Proposal.Recipients[index].Host.ID || !slices.Contains([]string{configurationPending, configurationIssued, configurationUncertain, configurationSaved, configurationConflict, configurationRejected, configurationCancelled}, target.State) {
			return errors.New("configuration: target differs from approval")
		}
		if target.Issued && (!ValidRunID(target.DispatchID) || target.Attempt == 0 || target.Attempt > change.Attempt) {
			return errors.New("configuration: invalid dispatch intent")
		}

		if err := validateConfigurationTarget(change, index); err != nil {
			return err
		}
	}
	return nil
}
func (s *Store) writeConfiguration(record ConfigurationRecord) error {
	if record.Current != nil {
		if err := validateConfigurationChange(*record.Current); err != nil {
			return err
		}
	}
	return s.writeAuthority("configuration.json", record)
}
func (s *Store) ConfigurationStatus(ctx context.Context, id string) (ConfigurationStatus, error) {
	unlock, err := s.lockContext(ctx)
	if err != nil {
		return ConfigurationStatus{}, err
	}
	defer unlock()
	record, err := s.readConfiguration()
	if err != nil {
		return ConfigurationStatus{}, err
	}
	if id == "" || record.Current != nil && record.Current.Proposal.ID == id {
		return configurationStatus(record.Revision, record.Current), nil
	}
	if !ValidRunID(id) {
		return ConfigurationStatus{}, errors.New("configuration: invalid change identity")
	}
	var change ConfigurationChange
	if err = s.readConfigurationHistory(id, &change); err != nil {
		return ConfigurationStatus{}, err
	}
	if err = validateConfigurationChange(change); err != nil {
		return ConfigurationStatus{}, err
	}
	return configurationStatus(record.Revision, &change), nil
}
func (c *Coordinator) PreviewConfiguration(ctx context.Context, actor VerifiedActor, selection ConfigurationSelection) (ConfigurationProposal, error) {
	if err := requireConfigurationActor(ctx, actor, false, c.ID); err != nil {
		return ConfigurationProposal{}, err
	}
	fleet, err := c.configurationScope(actor)
	if err != nil {
		return ConfigurationProposal{}, err
	}
	if err := validateConfigurationSelection(selection); err != nil {
		return ConfigurationProposal{}, err
	}
	hosts, err := configurationSelection(fleet, selection.Targets)
	if err != nil {
		return ConfigurationProposal{}, err
	}
	status, err := c.Store.ConfigurationStatus(ctx, "")
	if err != nil {
		return ConfigurationProposal{}, err
	}
	id, err := newAutomaticID()
	if err != nil {
		return ConfigurationProposal{}, err
	}
	proposal := ConfigurationProposal{ID: id, CoordinatorID: c.ID, OwnerID: actor.id, ExpectedRevision: status.Revision, Fleet: fleet, Membership: fleet.Digest()}
	for _, host := range hosts {
		recipient, err := c.previewConfigurationRecipient(ctx, host, selection)
		if err != nil {
			return ConfigurationProposal{}, err
		}
		proposal.Recipients = append(proposal.Recipients, recipient)
	}
	proposal.Digest = proposal.approvalDigest()
	if err = validateConfigurationProposal(proposal); err != nil {
		return proposal, err
	}
	return proposal, c.saveConfigurationPreview(ctx, actor, proposal)
}
func configurationSelection(fleet Fleet, ids []string) ([]Host, error) {
	if len(ids) == 0 || len(ids) > MaximumHosts {
		return nil, problem("configuration.invalid", "explicit target identities required")
	}
	hosts := make([]Host, 0, len(ids))
	for _, id := range ids {
		index := slices.IndexFunc(fleet.Members, func(host Host) bool { return host.ID == id })
		if index < 0 || slices.ContainsFunc(hosts, func(host Host) bool { return host.ID == id }) {
			return nil, problem("configuration.invalid", "target is duplicated or absent from the saved fleet")
		}
		hosts = append(hosts, fleet.Members[index])
	}
	return hosts, nil
}
func (c *Coordinator) previewConfigurationRecipient(ctx context.Context, host Host, selection ConfigurationSelection) (RecipientConfiguration, error) {
	inspect, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var snapshot ConfigurationSnapshot
	if err := c.Remote.Call(inspect, host, "configuration.snapshot", nil, &snapshot); err != nil {
		return RecipientConfiguration{}, fmt.Errorf("configuration: preview recipient %s: %w", host.ID, err)
	}
	if snapshot.HostID != host.ID || snapshot.Grant.LocalHostID != host.ID || snapshot.Grant.CoordinatorID != c.ID || selection.UsageFeed != nil && !snapshot.Grant.UsageFeed || selection.Domain != nil && !snapshot.Grant.DomainPolicy {
		return RecipientConfiguration{}, problem("configuration.denied", "recipient has not granted the selected configuration groups to this coordinator")
	}
	recipient := RecipientConfiguration{Host: host, GrantRevision: snapshot.Grant.Revision, Feed: selection.UsageFeed, Domain: selection.Domain}
	if selection.UsageFeed != nil {
		recipient.Expected = &snapshot.Feed
	}
	if selection.Domain != nil {
		if snapshot.Domain == nil {
			return RecipientConfiguration{}, errors.New("configuration: domain snapshot unavailable")
		}
		recipient.DomainExpected = snapshot.Domain
	}
	return recipient, nil
}
func (c *Coordinator) saveConfigurationPreview(ctx context.Context, actor VerifiedActor, proposal ConfigurationProposal) error {
	unlock, err := c.Store.lockContext(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	fleet, err := c.configurationScope(actor)
	if err != nil {
		return err
	}
	record, err := c.Store.readConfiguration()
	if err != nil {
		return err
	}
	if fleet.Digest() != proposal.Membership || record.Revision != proposal.ExpectedRevision {
		return problem("configuration.conflict", "configuration authority or membership changed during preview")
	}
	if len(record.Previews) == 16 {
		record.Previews = record.Previews[1:]
	}
	record.Previews = append(record.Previews, proposal)
	return c.Store.writeConfiguration(record)
}
func (c *Coordinator) ApproveConfiguration(ctx context.Context, actor VerifiedActor, approval ConfigurationApproval) (ConfigurationStatus, error) {
	if err := requireConfigurationActor(ctx, actor, false, c.ID); err != nil {
		return ConfigurationStatus{}, err
	}
	if !ValidRunID(approval.CommandID) {
		return ConfigurationStatus{}, errors.New("configuration: administrative command identity required")
	}
	if err := validateConfigurationProposal(approval.Proposal); err != nil {
		return ConfigurationStatus{}, err
	}
	unlock, err := c.Store.lockContext(ctx)
	if err != nil {
		return ConfigurationStatus{}, err
	}
	defer unlock()
	fleet, err := c.configurationScope(actor)
	if err != nil {
		return ConfigurationStatus{}, err
	}
	p := approval.Proposal
	if p.CoordinatorID != c.ID || p.OwnerID != actor.id || p.Membership != fleet.Digest() {
		return ConfigurationStatus{}, problem("configuration.denied", "preview belongs to another approver, coordinator or saved membership")
	}
	record, err := c.Store.readConfiguration()
	if err != nil {
		return ConfigurationStatus{}, err
	}
	digest := configurationDigest(approval)
	if configurationReplay(record, actor, approval.CommandID, digest, p.ExpectedRevision) {
		return ConfigurationStatus{record.Revision, record.Current}, nil
	}
	if err = validateConfigurationApproval(record, p, approval.CommandID); err != nil {
		return ConfigurationStatus{}, err
	}
	if err = c.Store.archiveConfiguration(record.Current); err != nil {
		return ConfigurationStatus{}, err
	}
	change := &ConfigurationChange{Proposal: p, Attempt: 1}
	for _, recipient := range p.Recipients {
		change.Targets = append(change.Targets, ConfigurationTarget{HostID: recipient.Host.ID, State: configurationPending})
	}
	record.Current, record.Previews = change, nil
	record.Revision++
	record.LastMutation = &MutationReceipt{ID: approval.CommandID, ActorID: actor.id, Digest: digest, ExpectedRevision: p.ExpectedRevision, ResultRevision: record.Revision, Attempt: 1}
	return ConfigurationStatus{record.Revision, change}, c.Store.writeConfiguration(record)
}
func validateConfigurationApproval(record ConfigurationRecord, p ConfigurationProposal, commandID string) error {
	if record.LastMutation != nil && record.LastMutation.ID == commandID {
		return problem("configuration.conflict", "administrative command identity was already used")
	}
	if record.Revision != p.ExpectedRevision || record.Revision == math.MaxUint64 {
		return problem("configuration.conflict", "configuration administrative revision changed")
	}
	if record.Current != nil && !record.Current.settled() {
		return problem("configuration.conflict", "current configuration has unsettled work")
	}
	if !slices.ContainsFunc(record.Previews, func(preview ConfigurationProposal) bool { return preview.ID == p.ID && preview.Digest == p.Digest }) {
		return problem("configuration.conflict", "exact verified preview is unavailable; request a fresh preview")
	}
	return nil
}
func configurationReplay(r ConfigurationRecord, actor VerifiedActor, id, digest string, expected uint64) bool {
	m := r.LastMutation
	return m != nil && m.ID == id && m.ActorID == actor.id && m.Digest == digest && m.ExpectedRevision == expected && m.ResultRevision == r.Revision && r.Current != nil && m.Attempt == r.Current.Attempt
}
func (s *Store) archiveConfiguration(change *ConfigurationChange) error {
	if change == nil {
		return nil
	}
	if !change.settled() {
		return problem("configuration.conflict", "issued configuration outcomes remain uncertain")
	}
	return s.publishConfigurationHistory(*change)

}
func (c *Coordinator) ChangeConfiguration(ctx context.Context, actor VerifiedActor, action string, command ConfigurationCommand) (ConfigurationStatus, error) {
	if err := requireConfigurationActor(ctx, actor, command.Retire, c.ID); err != nil {
		return ConfigurationStatus{}, err
	}
	if err := validateConfigurationCommand(action, command); err != nil {
		return ConfigurationStatus{}, err
	}
	unlock, err := c.Store.lockContext(ctx)
	if err != nil {
		return ConfigurationStatus{}, err
	}
	defer unlock()
	if c.Trusted == nil || !c.Trusted(actor.id) {
		return ConfigurationStatus{}, problem("configuration.denied", "configuration actor is no longer admitted")
	}
	r, err := c.Store.readConfiguration()
	if err != nil {
		return ConfigurationStatus{}, err
	}
	if err = c.authorizeConfigurationCommand(r, actor, action, command); err != nil {
		return ConfigurationStatus{}, err
	}
	digest := configurationDigest(struct {
		Action  string
		Command ConfigurationCommand
	}{action, command})
	if configurationReplay(r, actor, command.ID, digest, command.ExpectedRevision) {
		return ConfigurationStatus{r.Revision, r.Current}, nil
	}
	if r.Revision != command.ExpectedRevision || r.Revision == math.MaxUint64 {
		return ConfigurationStatus{}, problem("configuration.conflict", "configuration administrative revision changed")
	}
	if r.LastMutation != nil && r.LastMutation.ID == command.ID {
		return ConfigurationStatus{}, problem("configuration.conflict", "administrative command identity was already used")
	}
	if err = c.mutateConfiguration(action, command, &r); err != nil {
		return ConfigurationStatus{}, err
	}
	r.Revision++
	r.LastMutation = &MutationReceipt{ID: command.ID, ActorID: actor.id, Digest: digest, ExpectedRevision: command.ExpectedRevision, ResultRevision: r.Revision, Attempt: r.Current.Attempt}
	return ConfigurationStatus{r.Revision, r.Current}, c.Store.writeConfiguration(r)
}
func (c *Coordinator) mutateConfiguration(action string, command ConfigurationCommand, r *ConfigurationRecord) error {
	if action == configurationRetry {
		if err := retryConfiguration(r.Current); err != nil {
			return err
		}
	} else {
		cancelConfiguration(r.Current)
	}
	if !command.Retire || !r.Current.settled() {
		return nil
	}
	r.Current.Retired = true
	return c.Store.archiveConfiguration(r.Current)
}
func (c *Coordinator) authorizeConfigurationCommand(r ConfigurationRecord, actor VerifiedActor, action string, command ConfigurationCommand) error {
	if r.Current == nil || r.Current.Proposal.ID != command.ChangeID {
		return problem("configuration.conflict", "configuration change is no longer current")
	}
	if actor.id != r.Current.Proposal.OwnerID && !command.Retire {
		return problem("configuration.denied", "only the recorded approver may retry or cancel this change")
	}
	if action == configurationRetry {
		return c.validateConfigurationRetry(actor, r.Current)
	}
	return nil
}
func cancelConfiguration(change *ConfigurationChange) {
	change.Cancelled = true
	for index := range change.Targets {
		target := &change.Targets[index]
		if !target.Issued && target.State != configurationSaved {
			target.State = configurationCancelled
		}
	}
}
func (c *Coordinator) validateConfigurationRetry(actor VerifiedActor, change *ConfigurationChange) error {
	fleet, err := c.configurationScope(actor)
	if err != nil {
		return err
	}
	if fleet.Digest() != change.Proposal.Membership {
		return problem("configuration.conflict", "saved membership changed; cancel and settle this change")
	}
	return nil
}
func retryConfiguration(change *ConfigurationChange) error {
	if change.Cancelled || change.Retired || change.Attempt == math.MaxUint64 {
		return problem("configuration.conflict", "cancelled configuration needs a fresh preview after settlement")
	}
	change.Attempt++
	change.Blocked = ""
	for index := range change.Targets {
		target := &change.Targets[index]
		if target.State == configurationSaved {
			continue
		}
		target.RetryAt, target.Problem = time.Time{}, ""
		if target.Issued {
			target.State = configurationIssued
		} else {
			target.State = configurationPending
		}
	}
	return nil
}

type configurationCalls struct {
	mu            sync.Mutex
	active        map[string]context.CancelFunc
	joined        sync.WaitGroup
	problem       error
	observationID string
	observeAfter  map[string]time.Time
}
type configurationDispatch struct {
	changeID, digest, dispatchID string
	recipient                    RecipientConfiguration
	request                      ConfigurationPut
	operation                    string
}

func (c *Coordinator) stepConfiguration(ctx context.Context) error {
	c.configurationCalls.mu.Lock()
	defer c.configurationCalls.mu.Unlock()
	if ctx.Err() != nil {
		return fmt.Errorf("configuration dispatch cancelled: %w", ctx.Err())
	}
	if c.configurationCalls.active == nil {
		c.configurationCalls.active = make(map[string]context.CancelFunc)
	}
	priorProblem := c.configurationCalls.problem
	c.configurationCalls.problem = nil
	for len(c.configurationCalls.active) < 4 {
		dispatch, err := c.reserveConfiguration(ctx)
		if err != nil || dispatch == nil {
			return errors.Join(priorProblem, err)
		}
		if c.Remote == nil {
			return errors.Join(priorProblem, errors.New("configuration: remote caller unavailable"))
		}
		key := dispatch.changeID + "/" + dispatch.recipient.Host.ID
		call, cancel := context.WithTimeout(ctx, 15*time.Second)
		c.configurationCalls.active[key] = cancel
		c.configurationCalls.joined.Add(1)
		go c.callConfiguration(call, cancel, key, *dispatch)
	}
	return priorProblem
}
func (c *Coordinator) reserveConfiguration(ctx context.Context) (*configurationDispatch, error) {
	unlock, err := c.Store.lockContext(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	r, err := c.Store.readConfiguration()
	if err != nil || r.Current == nil {
		return nil, err
	}
	change := r.Current
	if c.configurationCalls.observationID != change.Proposal.ID {
		c.configurationCalls.observationID = change.Proposal.ID
		c.configurationCalls.observeAfter = make(map[string]time.Time)
	}
	priorBlock := change.Blocked
	if err = c.fenceConfiguration(change); err != nil {
		return nil, err
	}
	for index := range change.Targets {
		dispatch, err := c.reserveConfigurationTarget(r, index, priorBlock)
		if err != nil || dispatch != nil {
			return dispatch, err
		}
	}
	return nil, c.writeConfigurationBlock(r, priorBlock)
}
func (c *Coordinator) reserveConfigurationTarget(r ConfigurationRecord, index int, priorBlock string) (*configurationDispatch, error) {
	change := r.Current
	target := &change.Targets[index]
	key := change.Proposal.ID + "/" + target.HostID
	if c.configurationCalls.active[key] != nil || time.Now().Before(c.configurationCalls.observeAfter[key]) || time.Now().Before(target.RetryAt) && target.State != configurationSaved && !savedDomainTarget(*target, change.Proposal.Recipients[index]) || target.State == configurationCancelled || target.State == configurationConflict && !savedDomainTarget(*target, change.Proposal.Recipients[index]) || target.State == configurationRejected {
		return nil, nil
	}
	dispatch, err := newConfigurationDispatch(change, index)
	if err != nil || dispatch == nil {
		return nil, err
	}
	if dispatch.operation == "observe" {
		c.configurationCalls.observeAfter[key] = time.Now().Add(30 * time.Second)
		return dispatch, c.writeConfigurationBlock(r, priorBlock)
	}
	target.RetryAt = time.Now().Add(30 * time.Second)
	if err = c.Store.writeConfiguration(r); err != nil {
		return nil, err
	}
	return dispatch, nil
}

func (c *Coordinator) writeConfigurationBlock(r ConfigurationRecord, priorBlock string) error {
	if r.Current.Blocked == priorBlock {
		return nil
	}
	return c.Store.writeConfiguration(r)
}
func (c *Coordinator) fenceConfiguration(change *ConfigurationChange) error {
	if change.Blocked != "" {
		return nil
	}
	if c.Trusted == nil || !c.Trusted(change.Proposal.OwnerID) {
		change.Blocked = "owner-withdrawn"
		return nil
	}
	if c.SavedFleet == nil {
		return errors.New("configuration: saved fleet unavailable")
	}
	fleet, err := c.SavedFleet()
	if err != nil || fleet.Validate() != nil || fleet.Digest() != change.Proposal.Membership {
		change.Blocked = "membership-changed"
	}
	return nil
}
func newConfigurationDispatch(change *ConfigurationChange, index int) (*configurationDispatch, error) {
	target := &change.Targets[index]
	observe := change.Cancelled || change.Blocked != "" || target.State == configurationSaved || savedDomainTarget(*target, change.Proposal.Recipients[index])
	operation := configurationPutOperation
	if target.State == configurationUncertain {
		operation = "reconcile"
	}
	if observe {
		operation = "observe"
	}
	if observe && !target.Issued && target.State != configurationSaved && !savedDomainTarget(*target, change.Proposal.Recipients[index]) {
		return nil, nil
	}
	if !observe && !target.Issued {
		id, err := newAutomaticID()
		if err != nil {
			return nil, err
		}
		target.DispatchID, target.Attempt, target.Issued, target.State = id, change.Attempt, true, configurationIssued
	}
	recipient := change.Proposal.Recipients[index]
	request := ConfigurationPut{change.Proposal.ID, change.Proposal.Digest, change.Proposal.Membership, change.Proposal.CoordinatorID, recipient}
	return &configurationDispatch{change.Proposal.ID, change.Proposal.Digest, target.DispatchID, recipient, request, operation}, nil
}
func (c *Coordinator) callConfiguration(ctx context.Context, cancel context.CancelFunc, key string, dispatch configurationDispatch) {
	defer c.configurationCalls.joined.Done()
	defer func() {
		cancel()
		c.configurationCalls.mu.Lock()
		delete(c.configurationCalls.active, key)
		c.configurationCalls.mu.Unlock()
	}()
	var snapshot ConfigurationSnapshot
	var receipt ConfigurationReceipt
	var err error
	if dispatch.operation != configurationPutOperation {
		err = c.Remote.Call(ctx, dispatch.recipient.Host, "configuration.snapshot", nil, &snapshot)
		if err == nil && dispatch.operation == "reconcile" && repairableConfigurationSnapshot(snapshot, dispatch) {
			err = c.Remote.Call(ctx, dispatch.recipient.Host, "configuration.put", dispatch.request, &receipt)
			dispatch.operation = configurationPutOperation
		}
	} else {
		err = c.Remote.Call(ctx, dispatch.recipient.Host, "configuration.put", dispatch.request, &receipt)
	}
	if ctx.Err() != nil {
		return
	}
	if err == nil && dispatch.recipient.Domain != nil {
		snapshot.Daemon = nil
		var info Info
		observe, stop := context.WithTimeout(ctx, 5*time.Second)
		defer stop()
		if observeErr := c.Remote.Call(observe, dispatch.recipient.Host, "info", nil, &info); observeErr == nil && info.Domain != nil {
			snapshot.Daemon = info.Domain
		}
	}
	settle, stop := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer stop()
	if finishErr := c.finishConfiguration(settle, dispatch, receipt, snapshot, err); finishErr != nil {
		c.configurationCalls.mu.Lock()
		c.configurationCalls.problem = errors.Join(c.configurationCalls.problem, finishErr)
		c.configurationCalls.mu.Unlock()
	}
}
func (c *Coordinator) finishConfiguration(ctx context.Context, dispatch configurationDispatch, receipt ConfigurationReceipt, snapshot ConfigurationSnapshot, callErr error) error {
	unlock, err := c.Store.lockContext(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	r, err := c.Store.readConfiguration()
	if err != nil || r.Current == nil {
		return err
	}
	target := configurationResultTarget(&r, dispatch)
	if target == nil {
		return nil
	}
	change := r.Current
	prior := *target
	if dispatch.recipient.Domain != nil {
		target.Daemon = snapshot.Daemon
	}
	if callErr != nil {
		configurationCallError(target, callErr, dispatch.operation != configurationPutOperation)
		return c.writeConfigurationTarget(r, prior, *target)
	}
	if dispatch.operation != configurationPutOperation {
		var completed bool
		receipt, completed, err = observeConfigurationSnapshot(target, dispatch, snapshot)
		if err != nil {
			return err
		}
		if completed {
			return c.writeConfigurationTarget(r, prior, *target)
		}
	}
	if !matchesConfigurationReceipt(receipt, dispatch, c.ID, change.Proposal.Membership) {
		return errors.New("configuration: receipt differs from dispatch")
	}
	target.Receipt = &receipt
	if receipt.State == configurationSaved || receipt.State == configurationConflict {
		target.State, target.Issued, target.Problem = receipt.State, false, receipt.Problem
	}
	return c.writeConfigurationTarget(r, prior, *target)
}
func (c *Coordinator) writeConfigurationTarget(r ConfigurationRecord, before, after ConfigurationTarget) error {
	if reflect.DeepEqual(before, after) {
		return nil
	}
	return c.Store.writeConfiguration(r)
}
func configurationCallError(target *ConfigurationTarget, err error, observation bool) {
	target.Problem = boundedProblem(err.Error())
	var remote *RemoteError
	if !observation && target.State != configurationUncertain && errors.As(err, &remote) && slices.Contains([]ProblemCode{"configuration.denied", "configuration.conflict", "configuration.invalid"}, remote.Code) {
		target.Issued, target.State = false, configurationRejected
		if remote.Code == "configuration.conflict" {
			target.State = configurationConflict
		}
	} else if target.Issued {
		target.State = configurationUncertain
	}
}
func (c *Coordinator) joinConfiguration() {
	c.configurationCalls.mu.Lock()
	for _, cancel := range c.configurationCalls.active {
		cancel()
	}
	c.configurationCalls.mu.Unlock()
	c.configurationCalls.joined.Wait()
}

func matchesConfigurationReceipt(receipt ConfigurationReceipt, dispatch configurationDispatch, coordinator, membership string) bool {
	recipient := dispatch.recipient
	if receipt.Schema != 1 || receipt.ChangeID != dispatch.changeID || receipt.ProposalDigest != dispatch.digest || receipt.HostID != recipient.Host.ID || receipt.GrantRevision != recipient.GrantRevision || receipt.CoordinatorID != coordinator || receipt.Membership != membership {
		return false
	}
	if recipient.Feed != nil && (recipient.Expected == nil || receipt.Expected != *recipient.Expected || receipt.Desired != (FeedValue{Present: true, URL: recipient.Feed.URL}) || receipt.Revision != recipient.Expected.Revision+1) {
		return false
	}
	if recipient.Domain == nil {
		return true
	}
	domain := receipt.Domain
	return domain != nil && configurationDigest(domain.Expected) == configurationDigest(recipient.DomainExpected) && configurationDigest(domain.Desired) == configurationDigest(domainDesired(*recipient.Domain, recipient.DomainExpected.Value)) && domain.Revision == recipient.DomainExpected.Revision+1
}

func configurationStatus(revision uint64, change *ConfigurationChange) ConfigurationStatus {
	if change == nil {
		return ConfigurationStatus{Revision: revision}
	}
	now := time.Now()
	for index := range change.Targets {
		target := &change.Targets[index]
		target.Observers = slices.DeleteFunc(target.Observers, func(o ConfigurationObservation) bool { return now.Sub(o.RenewedAt) >= ConfigurationObserverLease })
		recipient := change.Proposal.Recipients[index]
		target.Consumer = configurationConsumerState(*target, recipient.Feed)
		target.DomainConsumer = configurationDomainConsumer(*target, recipient)
	}
	return ConfigurationStatus{revision, change}
}
func configurationConsumerState(target ConfigurationTarget, feed *FeedChange) string {
	if feed == nil {
		return ""
	}
	if target.State != configurationSaved {
		return "publication pending"
	}
	if target.ObservedFeed != nil && target.ObservedFeed.Digest != (FeedValue{Present: true, URL: feed.URL}).Digest() {
		return "saved; selected usage feed changed locally"
	}
	if len(target.Observers) == 0 {
		return "saved; no active dashboard observation"
	}
	digest := (FeedValue{Present: true, URL: feed.URL}).Digest()
	failing := false
	for _, observer := range target.Observers {
		if observer.Digest != digest || observer.State == "invalid" {
			return "saved; dashboard reload pending"
		}
		failing = failing || observer.Failing
	}
	if failing {
		return "loaded; feed read failed"
	}
	return "loaded by active dashboard"
}

func validateConfigurationTarget(change ConfigurationChange, index int) error {
	target := change.Targets[index]
	if target.State == configurationSaved && (target.Receipt == nil || target.Receipt.State != configurationSaved || target.Issued) {
		return errors.New("configuration: saved target has no durable receipt")
	}
	if len(target.Observers) > 16 {
		return errors.New("configuration: observer limit exceeded")
	}
	if target.Receipt == nil {
		return nil
	}
	recipient := change.Proposal.Recipients[index]
	dispatch := configurationDispatch{changeID: change.Proposal.ID, digest: change.Proposal.Digest, recipient: recipient}
	if !matchesConfigurationReceipt(*target.Receipt, dispatch, change.Proposal.CoordinatorID, change.Proposal.Membership) {
		return errors.New("configuration: saved receipt differs from immutable approval")
	}
	return nil
}

func repairableConfigurationSnapshot(snapshot ConfigurationSnapshot, dispatch configurationDispatch) bool {
	grant := snapshot.Grant
	if snapshot.HostID != dispatch.recipient.Host.ID || (dispatch.recipient.Feed != nil && !grant.UsageFeed || dispatch.recipient.Domain != nil && !grant.DomainPolicy) || grant.Revision != dispatch.recipient.GrantRevision || grant.CoordinatorID != dispatch.request.CoordinatorID {
		return false
	}
	return snapshot.Receipt == nil || snapshot.Receipt.ChangeID != dispatch.changeID || snapshot.Receipt.State == configurationPrepared
}

func validateConfigurationCommand(action string, command ConfigurationCommand) error {
	if !ValidRunID(command.ID) || !ValidRunID(command.ChangeID) || action != configurationRetry && action != configurationCancel {
		return problem("configuration.invalid", "invalid configuration administrative command")
	}
	if command.Retire && action != configurationCancel {
		return problem("configuration.invalid", "local retirement can only cancel and archive")
	}
	return nil
}

func configurationResultTarget(r *ConfigurationRecord, dispatch configurationDispatch) *ConfigurationTarget {
	change := r.Current
	if change == nil || change.Proposal.ID != dispatch.changeID || change.Proposal.Digest != dispatch.digest {
		return nil
	}
	index := slices.IndexFunc(change.Targets, func(target ConfigurationTarget) bool {
		return target.HostID == dispatch.recipient.Host.ID && target.DispatchID == dispatch.dispatchID
	})
	if index < 0 {
		return nil
	}
	return &change.Targets[index]
}
func observeConfigurationSnapshot(target *ConfigurationTarget, dispatch configurationDispatch, snapshot ConfigurationSnapshot) (ConfigurationReceipt, bool, error) {
	if snapshot.HostID != target.HostID {
		return ConfigurationReceipt{}, true, errors.New("configuration: snapshot identity differs")
	}
	target.Observers, target.ObservedFeed, target.ObservedDomain, target.Daemon = snapshot.Observers, &snapshot.Feed, snapshot.Domain, snapshot.Daemon
	if target.Issued && snapshot.Grant.Revision != dispatch.recipient.GrantRevision {
		target.State, target.Issued, target.Problem = configurationRejected, false, "target-local grant changed; earlier selected bytes remain visible in the snapshot"
		if snapshot.Receipt != nil && snapshot.Receipt.ChangeID == dispatch.changeID {
			target.Receipt = snapshot.Receipt
		}
		return ConfigurationReceipt{}, true, nil
	}
	if snapshot.Receipt == nil || snapshot.Receipt.ChangeID != dispatch.changeID {
		return ConfigurationReceipt{}, true, nil
	}
	return *snapshot.Receipt, false, nil
}

func (s *Store) configurationHistory(create bool) (*os.File, error) {
	parent, err := s.authorityDirectory()
	if err != nil {
		return nil, err
	}
	defer func() { _ = parent.Close() }()
	if create {
		if err := unix.Mkdirat(int(parent.Fd()), "configurations", 0700); err != nil && !errors.Is(err, unix.EEXIST) {
			return nil, fmt.Errorf("configuration history directory: %w", err)
		}
	}
	fd, err := unix.Openat(int(parent.Fd()), "configurations", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open configuration history: %w", err)
	}
	file := os.NewFile(uintptr(fd), "configurations")
	if err = privateAuthority(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}
func (s *Store) publishConfigurationHistory(change ConfigurationChange) error {
	dir, err := s.configurationHistory(true)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	data, err := json.Marshal(change)
	if err != nil {
		return fmt.Errorf("encode configuration history: %w", err)
	}
	if len(data) > maximumMessage {
		return errors.New("configuration history exceeds size limit")
	}
	id, err := newAutomaticID()
	if err != nil {
		return err
	}
	name := ".configuration-" + id
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return fmt.Errorf("create configuration history: %w", err)
	}
	defer func() { _ = unix.Unlinkat(int(dir.Fd()), name, 0) }()
	if err = publishAuthority(os.NewFile(uintptr(fd), name), dir, name, change.Proposal.ID+".json", data); err != nil {
		return err
	}
	if err = dir.Sync(); err != nil {
		return fmt.Errorf("settle configuration history: %w", err)
	}
	return s.settlePublication(s.directory)
}
func (s *Store) readConfigurationHistory(id string, change *ConfigurationChange) error {
	dir, err := s.configurationHistory(false)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	fd, err := unix.Openat(int(dir.Fd()), id+".json", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open configuration history record: %w", err)
	}
	file := os.NewFile(uintptr(fd), id+".json")
	defer func() { _ = file.Close() }()
	if err = privateAuthority(file); err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(file, maximumMessage+1))
	if err != nil {
		return fmt.Errorf("read configuration history: %w", err)
	}
	if len(data) > maximumMessage {
		return errors.New("configuration history exceeds size limit")
	}
	if err = decodeAuthority(data, change); err != nil {
		return err
	}
	if err = dir.Sync(); err != nil {
		return fmt.Errorf("settle configuration history: %w", err)
	}
	return s.settlePublication(s.directory)
}

func configurationDomainConsumer(target ConfigurationTarget, recipient RecipientConfiguration) string {
	if recipient.Domain == nil {
		return ""
	}
	if target.Receipt == nil || target.Receipt.Domain == nil || target.Receipt.Domain.State != configurationSaved {
		return "domain publication pending"
	}
	domain := target.Receipt.Domain
	if target.ObservedDomain != nil && target.ObservedDomain.Digest != domain.Desired.Digest() {
		return "saved; selected domain policy changed locally"
	}
	if target.Daemon == nil || target.Daemon.InstanceID == "" || time.Since(target.Daemon.ObservedAt) > 45*time.Second {
		return "saved; daemon policy observation unavailable"
	}
	if target.Daemon.Digest != domain.Desired.Digest() {
		return "saved; daemon restart required"
	}
	if len(target.Observers) == 0 {
		return "daemon loaded; no active dashboard observation; service HTTPS verification unknown"
	}
	for _, observer := range target.Observers {
		if observer.DomainDigest != domain.Desired.Digest() || observer.CatalogGeneration != domain.Generation.Generation {
			return "daemon loaded; dashboard reopen required"
		}
	}
	return "loaded by daemon and active dashboards; service HTTPS verification unknown"
}

func savedDomainTarget(target ConfigurationTarget, recipient RecipientConfiguration) bool {
	return recipient.Domain != nil && target.Receipt != nil && target.Receipt.Domain != nil && target.Receipt.Domain.State == configurationSaved && target.Receipt.State != configurationPrepared
}

func validateConfigurationSelection(selection ConfigurationSelection) error {
	if selection.UsageFeed == nil && selection.Domain == nil {
		return problem("configuration.invalid", "explicit configuration component required")
	}
	if selection.UsageFeed == nil {
		return nil
	}
	policy := domainpolicy.Current()
	if selection.Domain != nil {
		policy = domainpolicy.Policy{Primary: selection.Domain.Primary, Aliases: selection.Domain.Aliases}
	}
	return validateDeliveredFeedPolicy(selection.UsageFeed.URL, policy)
}
