package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

var errWatchMode = errors.New("daemon: state.watch requires a dedicated control connection")

func readStateRequests(ctx context.Context, conn transport.Conn, requests chan<- protocol.Frame, cancel context.CancelFunc) {
	defer cancel()
	for {
		frame, err := conn.ReadFrame()
		if err != nil {
			return
		}
		select {
		case requests <- frame:
		case <-ctx.Done():
			return
		}
	}
}

func (s *clientServer) stateRequest(ctx context.Context, frame protocol.Frame) protocol.Control {
	request := requestMetadata(frame)
	if frame.Kind != protocol.KindControl {
		return stateRequestError(request, errWatchMode)
	}
	request, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return stateRequestError(request, fmt.Errorf("daemon: decode watch control: %w", err))
	}
	if err := validateRequestID(request); err != nil {
		return stateRequestError(request, err)
	}
	readCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	response, err := s.stateReadControl(readCtx, request)
	if err != nil {
		return stateRequestError(request, err)
	}
	if response.Host != nil && s.wake != nil {
		response.Host.Wake = s.wake.info()
	}
	return response
}
func stateRequestError(request protocol.Control, err error) protocol.Control {
	return protocol.Control{Type: protocol.TypeError, RequestID: request.RequestID, SessionID: request.SessionID, ErrorCode: clientErrorCode(err), Message: err.Error()}
}
func (s *clientServer) stateReadControl(ctx context.Context, request protocol.Control) (protocol.Control, error) {
	var handler controlHandler
	switch request.Type {
	case protocol.TypeHostInfo, protocol.TypeList, protocol.TypeInspect:
		handler = s.lifecycle
	case protocol.TypeServiceList:
		handler = s.services
	case protocol.TypeHostMetrics:
		if s.metrics == nil {
			return protocol.Control{}, errWatchMode
		}
		frame, err := s.readMetrics(ctx, request)
		if err != nil {
			return protocol.Control{}, err
		}
		response, err := protocol.DecodeControl(frame.Payload)
		if err != nil {
			return protocol.Control{}, fmt.Errorf("daemon: decode metrics reply: %w", err)
		}
		return response, nil
	default:
		return protocol.Control{}, errWatchMode
	}
	response, handled, err := handler.HandleControl(ctx, request)
	if err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: read watch control: %w", err)
	}
	if !handled {
		return protocol.Control{}, errWatchMode
	}
	return response, nil
}
