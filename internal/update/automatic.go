package update

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/updateinstall"
)

const (
	automaticEnable        = "enable"
	automaticRetry         = "retry"
	continuationActive     = "active"
	continuationSuperseded = "superseded"
	continuationCompleted  = "completed"
	stopRecovering         = "recovering"
	canaryVerified         = "verified"
)

type VerifiedActor struct {
	id    string
	local bool
}

func (a VerifiedActor) ID() string { return a.id }

type AutomaticOrigin struct {
	PolicyID   string `json:"policyId"`
	Epoch      uint64 `json:"epoch"`
	Membership string `json:"membership"`
}
type AutomaticPolicy struct {
	ID            string `json:"id"`
	Revision      uint64 `json:"revision"`
	OwnerID       string `json:"ownerId"`
	CoordinatorID string `json:"coordinatorId"`
	Epoch         uint64 `json:"epoch"`
	Fleet         Fleet  `json:"fleet"`
	Membership    string `json:"membership"`
	Enabled       bool   `json:"enabled"`
}
type AutomaticStop struct {
	ID      string    `json:"id"`
	RunID   string    `json:"runId"`
	Attempt uint64    `json:"attempt"`
	Kind    string    `json:"kind"`
	HostID  string    `json:"hostId,omitempty"`
	Problem string    `json:"problem"`
	At      time.Time `json:"at"`
}
type Canary struct {
	RunID          string `json:"runId"`
	Attempt        uint64 `json:"attempt"`
	HostID         string `json:"hostId"`
	ExpectedBinary string `json:"expectedBinary"`
	State          string `json:"state"`
}
type ManualContinuation struct {
	PolicyID       string `json:"policyId"`
	Epoch          uint64 `json:"epoch"`
	RunID          string `json:"runId"`
	Attempt        uint64 `json:"attempt"`
	ActorID        string `json:"actorId"`
	PolicyRevision uint64 `json:"policyRevision"`
	CommandID      string `json:"commandId"`
	State          string `json:"state"`
}
type MemberConsent struct {
	Consent    RecipientConsent `json:"consent"`
	ObservedAt time.Time        `json:"observedAt"`
	Problem    string           `json:"problem,omitempty"`
}
type CompletedFleetRelease struct {
	Version string    `json:"version"`
	Digest  string    `json:"digest"`
	At      time.Time `json:"at"`
}
type MutationReceipt struct {
	PolicyID         string         `json:"policyId,omitempty"`
	Stop             *AutomaticStop `json:"stop,omitempty"`
	ID               string         `json:"id"`
	ActorID          string         `json:"actorId"`
	Digest           string         `json:"digest"`
	ExpectedRevision uint64         `json:"expectedRevision"`
	ResultRevision   uint64         `json:"resultRevision"`
	Attempt          uint64         `json:"attempt"`
}
type AutomaticRecord struct {
	EnrollmentResetRequired bool                   `json:"enrollmentResetRequired,omitempty"`
	LastAcknowledgement     *MutationReceipt       `json:"lastAcknowledgement,omitempty"`
	Schema                  int                    `json:"schema"`
	Policy                  AutomaticPolicy        `json:"policy"`
	Stop                    *AutomaticStop         `json:"stop,omitempty"`
	Current                 *Run                   `json:"current,omitempty"`
	Attempt                 uint64                 `json:"attempt"`
	Canary                  *Canary                `json:"canary,omitempty"`
	Enrollment              []MemberConsent        `json:"enrollment,omitempty"`
	LastCompleted           *CompletedFleetRelease `json:"lastCompleted,omitempty"`
	LastMutation            *MutationReceipt       `json:"lastMutation,omitempty"`
	Continuation            *ManualContinuation    `json:"continuation,omitempty"`
}
type AutomaticStatus struct {
	Record           *AutomaticRecord `json:"record"`
	Revision         *uint64          `json:"revision"`
	State            string           `json:"state"`
	Problem          string           `json:"problem,omitempty"`
	TransportProblem string           `json:"transportProblem,omitempty"`
	SavedFleet       *Fleet           `json:"savedFleet,omitempty"`
	SavedMembership  string           `json:"savedMembership,omitempty"`
	PendingEpoch     uint64           `json:"pendingEpoch,omitempty"`
}
type AutomaticCommand struct {
	ExpectedMembership string `json:"expectedMembership,omitempty"`
	ID                 string `json:"id"`
	ExpectedRevision   uint64 `json:"expectedRevision"`
	Action             string `json:"action"`
	RunID              string `json:"runId,omitempty"`
	TakeOwnership      bool   `json:"takeOwnership,omitempty"`
}
type AdmissionScope struct {
	CoordinatorID string
	SavedFleet    func() (Fleet, error)
	Trusted       func(string) bool
}

