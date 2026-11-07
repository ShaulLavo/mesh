package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/transport"
)

type ProblemCode string

type automaticProblem struct {
	cause error
	code  ProblemCode
	text  string
}

func (p *automaticProblem) Unwrap() error         { return p.cause }
func (p *automaticProblem) Error() string         { return p.text }
func problem(code ProblemCode, text string) error { return &automaticProblem{code: code, text: text} }
func responseProblemCode(err error) ProblemCode {
	var specific *automaticProblem
	if errors.As(err, &specific) {
		return specific.code
	}
	return ""
}

// RequireAutomaticActor binds only the new authority boundary, not legacy relays.
func RequireAutomaticActor(ctx context.Context, actor VerifiedActor) error {
	if actor.id == "" {
		return errors.New("update: verified signed actor required")
	}
	if peer, remote := transport.Peer(ctx); remote && peer.Identity != actor.id {
		return problem("automatic.conflict", "automatic request signer differs from its transport peer")
	}
	return nil
}
func automaticAction(action string) bool { return strings.HasPrefix(action, "automatic.") }
func (c *Coordinator) refreshAutomatic(ctx context.Context) error {
	status, err := c.Store.ReadAutomatic(ctx)
	if err != nil {
		return err
	}
	if status.Record == nil {
		return nil
	}
	r := *status.Record
	if !r.Policy.Enabled && r.Current == nil {
		return nil
	}
	if err = c.checkAutomaticScope(ctx); err != nil {
		if stoppedAutomaticError(err) {
			return nil
		}
		return err
	}
	if !automaticAdmissionEligible(r) {
		return nil
	}
	manual, err := c.Store.overlappingManual(r.Policy.Fleet)
	if err != nil {
		return err
	}
	if manual != "" {
		return nil
	}
	if !c.lastAutomaticCheck.IsZero() && time.Since(c.lastAutomaticCheck) < 30*time.Second {
		return nil
	}
	c.lastAutomaticCheck = time.Now()
	observations := make([]MemberConsent, 0, len(r.Policy.Fleet.Members))
	for _, host := range r.Policy.Fleet.Members {
		observations = append(observations, c.observeConsent(ctx, host))
	}
	if err = c.saveEnrollment(ctx, r, observations); err != nil {
		return err
	}

	return c.acceptAutomaticObservation(ctx)
}
func (c *Coordinator) acceptAutomaticObservation(ctx context.Context) error {
	if c.CheckedRelease == nil {
		return nil
	}
	checked, err := c.CheckedRelease(ctx)
	if err != nil {
		return fmt.Errorf("update: checked release unavailable: %w", err)
	}
	if checked == nil {
		return nil
	}
	_, err = c.Store.AcceptCheckedRelease(ctx, *checked, c.automaticScope())
	return err
}

// reserveAutomatic publishes an exact dispatch intention before leaving the lock.
func (c *Coordinator) reserveAutomatic(ctx context.Context, id string, index int, kind string) (Run, bool, error) {
	unlock, err := c.Store.lockContext(ctx)
	if err != nil {
		return Run{}, false, err
	}
	defer unlock()
	r, err := c.Store.readAutomatic()
	if err != nil {
		return Run{}, false, err
	}
	if r == nil || r.Current == nil || r.Current.ID != id {
		return Run{}, false, problem("automatic.conflict", "automatic run is no longer current")
	}
	if err = c.Store.fenceAutomatic(r, c.automaticScope()); err != nil {
		return *r.Current, false, err
	}
	run := r.Current
	target := &run.Targets[index]
	if kind == "grant" && target.Grant {
		return *run, true, nil
	}
	if run.Cancel || run.Stopped || r.Stop != nil && r.Stop.Kind != stopRecovering || !r.Policy.Enabled && !activeContinuation(*r) {
		return *run, false, nil
	}
	issued, err := prepareAutomaticDispatch(r, index, kind)
	if err != nil || !issued {
		return *run, issued, err
	}

	return *run, true, c.Store.writeAutomatic(*r)
}
func reserveCanary(r *AutomaticRecord, index int) bool {
	target := r.Current.Targets[index]
	if target.InstallationID != "" {
		return false
	}
	if r.Canary != nil && r.Canary.State != canaryVerified {
		return r.Canary.HostID == target.Host.ID && r.Canary.Attempt == r.Attempt
	}
	if r.Canary != nil {
		return true
	}
	artifact, err := r.Current.Release.Artifact(target.Host.Platform)
	if err != nil {
		return false
	}
	r.Canary = &Canary{r.Current.ID, r.Attempt, target.Host.ID, artifact.BinarySHA256, "issued"}
	return true
}

func DecodeAutomatic(data json.RawMessage, value any) error { return decodeAuthority(data, value) }

func stoppedAutomaticError(err error) bool {
	var p *automaticProblem
	return errors.As(err, &p) && p.code == "automatic.stopped"
}

func (c *Coordinator) saveEnrollment(ctx context.Context, observed AutomaticRecord, observations []MemberConsent) error {
	unlock, err := c.Store.lockContext(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	current, err := c.Store.readAutomatic()
	if err != nil {
		return err
	}
	if current == nil || current.Policy.ID != observed.Policy.ID || current.Policy.Epoch != observed.Policy.Epoch || current.Policy.Membership != observed.Policy.Membership {
		return nil
	}
	current.Enrollment = observations
	return c.Store.writeAutomatic(*current)
}

func prepareAutomaticDispatch(r *AutomaticRecord, index int, kind string) (bool, error) {
	target := &r.Current.Targets[index]
	revision := consentFor(*r, target.Host.ID)
	if revision == 0 || target.ConsentRevision != 0 && target.ConsentRevision != revision {
		return false, problem("automatic.consent_revoked", "recipient consent changed; re-enroll explicitly")
	}
	target.ConsentRevision = revision
	switch kind {
	case "stage":
		target.StageAttempt = r.Attempt
	case automaticRetry:
		target.RetryAttempt = r.Attempt
	case "grant":
		if target.State != Staged || !canGrant(*r.Current, index) || !reserveCanary(r, index) {
			return false, nil
		}
		target.Grant = true
		target.State = Granted
	default:
		return false, errors.New("update: unknown automatic dispatch")
	}
	return true, nil
}

func unavailableAutomaticState(err error) error {
	return &automaticProblem{code: "state.unavailable", text: err.Error(), cause: err}
}

func automaticAdmissionEligible(r AutomaticRecord) bool {
	return r.Policy.Enabled && r.Stop == nil && (r.Current == nil || automaticSuccessful(r))
}
