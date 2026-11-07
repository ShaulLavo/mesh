package hostprofile

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const ActionFleetPut = "fleet-profile.put"
const fleetDigestField = "digest"

type Acceptance string

const (
	Applied   Acceptance = "applied"
	Unchanged Acceptance = "unchanged"
	Conflict  Acceptance = "conflict"
	Older     Acceptance = "older"
	Untrusted Acceptance = "untrusted"
)

type PutRequest struct {
	Document FleetDocument `json:"document"`
}
type PutResult struct {
	Outcome  Acceptance     `json:"outcome"`
	Revision uint64         `json:"revision"`
	Digest   string         `json:"digest"`
	Current  *FleetDocument `json:"current,omitempty"`
}

func SignFleet(e Editable, fleet string, revision uint64, at time.Time, id string, key ed25519.PrivateKey) (FleetDocument, error) {
	normalized, err := normalizeEditable(e, FleetLimits)
	if err != nil {
		return FleetDocument{}, err
	}
	if len(key) != ed25519.PrivateKeySize || base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey)) != id {
		return FleetDocument{}, errors.New("fleet signing key does not match author")
	}
	doc := FleetDocument{Editable: normalized, Fleet: fleet, Revision: revision, UpdatedAt: at.UTC(), Author: id}
	doc.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, doc.signedBytes()))
	return doc, VerifyFleet(doc)
}

func ParsePutRequest(data []byte) (PutRequest, error) {
	var request PutRequest
	if err := strictObject(data, FleetLimits.Encoded+32, []string{"document"}); err != nil {
		return request, err
	}
	var raw struct {
		Document json.RawMessage `json:"document"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return request, fmt.Errorf("decode fleet request: %w", err)
	}
	doc, err := parseFleet(raw.Document)
	request.Document = doc
	return request, err
}

func (r PutResult) Validate(offered FleetDocument) error {
	if r.Outcome == Untrusted {
		return r.validateUntrusted()
	}
	if !validDigest(r.Digest) || r.Revision == 0 {
		return errors.New("fleet result lacks revision or digest")
	}
	switch r.Outcome {
	case Applied, Unchanged:
		if r.Revision != offered.Revision || r.Digest != offered.Digest() || r.Current != nil {
			return errors.New("fleet acceptance does not match offered document")
		}
	case Conflict:
		if r.Revision != offered.Revision || r.Digest == offered.Digest() || r.Current != nil {
			return errors.New("invalid fleet conflict result")
		}
	case Older:
		return r.validateCurrent(offered.Revision)
	case Untrusted:
		return r.validateUntrusted()
	default:
		return errors.New("unknown fleet acceptance outcome")
	}
	return nil
}
func (r PutResult) validateUntrusted() error {
	if r.Current != nil || r.Revision != 0 || r.Digest != "" {
		return errors.New("untrusted result contains accepted metadata")
	}
	return nil
}
func (r PutResult) validateCurrent(offered uint64) error {
	if r.Current == nil || r.Revision <= offered {
		return errors.New("fleet current result is not newer")
	}
	if r.Current.Revision != r.Revision || r.Current.Digest() != r.Digest {
		return errors.New("fleet current result metadata does not match document")
	}
	return VerifyFleet(*r.Current)
}

func validDigest(value string) bool {
	data, err := hex.DecodeString(value)
	return err == nil && len(data) == 32 && hex.EncodeToString(data) == value
}

// UnmarshalJSON keeps the complete signed Current document strict when
// update.Client decodes a reply, before any fields can be discarded.
func (r *PutResult) UnmarshalJSON(data []byte) error {
	if err := strictObject(data, FleetLimits.Encoded+1024, []string{"outcome", fleetRevisionField, fleetDigestField, "current"}); err != nil {
		return err
	}
	var value struct {
		Outcome  Acceptance      `json:"outcome"`
		Revision uint64          `json:"revision"`
		Digest   string          `json:"digest"`
		Current  json.RawMessage `json:"current"`
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("decode fleet put result: %w", err)
	}
	result := PutResult{Outcome: value.Outcome, Revision: value.Revision, Digest: value.Digest}
	if len(value.Current) != 0 {
		current, err := parseFleet(value.Current)
		if err != nil {
			return err
		}
		result.Current = &current
	}
	*r = result
	return nil
}