func automaticStatus(record *AutomaticRecord) AutomaticStatus {
	revision := uint64(0)
	result := AutomaticStatus{Record: record, Revision: &revision, State: "not enrolled"}
	if record == nil {
		return result
	}
	revision = record.Policy.Revision
	result.State = automaticProgressState(*record)
	if record.Stop != nil {
		result.State = "stopped"
		result.Problem = record.Stop.Problem
	}
	if record.Stop != nil && record.Stop.Kind == stopRecovering {
		result.State = "recovering pinned run"
		if !record.Policy.Enabled && !activeContinuation(*record) {
			result.State = "stopped; recovery authority superseded"
		}
	}
	return result
}
func activeContinuation(r AutomaticRecord) bool {
	c := r.Continuation
	return c != nil && c.State == continuationActive && r.Current != nil && c.PolicyID == r.Policy.ID && c.Epoch == r.Policy.Epoch && c.RunID == r.Current.ID && c.Attempt == r.Attempt && (c.ActorID == r.Policy.OwnerID || c.ActorID == r.Policy.CoordinatorID)
}
func enrollmentReady(r AutomaticRecord) bool {
	if len(r.Enrollment) != len(r.Policy.Fleet.Members) {
		return false
	}
	for _, host := range r.Policy.Fleet.Members {
		if consentFor(r, host.ID) == 0 {
			return false
		}
	}
	return true
}
func consentFor(r AutomaticRecord, host string) uint64 {
	for _, member := range r.Enrollment {
		c := member.Consent
		if c.LocalHostID == host && c.Enabled && c.CoordinatorID == r.Policy.CoordinatorID && c.Membership == r.Policy.Membership && c.Epoch == r.Policy.Epoch && member.Problem == "" {
			return c.Revision
		}
	}
	return 0
}
func newAutomaticID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("update: allocate authority identity: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}
func commandDigest(command AutomaticCommand) string {
	data, _ := json.Marshal(command)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
func (c *Coordinator) automaticScope() AdmissionScope {
	return AdmissionScope{c.ID, c.SavedFleet, c.Trusted}
}
func (c *Coordinator) ChangeAutomatic(ctx context.Context, actor VerifiedActor, command AutomaticCommand) (AutomaticStatus, error) {
	return c.Store.changeAutomatic(ctx, actor, command, c.automaticScope())
}
func (s *Store) changeAutomatic(ctx context.Context, actor VerifiedActor, command AutomaticCommand, scope AdmissionScope) (AutomaticStatus, error) {
	unlock, err := s.lockContext(ctx)
	if err != nil {
		return AutomaticStatus{}, err
	}
	defer unlock()
	record, err := s.readAutomatic()
	if err != nil {
		return AutomaticStatus{}, err
	}
	if err = validateAutomaticActor(record, actor, command, scope); err != nil {
		return automaticStatus(record), err
	}
	if automaticReplay(record, actor, command) {
		return automaticStatus(record), s.settlePublication(s.directory)
	}
	if err = validateAutomaticCAS(record, command); err != nil {
		return automaticStatus(record), err
	}
	record, err = s.applyAutomaticDecision(record, actor, command, scope)
	if err != nil {
		return automaticStatus(record), err
	}
	return automaticStatus(record), s.writeAutomatic(*record)
}
func validateAutomaticCAS(record *AutomaticRecord, command AutomaticCommand) error {
	if record != nil && record.LastMutation != nil && record.LastMutation.ID == command.ID && record.LastMutation.Digest != commandDigest(command) {
		return problem("automatic.conflict", "automatic command identity was reused with different content")
	}
	expected := uint64(0)
	if record != nil {
		expected = record.Policy.Revision
	}
	if command.ExpectedRevision != expected || expected == math.MaxUint64 {
		return problem("automatic.conflict", "automatic policy revision changed; inspect automatic status")
	}
	return nil
}
func (s *Store) applyAutomaticDecision(record *AutomaticRecord, actor VerifiedActor, command AutomaticCommand, scope AdmissionScope) (*AutomaticRecord, error) {
	var acknowledged *AutomaticStop
	if record != nil && command.Action == "acknowledge-stop" {
		acknowledged = record.Stop
	}
	var err error
	if record == nil {
		record, err = initialAutomatic(scope, actor, command)
	} else {
		err = s.applyAutomatic(record, actor, command, scope)
	}
	if err != nil {
		return record, err
	}
	record.Policy.Revision++
	record.LastMutation = &MutationReceipt{ID: command.ID, ActorID: actor.id, Digest: commandDigest(command), ExpectedRevision: command.ExpectedRevision, ResultRevision: record.Policy.Revision, Attempt: record.Attempt, PolicyID: record.Policy.ID, Stop: acknowledged}
	if acknowledged != nil {
		record.LastAcknowledgement = record.LastMutation
	}
	if activeContinuation(*record) {
		record.Continuation.PolicyRevision = record.Policy.Revision
	}
	return record, nil
}

func automaticReplay(r *AutomaticRecord, actor VerifiedActor, cmd AutomaticCommand) bool {
	if r == nil || r.LastMutation == nil {
		return false
	}
	m := r.LastMutation
	if m.ID != cmd.ID || m.ActorID != actor.id || m.Digest != commandDigest(cmd) || m.ResultRevision != r.Policy.Revision {
		return false
	}
	if cmd.Action != automaticRetry {
		return true
	}
	return m.Attempt == r.Attempt && r.Stop != nil && r.Stop.Kind == stopRecovering && (r.Policy.Enabled || activeContinuation(*r))
}
func initialAutomatic(scope AdmissionScope, actor VerifiedActor, cmd AutomaticCommand) (*AutomaticRecord, error) {
	if cmd.Action != automaticEnable || cmd.TakeOwnership {
		return nil, errors.New("update: first automatic decision must enable the saved fleet")
	}
	fleet, err := readAutomaticFleet(scope)
	if err != nil {
		return nil, err
	}
	if cmd.ExpectedMembership != "" && cmd.ExpectedMembership != fleet.Digest() {
		return nil, problem("automatic.conflict", "saved fleet changed after approval preview")
	}
	id, err := newAutomaticID()
	if err != nil {
		return nil, err
	}
	return &AutomaticRecord{Schema: 1, Policy: AutomaticPolicy{ID: id, OwnerID: actor.id, CoordinatorID: scope.CoordinatorID, Epoch: 1, Fleet: fleet, Membership: fleet.Digest(), Enabled: true}}, nil
}
func readAutomaticFleet(scope AdmissionScope) (Fleet, error) {
	if scope.SavedFleet == nil {
		return Fleet{}, errors.New("update: saved fleet reader unavailable")
	}
	fleet, err := scope.SavedFleet()
	if err != nil {
		return Fleet{}, err
	}
	if err = fleet.Validate(); err != nil {
		return Fleet{}, err
	}
	if err = validateAutomaticPlatforms(fleet); err != nil {
		return Fleet{}, err
	}
	for _, host := range fleet.Members {
		if host.ID == scope.CoordinatorID {
			return fleet, nil
		}
	}
	return Fleet{}, errors.New("update: automatic coordinator must be in its saved fleet")
}
func (s *Store) applyAutomatic(r *AutomaticRecord, actor VerifiedActor, cmd AutomaticCommand, scope AdmissionScope) error {
	if cmd.TakeOwnership && (cmd.Action != automaticEnable || !actor.local || actor.id != scope.CoordinatorID || r.Policy.Enabled || r.Current != nil || r.Stop != nil) {
		return problem("automatic.conflict", "take ownership requires local disabled, acknowledged and settled policy")
	}
	switch cmd.Action {
	case automaticEnable:
		return enableAutomatic(r, actor, cmd, scope)
	case "disable":
		r.Policy.Enabled = false
		supersedeContinuation(r)
	case automaticRetry:
		return retryAutomatic(r, actor, cmd)
	case "cancel":
		if r.Current == nil || r.Current.ID != cmd.RunID {
			return problem("automatic.conflict", "automatic run is not current")
		}
		r.Policy.Enabled = false
		supersedeContinuation(r)
		r.Current.Cancel = true
		r.Current.Stopped = true
		stopAutomatic(r, "cancelled", "", "Automatic operation cancelled. Settle issued work, then acknowledge-stop before enabling again.")
	case "acknowledge-stop":
		return s.acknowledgeAutomatic(r, cmd)
	default:
		return errors.New("update: unknown automatic decision")
	}
	return nil
}
func enableAutomatic(r *AutomaticRecord, actor VerifiedActor, cmd AutomaticCommand, scope AdmissionScope) error {
	if r.Stop != nil {
		return problem("automatic.stopped", "automatic run is stopped; explicitly retry or cancel and acknowledge-stop")
	}
	fleet, err := readAutomaticFleet(scope)
	if err != nil {
		return err
	}
	if cmd.ExpectedMembership != "" && cmd.ExpectedMembership != fleet.Digest() {
		return problem("automatic.conflict", "saved fleet changed after approval preview")
	}
	changed := fleet.Digest() != r.Policy.Membership || cmd.TakeOwnership || r.EnrollmentResetRequired
	if changed && r.Current != nil {
		return problem("automatic.conflict", "cancel and settle the current run before changing enrollment")
	}
	if changed && r.Policy.Epoch == math.MaxUint64 {
		return errors.New("update: enrollment epoch exhausted")
	}
	if changed {
		r.Policy.Epoch++
		r.Policy.Fleet = fleet
		r.Policy.Membership = fleet.Digest()
		r.Enrollment = nil
		r.EnrollmentResetRequired = false
	}
	if cmd.TakeOwnership {
		r.Policy.OwnerID = actor.id
	}
	r.Policy.Enabled = true
	return nil
}
func supersedeContinuation(r *AutomaticRecord) {
	if r.Continuation != nil && r.Continuation.State == continuationActive {
		r.Continuation.State = continuationSuperseded
	}
}
func retryAutomatic(r *AutomaticRecord, actor VerifiedActor, cmd AutomaticCommand) error {
	if r.Current == nil || r.Current.ID != cmd.RunID || r.Current.Cancel {
		return problem("automatic.conflict", "retry requires the current uncancelled automatic run")
	}
	if r.Attempt == math.MaxUint64 {
		return errors.New("update: automatic attempt exhausted")
	}
	if err := retryRun(r.Current); err != nil {
		return err
	}
	r.Attempt++
	r.Canary = nil
	if r.Stop == nil {
		stopAutomatic(r, stopRecovering, "", "Explicit recovery of the pinned automatic run.")
	}
	r.Stop.Kind = stopRecovering
	r.Stop.Attempt = r.Attempt
	r.Current.Stopped = false
	if !r.Policy.Enabled {
		r.Continuation = &ManualContinuation{r.Policy.ID, r.Policy.Epoch, r.Current.ID, r.Attempt, actor.id, r.Policy.Revision + 1, cmd.ID, continuationActive}
	}
	return nil
}
func (s *Store) acknowledgeAutomatic(r *AutomaticRecord, cmd AutomaticCommand) error {
	if r.Current == nil {
		return s.acknowledgePendingPolicy(r, cmd)
	}
	if r.Current == nil || r.Current.ID != cmd.RunID || !r.Current.Cancel || !automaticSettled(*r.Current) || r.Stop == nil {
		return problem("automatic.conflict", "acknowledge-stop requires a cancelled automatic run with all issued and joined work settled")
	}
	if err := s.write(*r.Current); err != nil {
		return err
	}
	r.Current = nil
	r.Stop = nil
	r.Canary = nil
	r.Policy.Enabled = false
	supersedeContinuation(r)
	return nil
}
func automaticSettled(r Run) bool {
	for _, target := range r.Targets {
		if target.Grant || target.JoinedUnsettled || target.StageUnsettled || target.RetryUnsettled {
			return false
		}
		if !settled(target.State) && !(r.Cancel && target.State == Failed) {
			return false
		}
	}
	return true
}
func stopAutomatic(r *AutomaticRecord, kind, host, text string) {
	if r.Stop != nil && r.Stop.Kind != stopRecovering && kind != "cancelled" {
		return
	}
	id, err := newAutomaticID()
	if err != nil {
		id = r.Policy.ID + "-stop"
	}
	runID := ""
	if r.Current != nil {
		runID = r.Current.ID
		r.Current.Stopped = true
	}
	r.Stop = &AutomaticStop{id, runID, r.Attempt, kind, host, boundedProblem(text), time.Now().UTC()}
	supersedeContinuation(r)
}
func boundedProblem(text string) string {
	text = strings.Map(plainProblemRune, text)
	if len(text) <= 512 {
		return text
	}
	text = text[:512]
	for !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return text
}
func plainProblemRune(value rune) rune {
	if unicode.IsControl(value) {
		return ' '
	}
	return value
}
func automaticRequest(r Run, t Target) *updateinstall.AutomaticReference {
	if r.Origin == nil {
		return nil
	}
	return &updateinstall.AutomaticReference{PolicyID: r.Origin.PolicyID, CoordinatorID: r.Coordinator, Membership: r.Membership, Epoch: r.Origin.Epoch, ConsentRevision: t.ConsentRevision}
}

func NewAutomaticCommandID() (string, error) { return newAutomaticID() }

func validateAutomaticActor(record *AutomaticRecord, actor VerifiedActor, command AutomaticCommand, scope AdmissionScope) error {
	if actor.id == "" || scope.Trusted == nil || !scope.Trusted(actor.id) {
		return problem("automatic.conflict", "policy actor is no longer an update administrator")
	}
	if !ValidRunID(command.ID) {
		return errors.New("update: automatic command needs an exact command ID")
	}
	if record != nil && actor.id != record.Policy.OwnerID && !(actor.local && actor.id == scope.CoordinatorID) {
		return problem("automatic.conflict", "only the policy owner or local coordinator can change this policy")
	}
	return nil
}

func (c *Coordinator) AutomaticStatus(ctx context.Context) (AutomaticStatus, error) {
	status, err := c.Store.ReadAutomatic(ctx)
	if err != nil {
		return status, err
	}
	status = automaticFleetObservation(status, c.automaticScope())
	return c.automaticAdmissionObservation(ctx, status), nil
}
func automaticFleetObservation(status AutomaticStatus, scope AdmissionScope) AutomaticStatus {
	fleet, err := readAutomaticFleet(scope)
	if err != nil {
		status.Problem = err.Error()
		return status
	}
	status.SavedFleet = &fleet
	status.SavedMembership = fleet.Digest()
	status.PendingEpoch = 1
	if status.Record != nil {
		status.PendingEpoch = status.Record.Policy.Epoch
	}
	if status.Record != nil && status.Record.Policy.Membership != status.SavedMembership && status.PendingEpoch < math.MaxUint64 {
		status.PendingEpoch++
	}
	return status
}

func (s *Store) acknowledgePendingPolicy(r *AutomaticRecord, cmd AutomaticCommand) error {
	if cmd.RunID != r.Policy.ID || r.Policy.Enabled || r.Stop == nil || r.Stop.RunID != "" || r.Canary != nil {
		return problem("automatic.conflict", "policy acknowledgement requires an exact disabled stopped enrollment with no current run")
	}
	if r.Continuation != nil && r.Continuation.State == continuationActive {
		return problem("automatic.conflict", "policy still has executable continuation authority")
	}
	runs, err := s.List()
	if err != nil {
		return err
	}
	for _, run := range runs {
		if run.Origin != nil && run.Origin.PolicyID == r.Policy.ID && !automaticSettled(run) {
			return problem("automatic.conflict", "policy history still has unsettled issued or joined work")
		}
	}
	r.Stop = nil
	r.Policy.Enabled = false
	return nil
}

func validateAutomaticPlatforms(fleet Fleet) error {
	for _, host := range fleet.Members {
		if !automaticPlatform(host.Platform) {
			return errors.New("update: automatic fleet must pin each target's supported platform")
		}
	}
	return nil
}
func automaticPlatform(platform release.Platform) bool {
	return platform == (release.Platform{OS: "linux", Arch: "amd64"}) || platform == (release.Platform{OS: "linux", Arch: "arm64"}) || platform == (release.Platform{OS: "darwin", Arch: "arm64"})
}

func (c *Coordinator) automaticAdmissionObservation(ctx context.Context, status AutomaticStatus) AutomaticStatus {
	r := status.Record
	if r == nil || !r.Policy.Enabled || r.Stop != nil || r.Current != nil || !enrollmentReady(*r) {
		return status
	}
	manual, err := c.Store.overlappingManual(r.Policy.Fleet)
	if err != nil {
		status.Problem = err.Error()
		return status
	}
	if manual != "" {
		status.State = "waiting for manual operation"
		status.Problem = "Inspect overlapping manual operation " + manual + " with mesh update status."
		return status
	}
	if c.CheckedRelease == nil {
		return status
	}
	_, err = c.CheckedRelease(ctx)
	if err != nil {
		status.State = "waiting for a valid checked release"
		status.Problem = boundedProblem(err.Error())
	}
	return status
}

func automaticProgressState(record AutomaticRecord) string {
	state := "disabled"
	if record.Policy.Enabled {
		state = "enabled; waiting for a checked release"
	}
	if record.Policy.Enabled && !enrollmentReady(record) {
		state = "waiting for enrollment confirmation"
	}
	if record.Current != nil {
		state = "running"
	}
	if record.Current != nil && automaticSuccessful(record) {
		state = "complete; waiting for a newer checked release"
	}
	if !record.Policy.Enabled && record.Current != nil {
		state = "disabled; issued work may still finish"
	}
	if !record.Policy.Enabled && record.Current != nil && automaticSettled(*record.Current) {
		state = "disabled; pinned run settled"
	}
	if activeContinuation(record) {
		state = "explicit pinned continuation"
	}
	return state
}
