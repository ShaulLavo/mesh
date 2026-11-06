package hostprofile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"time"
)

const MaximumFleetMembers = 256
const FleetGiveUp = 7 * 24 * time.Hour

type DeliveryState string

const (
	Pending   DeliveryState = "pending"
	Delivered DeliveryState = "delivered"
	Diverged  DeliveryState = "conflict"
	Stale     DeliveryState = "stale"
)

type DeliveryError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}
type Delivery struct {
	HostID         string         `json:"hostId"`
	State          DeliveryState  `json:"state"`
	QueuedAt       time.Time      `json:"queuedAt"`
	SettledAt      *time.Time     `json:"settledAt,omitempty"`
	LastError      *DeliveryError `json:"lastError,omitempty"`
	MemberRevision uint64         `json:"memberRevision,omitempty"`
	MemberDigest   string         `json:"memberDigest,omitempty"`
}
type Outbox struct {
	Version  int        `json:"version"`
	Revision uint64     `json:"revision"`
	Digest   string     `json:"digest"`
	Members  []Delivery `json:"members"`
}

// FleetScope is a current local policy observation, never authority supplied by a peer.
type FleetScope struct {
	Members []string
	Trusted func(string) bool
}
type Outcome struct {
	Result *PutResult
	Error  *DeliveryError
}

func fleetMembers(members []string) ([]string, error) {
	if len(members) > MaximumFleetMembers {
		return nil, errors.New("fleet outbox exceeds 256 members")
	}
	result := slices.Clone(members)
	slices.Sort(result)
	for i, id := range result {
		key, err := baseIdentity(id)
		if err != nil || len(key) != 32 {
			return nil, errors.New("invalid fleet member identity")
		}
		if i > 0 && result[i-1] == id {
			return nil, errors.New("duplicate fleet member identity")
		}
	}
	return result, nil
}

func readOutbox(dir *os.File) (Outbox, error) {
	data, err := readPrivate(dir, "outbox.json", 256<<10)
	if err != nil {
		return Outbox{}, err
	}
	if err := strictObject(data, 256<<10, []string{"version", fleetRevisionField, "digest", "members"}); err != nil {
		return Outbox{}, err
	}
	var out Outbox
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&out); err != nil {
		return out, invalid("outbox", err.Error())
	}
	if out.Version != 1 || !validDigest(out.Digest) || out.Revision == 0 || len(out.Members) > MaximumFleetMembers {
		return out, invalid("outbox", "invalid metadata")
	}
	previous := ""
	for _, member := range out.Members {
		if err := validateDelivery(member, previous); err != nil {
			return Outbox{}, err
		}
		previous = member.HostID
	}
	return out, nil
}
func validateDelivery(d Delivery, previous string) error {
	key, err := baseIdentity(d.HostID)
	if err != nil || len(key) != 32 || d.HostID <= previous || d.QueuedAt.IsZero() {
		return invalid("outbox", "invalid member identity, order or age")
	}
	switch d.State {
	case Pending, Delivered, Diverged, Stale:
	default:
		return invalid("outbox", "unknown member state")
	}
	if d.LastError != nil && (d.LastError.Code == "" || len(d.LastError.Code) > 64 || validateText("delivery error", d.LastError.Message, 512, false) != nil) {
		return invalid("outbox", "invalid delivery error")
	}
	return nil
}
func reconcileOutbox(current Outbox, doc FleetDocument, members []string, now time.Time) Outbox {
	out := Outbox{Version: 1, Revision: doc.Revision, Digest: doc.Digest(), Members: make([]Delivery, 0, len(members))}
	prior := map[string]Delivery{}
	if current.Digest == out.Digest && current.Revision == out.Revision {
		for _, member := range current.Members {
			prior[member.HostID] = member
		}
	}
	for _, id := range members {
		member, ok := prior[id]
		if !ok {
			member = Delivery{HostID: id, State: Pending, QueuedAt: now.UTC()}
		}
		out.Members = append(out.Members, member)
	}
	return out
}
func sameOutbox(a, b Outbox) bool {
	first, _ := json.Marshal(a)
	second, _ := json.Marshal(b)
	return string(first) == string(second)
}
func (s *FleetStore) saveOutbox(dir *os.File, out Outbox) error {
	data, err := json.Marshal(out)
	if err != nil {
		return fmt.Errorf("encode fleet outbox: %w", err)
	}
	_, err = s.publishFleetFile(dir, "outbox.json", data)
	return err
}

