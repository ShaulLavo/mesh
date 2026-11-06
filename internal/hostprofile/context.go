package hostprofile

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

type Completeness string

const (
	Summary Completeness = "summary"
	Full    Completeness = "full"
)

type ContextRequest struct {
	Full bool `json:"full"`
}

type ContextReply struct {
	Host  WireSection `json:"host"`
	Fleet WireSection `json:"fleet"`
}

type WireSection struct {
	State             ProfileState `json:"state"`
	Completeness      Completeness `json:"completeness"`
	Problem           string       `json:"problem,omitempty"`
	Revision          *uint64      `json:"revision"`
	Digest            string       `json:"digest,omitempty"`
	UpdatedAt         *time.Time   `json:"updatedAt,omitempty"`
	Author            string       `json:"author,omitempty"`
	Fleet             string       `json:"fleet,omitempty"`
	Signature         string       `json:"signature,omitempty"`
	Profile           *Editable    `json:"profile,omitempty"`
	InstructionsBytes int          `json:"instructionsBytes"`
}

func ParseContextRequest(data []byte) (ContextRequest, error) {
	var request ContextRequest
	if err := strictObject(data, 64, []string{"full"}); err != nil {
		return request, err
	}
	if err := json.Unmarshal(data, &request); err != nil {
		return request, invalid("context request", err.Error())
	}
	return request, nil
}

func ReadContext(configDir, stateDir, hostID string, trusted func(string) bool, full bool) ContextReply {
	host := absentSnapshot()
	if configDir != "" {
		host, _ = OpenHostStore(configDir, hostID).Read()
	}
	fleet, _ := OpenFleetStore(stateDir).ReadReplica(trusted)
	return ContextReply{Host: HostSection(host, full), Fleet: FleetSection(fleet, full)}
}

func HostSection(snapshot Snapshot[HostProfile], full bool) WireSection {
	section := snapshotSection(snapshot.State, snapshot.Revision, snapshot.Problem, full)
	if snapshot.Value == nil {
		return section
	}
	doc := snapshot.Value
	section.Digest, section.UpdatedAt = doc.Digest(), &doc.UpdatedAt
	section.InstructionsBytes = len(doc.Instructions)
	p := doc.Editable
	if !full {
		p.Instructions = ""
	}
	section.Profile = &p
	return section
}

func FleetSection(snapshot Snapshot[FleetDocument], full bool) WireSection {
	section := snapshotSection(snapshot.State, snapshot.Revision, snapshot.Problem, full)
	if snapshot.Value == nil {
		return section
	}
	doc := snapshot.Value
	section.Digest, section.UpdatedAt = doc.Digest(), &doc.UpdatedAt
	section.Author, section.Fleet = doc.Author, doc.Fleet
	section.InstructionsBytes = len(doc.Instructions)
	if full {
		p := doc.Editable
		section.Profile, section.Signature = &p, doc.Signature
	}
	return section
}

func snapshotSection(state ProfileState, revision *uint64, problem string, full bool) WireSection {
	completeness := Summary
	if full {
		completeness = Full
	}
	return WireSection{State: state, Revision: revision, Problem: problemText(problem), Completeness: completeness}
}

const MaximumContextBytes = 160 << 10

func ParseContext(data []byte, hostID string) (ContextReply, error) {
	var reply ContextReply
	if err := strictObject(data, MaximumContextBytes, []string{"host", "fleet"}); err != nil {
		return reply, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return reply, invalid("context", err.Error())
	}
	host, err := parseWireSection(raw["host"])
	if err != nil {
		return reply, err
	}
	fleet, err := parseWireSection(raw["fleet"])
	if err != nil {
		return reply, err
	}
	reply = ContextReply{Host: host, Fleet: fleet}
	if err := ValidateContext(reply, hostID); err != nil {
		return ContextReply{}, err
	}
	return reply, nil
}

func parseWireSection(data []byte) (WireSection, error) {
	var section WireSection
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil || raw == nil {
		return section, invalid("section", "must be an object")
	}
	fields := []string{"state", "completeness", "problem", "revision", "digest", "updatedAt", "author", "fleet", "signature", "profile", "instructionsBytes"}
	for key, value := range raw {
		if key == "revision" && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			continue
		}
		if err := checkField(key, value, fields); err != nil {
			return section, err
		}
	}
	if err := json.Unmarshal(data, &section); err != nil {
		return section, invalid("section", err.Error())
	}
	if section.Profile != nil {
		if err := strictObject(raw["profile"], FleetLimits.Encoded, editableFields); err != nil {
			return section, err
		}
	}
	return section, nil
}

