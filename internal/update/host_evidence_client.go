package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

type EvidenceReadState string

const (
	EvidenceVerified    EvidenceReadState = "verified"
	EvidenceReading     EvidenceReadState = "reading"
	EvidenceOffline     EvidenceReadState = "offline"
	EvidenceRefused     EvidenceReadState = "refused"
	EvidenceUnsupported EvidenceReadState = "unsupported"
	EvidenceInvalid     EvidenceReadState = "invalid"
)

type EvidenceReadError struct {
	State EvidenceReadState
	Cause error
}

func (e *EvidenceReadError) Error() string {
	return fmt.Sprintf("update evidence %s: %v", e.State, e.Cause)
}
func (e *EvidenceReadError) Unwrap() error { return e.Cause }
func evidenceError(state EvidenceReadState, err error) error {
	return &EvidenceReadError{State: state, Cause: err}
}

// Evidence uses full device admission, independently of update-administrator
// enrollment. Only a failed connection can reuse the saved successful read.
func (c Client) Evidence(ctx context.Context, host Host, request EvidenceRequest) (EvidenceReply, error) {
	if err := host.Validate(); err != nil {
		return EvidenceReply{}, evidenceError(EvidenceInvalid, err)
	}
	if err := validEvidenceRequest(request); err != nil {
		return EvidenceReply{}, evidenceError(EvidenceInvalid, err)
	}
	conn, err := c.dial(ctx, host)
	if err != nil {
		return EvidenceReply{}, evidenceError(evidenceDialState(err), err)
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	id, err := writeEvidenceRequest(conn, request)
	if err != nil {
		return EvidenceReply{}, evidenceError(EvidenceInvalid, err)
	}

	frame, err := conn.ReadFrame()
	if err != nil {
		return EvidenceReply{}, evidenceError(EvidenceInvalid, err)
	}
	response, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return EvidenceReply{}, evidenceError(EvidenceInvalid, err)
	}
	if response.RequestID != id || frame.Kind != protocol.KindControl {
		return EvidenceReply{}, evidenceError(EvidenceInvalid, errors.New("uncorrelated evidence reply"))
	}
	if response.Type == protocol.TypeError {
		state := EvidenceInvalid
		if response.ErrorCode == protocol.ErrorCodeUnknownControl {
			state = EvidenceUnsupported
		}
		return EvidenceReply{}, evidenceError(state, errors.New(response.Message))
	}
	if response.Type != protocol.TypeUpdateEvidenceResult {
		return EvidenceReply{}, evidenceError(EvidenceUnsupported, errors.New("peer does not support update.evidence"))
	}
	var reply EvidenceReply
	if err := decodeMessage(response.Update, &reply); err != nil {
		return reply, evidenceError(EvidenceInvalid, err)
	}
	if err := validateEvidenceReply(host.ID, request, reply); err != nil {
		return reply, evidenceError(EvidenceInvalid, err)
	}
	return reply, nil
}

func evidenceDialState(err error) EvidenceReadState {
	if errors.Is(err, transport.ErrAuthentication) || errors.Is(err, transport.ErrAuthenticationRequired) || errors.Is(err, transport.ErrControlAuthenticationRequired) {
		return EvidenceRefused
	}
	var network *net.OpError
	if errors.As(err, &network) && network.Op == "dial" {
		return EvidenceOffline
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return EvidenceOffline
	}
	return EvidenceRefused
}

func validateEvidenceReply(id string, request EvidenceRequest, reply EvidenceReply) error {
	if reply.Schema != 1 || reply.ID != id || reply.Mode != HostManual && reply.Mode != HostAutomatic || !validHostPhase(reply.Phase) || len(reply.Entries) > request.Limit || len(reply.Failed) > request.Limit || len(reply.Attention) > 512 || len(reply.FailedNext) > 256 {
		return errors.New("update: invalid evidence reply identity or bounds")
	}
	if err := validateEvidenceEntries(request, reply); err != nil {
		return err
	}

	for _, failed := range reply.Failed {
		if err := validEvidenceFailure(failed); err != nil {
			return err
		}
	}
	if reply.FailedNext != "" && (len(reply.Failed) == 0 || reply.FailedNext <= request.FailedAfter || stringsContainsPath(reply.FailedNext)) {
		return errors.New("update: invalid failure continuation")
	}
	if reply.Adoption != nil {
		return reply.Adoption.validate()
	}
	return nil
}

func stringsContainsPath(value string) bool {
	for _, char := range value {
		if char == '/' || char == '\\' {
			return true
		}
	}
	return false
}

func writeEvidenceRequest(conn transport.Conn, request EvidenceRequest) (string, error) {
	data, err := json.Marshal(request)
	if err != nil {
		return "", hostError(err)
	}
	id, err := newAutomaticID()
	if err != nil {
		return "", hostError(err)
	}
	payload, err := (protocol.Control{Type: protocol.TypeUpdateEvidence, RequestID: id, Update: data}).Encode()
	if err != nil {
		return "", hostError(err)
	}
	if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload}); err != nil {
		return "", hostError(err)
	}
	return id, nil
}

func validateEvidenceEntries(request EvidenceRequest, reply EvidenceReply) error {
	seq := request.AfterSeq
	for _, entry := range reply.Entries {
		if err := validEvidenceEntry(entry); err != nil {
			return err
		}
		if entry.Seq <= seq {
			return errors.New("update: evidence sequence did not advance")
		}
		seq = entry.Seq
	}
	if reply.More && len(reply.Entries) == 0 {
		return errors.New("update: empty continuing evidence page")
	}
	return nil
}
