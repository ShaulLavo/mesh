package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/shaul/mesh/internal/access"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

func (s *clientServer) serveScoped(ctx context.Context, conn transport.Conn, binding access.Binding) error {
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	for {
		frame, err := conn.ReadFrame()
		if err != nil {
			return clientOperationError(ctx, "read scoped request", err)
		}
		response, err := s.scopedReply(ctx, binding, frame)
		if err != nil {
			return err
		}
		if err := conn.WriteFrame(response); err != nil {
			return clientOperationError(ctx, "write scoped reply", err)
		}
	}
}

func (s *clientServer) scopedReply(ctx context.Context, binding access.Binding, frame protocol.Frame) (protocol.Frame, error) {
	if frame.Kind != protocol.KindControl {
		return protocol.Frame{}, access.ErrDenied
	}
	name, err := protocol.AccessRequestType(frame.Payload)
	if err != nil {
		return protocol.Frame{}, fmt.Errorf("daemon: scoped admission: %w", err)
	}
	if name == protocol.TypeAccessOutput {
		return s.scopedOutputReply(ctx, binding, frame)
	}
	request, err := protocol.DecodeAccessRead(frame.Payload)
	if err != nil {
		return protocol.Frame{}, fmt.Errorf("daemon: scoped admission: %w", err)
	}
	if err := binding.RequireMetadata(); err != nil {
		return protocol.Frame{}, fmt.Errorf("daemon: scoped admission: %w", err)
	}
	result, readErr := s.access.Read(ctx, binding, request.Read)
	reply := protocol.AccessReply{Type: protocol.TypeAccessResult, RequestID: request.RequestID, Result: &result}
	if readErr != nil {
		reply = scopedError(request.RequestID, readErr)
	}
	if err := binding.RequireMetadata(); err != nil {
		return protocol.Frame{}, fmt.Errorf("daemon: scoped admission: %w", err)
	}
	data, err := reply.Encode()
	if err != nil {
		data, err = scopedError(request.RequestID, errAccessUnavailable).Encode()
		if err != nil {
			return protocol.Frame{}, fmt.Errorf("daemon: encode scoped error: %w", err)
		}
	}
	if len(data) > protocol.MaximumAccessReply {
		return protocol.Frame{}, errors.New("daemon: scoped reply exceeds size limit")
	}
	return protocol.Frame{Kind: protocol.KindControl, Payload: data}, nil
}

var (
	errAccessUnavailable = errors.New("state.unavailable")
	errAccessNotFound    = errors.New("session.not_found")
	errAccessLimit       = errors.New("resource.limit")
	errAccessInvalid     = errors.New("usage.invalid")
)

func scopedError(requestID string, err error) protocol.AccessReply {
	reply := protocol.AccessReply{Type: protocol.TypeAccessDenied, RequestID: requestID, ErrorCode: "state.unavailable", Message: "Metadata is unavailable."}
	switch {
	case errors.Is(err, access.ErrDenied):
		reply.ErrorCode = "access.denied"
		reply.Message = "Metadata permission is required."
	case errors.Is(err, errAccessNotFound):
		reply.ErrorCode = "session.not_found"
		reply.Message = "Session was not found."
	case errors.Is(err, errAccessLimit):
		reply.ErrorCode = "resource.limit"
		reply.Message = "Metadata exceeds the response limit."
	case errors.Is(err, errAccessInvalid):
		reply.ErrorCode = "usage.invalid"
		reply.Message = "Metadata request is invalid."
	}
	return reply
}

func (s *clientServer) scopedOutputReply(ctx context.Context, binding access.Binding, frame protocol.Frame) (protocol.Frame, error) {
	request, err := protocol.DecodeAccessOutput(frame.Payload)
	if err != nil {
		return protocol.Frame{}, fmt.Errorf("daemon: scoped output reply: %w", err)
	}
	if err := binding.RequireMetadata(); err != nil {
		return protocol.Frame{}, fmt.Errorf("daemon: scoped output reply: %w", err)
	}
	result, readErr := s.access.Output(ctx, binding, request.Output)
	var data []byte
	if readErr != nil {
		data, err = scopedOutputFailure(request.RequestID, readErr).Encode()
	} else {
		data, err = (protocol.AccessOutputReply{Type: protocol.TypeAccessOutputResult, RequestID: request.RequestID, Result: &result}).Encode(request.Output)
	}
	if err != nil {
		data, err = scopedOutputFailure(request.RequestID, errAccessUnavailable).Encode()
	}
	if err != nil {
		return protocol.Frame{}, fmt.Errorf("daemon: scoped output reply: %w", err)
	}
	if err := binding.RequireMetadata(); err != nil {
		return protocol.Frame{}, fmt.Errorf("daemon: scoped output reply: %w", err)
	}
	return protocol.Frame{Kind: protocol.KindControl, Payload: data}, nil
}
func scopedOutputFailure(requestID string, err error) protocol.AccessReply {
	reply := scopedError(requestID, err)
	switch reply.ErrorCode {
	case "access.denied":
		reply.Message = "Session-output permission is required."
	case "state.unavailable":
		reply.Message = "Session output is unavailable."
	case "resource.limit":
		reply.Message = "Session output exceeds the response limit."
	case "usage.invalid":
		reply.Message = "Session output request is invalid."
	}
	return reply
}