func ValidateContext(reply ContextReply, hostID string) error {
	if err := validateSection(reply.Host, hostID, false); err != nil {
		return fmt.Errorf("host context: %w", err)
	}
	if err := validateSection(reply.Fleet, hostID, true); err != nil {
		return fmt.Errorf("fleet context: %w", err)
	}
	return nil
}

func validateSection(s WireSection, hostID string, fleet bool) error {
	if s.Completeness != Summary && s.Completeness != Full {
		return invalid("completeness", "must be summary or full")
	}
	if err := validateText("problem", s.Problem, 512, false); err != nil {
		return err
	}
	switch s.State {
	case StateAbsent:
		if s.Revision == nil || *s.Revision != 0 {
			return invalid("revision", "absence requires revision zero")
		}
	case StateInvalid, StateUnavailable:
	case StateValid, StateUntrusted:
		return validatePresentSection(s, hostID, fleet)
	default:
		return invalid("state", "unknown context state")
	}
	if s.Profile != nil || s.Digest != "" || s.Signature != "" || s.Author != "" || s.Fleet != "" || s.UpdatedAt != nil || s.InstructionsBytes != 0 {
		return invalid("section", "non-present context contains document fields")
	}
	return nil
}

func validatePresentSection(s WireSection, hostID string, fleet bool) error {
	if s.Revision == nil || *s.Revision == 0 || s.UpdatedAt == nil || s.UpdatedAt.IsZero() {
		return invalid("metadata", "requires revision and updatedAt")
	}
	digest, err := hex.DecodeString(s.Digest)
	if err != nil || len(digest) != 32 || hex.EncodeToString(digest) != s.Digest {
		return invalid("digest", "must be a lowercase SHA-256 digest")
	}
	limits := HostLimits
	if fleet {
		limits = FleetLimits
	}
	if s.InstructionsBytes < 0 || s.InstructionsBytes > limits.Instructions {
		return invalid("instructionsBytes", "outside profile limits")
	}
	if fleet {
		return validateFleetSection(s)
	}
	if s.State == StateUntrusted || s.Author != "" || s.Fleet != "" || s.Signature != "" {
		return invalid("host", "contains fleet-only metadata")
	}
	if err := validateSectionProfile(s, limits); err != nil {
		return err
	}
	if s.Completeness == Full && (HostProfile{Editable: *s.Profile, HostID: hostID, Revision: *s.Revision, UpdatedAt: *s.UpdatedAt}).Digest() != s.Digest {
		return invalid("digest", "does not match pinned host profile")
	}
	return nil
}

func validateSectionProfile(s WireSection, limits Limits) error {
	if s.Profile == nil {
		return invalid("profile", "missing profile body")
	}
	if _, err := normalizeEditable(*s.Profile, limits); err != nil {
		return err
	}
	if s.Completeness == Summary && s.Profile.Instructions != "" {
		return invalid("instructions", "summary contains instructions")
	}
	if s.Completeness == Full && len(s.Profile.Instructions) != s.InstructionsBytes {
		return invalid("instructionsBytes", "does not match full body")
	}
	return nil
}

func validateFleetSection(s WireSection) error {
	if err := validateText("fleet", s.Fleet, 256, false); err != nil {
		return err
	}
	author, err := baseIdentity(s.Author)
	if err != nil || len(author) != 32 {
		return invalid("author", "must be a Mesh identity")
	}
	if s.Completeness == Summary {
		if s.Profile != nil || s.Signature != "" {
			return invalid("fleet summary", "contains a full document")
		}
		return nil
	}
	if err := validateSectionProfile(s, FleetLimits); err != nil {
		return err
	}
	doc := FleetDocument{Editable: *s.Profile, Fleet: s.Fleet, Revision: *s.Revision, UpdatedAt: *s.UpdatedAt, Author: s.Author, Signature: s.Signature}
	if err := VerifyFleet(doc); err != nil {
		return err
	}
	if doc.Digest() != s.Digest {
		return invalid("digest", "does not match signed fleet profile")
	}
	return nil
}

func baseIdentity(id string) ([]byte, error) {
	key, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil || base64.RawURLEncoding.EncodeToString(key) != id {
		return nil, invalid("identity", "invalid encoding")
	}
	return key, nil
}