func (s *FleetStore) Settle(ctx context.Context, digest, member string, outcome Outcome, scope FleetScope) (FleetWrite, error) {
	return s.locked(ctx, func(dir *os.File) (FleetWrite, error) { return s.settleDelivery(dir, digest, member, outcome, scope) })
}

func (s *FleetStore) settleDelivery(dir *os.File, digest, member string, outcome Outcome, scope FleetScope) (FleetWrite, error) {
	// Fence before recovery or adoption. No late response can operate on a newer document.
	snapshot, err := readReplica(dir, scope.Trusted)
	if err != nil {
		return FleetWrite{Publication: BeforeReplicaRename}, err
	}
	if snapshot.Value == nil || snapshot.Value.Digest() != digest {
		return FleetWrite{Publication: BeforeReplicaRename}, nil
	}
	snapshot, result, err := s.recover(dir, scope)
	if err != nil {
		return result, err
	}
	if snapshot.State != StateValid {
		return result, errors.New("cannot settle delivery for invalid or revoked fleet document")
	}
	if outcome.Result != nil {
		if err := outcome.Result.Validate(*snapshot.Value); err != nil {
			return result, err
		}
	}
	if outcome.Result != nil && outcome.Result.Outcome == Older {
		return s.adoptCurrent(dir, result, member, *outcome.Result.Current, scope)
	}
	return s.settleMember(dir, result, member, outcome)
}
func (s *FleetStore) adoptCurrent(dir *os.File, result FleetWrite, member string, doc FleetDocument, scope FleetScope) (FleetWrite, error) {
	if err := verifyTrusted(doc, scope.Trusted); err != nil {
		return result, err
	}
	next, err := s.publishDocument(dir, doc, scope)
	if err != nil {
		return next, err
	}
	return s.markSender(dir, next, member)
}
func (s *FleetStore) settleMember(dir *os.File, result FleetWrite, id string, outcome Outcome) (FleetWrite, error) {
	out := result.Outbox
	out.Members = slices.Clone(result.Outbox.Members)
	for i := range out.Members {
		if out.Members[i].HostID != id || out.Members[i].State != Pending {
			continue
		}
		next, err := deliveryTransition(out.Members[i], outcome, s.now().UTC())
		if err != nil {
			return result, err
		}
		out.Members[i] = next
	}
	if sameOutbox(out, result.Outbox) {
		return result, nil
	}
	if err := s.saveOutbox(dir, out); err != nil {
		result.Publication = ReplicaDurable
		return result, err
	}
	result.Outbox = out
	result.Publication = PublicationComplete
	return result, nil
}
func deliveryTransition(member Delivery, outcome Outcome, now time.Time) (Delivery, error) {
	if outcome.Error != nil {
		return failedDelivery(member, *outcome.Error, now), nil
	}
	if outcome.Result == nil {
		return member, errors.New("delivery has no result or error")
	}
	result := outcome.Result
	switch result.Outcome {
	case Applied, Unchanged:
		member.State, member.SettledAt, member.LastError = Delivered, &now, nil
	case Conflict:
		member.State, member.SettledAt = Diverged, &now
		member.MemberRevision, member.MemberDigest = result.Revision, result.Digest
		member.LastError = &DeliveryError{Code: "fleet.revision_conflict", Message: "The member holds a different fleet document at the same revision."}
	case Untrusted:
		member = failedDelivery(member, DeliveryError{Code: "fleet.signer_untrusted", Message: "The member does not trust the document author to publish fleet notes.", Retryable: true}, now)
	case Older:
		return member, errors.New("newer fleet Current must be adopted before member settlement")
	default:
		return member, errors.New("unexpected fleet delivery outcome")
	}
	return member, nil
}
func failedDelivery(member Delivery, problem DeliveryError, now time.Time) Delivery {
	if !now.Before(member.QueuedAt.Add(FleetGiveUp)) {
		member.State, member.SettledAt = Stale, &now
		problem = DeliveryError{Code: "fleet.stale", Message: "Delivery failed after seven days. Run mesh profile status --fleet --retry --json to try again."}
	}
	if member.LastError == nil || member.LastError.Code != problem.Code {
		problem.Message = problemText(problem.Message)
		member.LastError = &problem
	}
	return member
}
