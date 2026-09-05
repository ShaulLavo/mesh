package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/coder/websocket"

	"github.com/shaul/mesh/internal/edge"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
	meshserve "github.com/shaul/mesh/internal/serve"
	"github.com/shaul/mesh/internal/transport"
)

// clientServer dispatches frames for each disposable client connection. Its
// lifecycle and worker dependencies are safe to share across clients.
type clientServer struct {
	lifecycle    *lifecycle
	workers      WorkerConnector
	edge         controlHandler
	services     controlHandler
	certificates controlHandler
}

type controlHandler interface {
	HandleControl(context.Context, protocol.Control) (protocol.Control, bool, error)
}

type disabledEdgeController struct{}

func (disabledEdgeController) HandleControl(context.Context, protocol.Control) (protocol.Control, bool, error) {
	return protocol.Control{}, false, nil
}

func newClientServer(lifecycle *lifecycle, workers WorkerConnector, edge, services, certificates controlHandler) (*clientServer, error) {
	if lifecycle == nil {
		return nil, fmt.Errorf("daemon: nil client lifecycle")
	}
	if workers == nil {
		return nil, fmt.Errorf("daemon: nil client worker connector")
	}
	if edge == nil {
		return nil, fmt.Errorf("daemon: nil public edge controller")
	}
	if services == nil {
		return nil, fmt.Errorf("daemon: nil service controller")
	}
	if certificates == nil {
		return nil, fmt.Errorf("daemon: nil certificate controller")
	}
	return &clientServer{lifecycle: lifecycle, workers: workers, edge: edge, services: services, certificates: certificates}, nil
}

// Handle serves one client until it disconnects or ctx is cancelled. This is
// the sole reader; clientRelay only owns worker readers.
func (s *clientServer) Handle(ctx context.Context, conn transport.Conn) (resultErr error) {
	if ctx == nil {
		return fmt.Errorf("daemon: nil client context")
	}
	if conn == nil {
		return fmt.Errorf("daemon: nil client connection")
	}

	client := conn
	relay := newClientRelay(client, s.workers)
	stopCancellation := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer func() {
		stopCancellation()
		if closeErr := relay.Close(); closeErr != nil && !onlyNormalClientErrors(closeErr) {
			resultErr = errors.Join(resultErr, fmt.Errorf("daemon: close client relay: %w", closeErr))
		}
	}()

	for {
		frame, err := client.ReadFrame()
		if err != nil {
			return clientOperationError(ctx, "read client frame", err)
		}
		if ctx.Err() != nil {
			return nil
		}

		handled, requestErr := relay.HandleFrame(ctx, frame)
		if handled {
			if requestErr == nil {
				continue
			}
			if err := writeClientRequestError(relay, requestMetadata(frame), requestErr); err != nil {
				return clientOperationError(ctx, fmt.Sprintf("report client request error %v", requestErr), err)
			}
			continue
		}

		request, err := protocol.DecodeControl(frame.Payload)
		if err != nil {
			if err := writeClientRequestError(relay, request, fmt.Errorf("daemon: decode lifecycle control: %w", err)); err != nil {
				return clientOperationError(ctx, "report lifecycle decode error", err)
			}
			continue
		}

		response, lifecycleHandled, requestErr := s.lifecycle.HandleControl(ctx, request)
		if !lifecycleHandled {
			response, lifecycleHandled, requestErr = s.edge.HandleControl(ctx, request)
		}
		if !lifecycleHandled {
			response, lifecycleHandled, requestErr = s.services.HandleControl(ctx, request)
		}
		if !lifecycleHandled {
			response, lifecycleHandled, requestErr = s.certificates.HandleControl(ctx, request)
		}
		if requestErr != nil {
			if err := writeClientRequestError(relay, request, requestErr); err != nil {
				return clientOperationError(ctx, fmt.Sprintf("report %s error %v", request.Type, requestErr), err)
			}
			continue
		}
		if !lifecycleHandled {
			requestErr = fmt.Errorf("daemon: unknown control %q", request.Type)
			if err := writeClientRequestError(relay, request, requestErr); err != nil {
				return clientOperationError(ctx, fmt.Sprintf("report unknown control %q", request.Type), err)
			}
			continue
		}

		responseFrame, err := encodeClientControl(response)
		if err != nil {
			if err := writeClientRequestError(relay, request, err); err != nil {
				return clientOperationError(ctx, fmt.Sprintf("report %s encoding error", request.Type), err)
			}
			continue
		}
		if err := relay.enqueueResponse(responseFrame); err != nil {
			return clientOperationError(ctx, fmt.Sprintf("write %s response", request.Type), err)
		}
	}
}

func requestMetadata(frame protocol.Frame) protocol.Control {
	if frame.Kind == protocol.KindControl {
		request, _ := protocol.DecodeControl(frame.Payload)
		return request
	}
	return protocol.Control{SessionID: frame.Session.String()}
}

func writeClientRequestError(relay *clientRelay, request protocol.Control, requestErr error) error {
	frame, err := encodeClientControl(protocol.Control{
		Type:      protocol.TypeError,
		RequestID: request.RequestID,
		SessionID: request.SessionID,
		ErrorCode: clientErrorCode(requestErr),
		Message:   requestErr.Error(),
	})
	if err != nil {
		return fmt.Errorf("daemon: encode client error response: %w", err)
	}
	if err := relay.enqueueResponse(frame); err != nil {
		return fmt.Errorf("daemon: write client error response: %w", err)
	}
	return nil
}

func clientErrorCode(err error) string {
	if errors.Is(err, recovery.ErrUncertain) {
		return protocol.ErrorCodeRecoveryUncertain
	}
	switch {
	case errors.Is(err, edge.ErrRouteCollision):
		return protocol.ErrorCodeEdgeRouteCollision
	case errors.Is(err, edge.ErrStaleSequence):
		return protocol.ErrorCodeEdgeStaleSequence
	case errors.Is(err, edge.ErrSequenceConflict):
		return protocol.ErrorCodeEdgeConflict
	case errors.Is(err, edge.ErrWakeUnavailable):
		return protocol.ErrorCodeEdgeWakeUnavailable
	case errors.Is(err, meshserve.ErrCredentialsFound):
		return protocol.ErrorCodeCredentialsFound
	default:
		return ""
	}
}

func encodeClientControl(message protocol.Control) (protocol.Frame, error) {
	payload, err := message.Encode()
	if err != nil {
		return protocol.Frame{}, fmt.Errorf("daemon: encode %s response: %w", message.Type, err)
	}
	return protocol.Frame{Kind: protocol.KindControl, Payload: payload}, nil
}

func clientOperationError(ctx context.Context, operation string, err error) error {
	if ctx.Err() != nil || onlyNormalClientErrors(err) {
		return nil
	}
	return fmt.Errorf("daemon: %s: %w", operation, err)
}

func onlyNormalClientErrors(err error) bool {
	if err == nil {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !onlyNormalClientErrors(child) {
				return false
			}
		}
		return true
	}
	if unwrapped := errors.Unwrap(err); unwrapped != nil {
		return onlyNormalClientErrors(unwrapped)
	}
	if err == io.EOF || err == io.ErrClosedPipe || err == net.ErrClosed || err == transport.ErrClosed || err == context.Canceled || err == context.DeadlineExceeded {
		return true
	}
	status := websocket.CloseStatus(err)
	return status == websocket.StatusNormalClosure || status == websocket.StatusGoingAway
}
