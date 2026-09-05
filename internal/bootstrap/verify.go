package bootstrap

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/wake"
)

type verificationResult struct {
	host      verifiedHost
	endpoint  string
	connected bool
	err       error
}

func verifyWebSocket(ctx context.Context, addresses []string, port uint16, webSocketPath string) (verifiedHost, string, error) {
	if len(addresses) == 0 {
		return verifiedHost{}, "", diagnostic(DiagnosticPortBlocked, errors.New("no Tailscale address to verify"))
	}
	verifyCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan verificationResult, len(addresses))
	for _, address := range addresses {
		endpoint := meshEndpoint(address, port, webSocketPath)
		go func() {
			host, connected, err := verifyUntilReady(verifyCtx, endpoint)
			results <- verificationResult{host: host, endpoint: endpoint, connected: connected, err: err}
		}()
	}

	var dialErrors []error
	var protocolErrors []error
	for range addresses {
		result := <-results
		if result.err == nil {
			cancel()
			return result.host, result.endpoint, nil
		}
		if result.connected {
			protocolErrors = append(protocolErrors, fmt.Errorf("%s: %w", result.endpoint, result.err))
		} else {
			dialErrors = append(dialErrors, fmt.Errorf("%s: %w", result.endpoint, result.err))
		}
	}
	if len(protocolErrors) > 0 {
		return verifiedHost{}, "", diagnostic(DiagnosticIdentity, errors.Join(protocolErrors...))
	}
	if ctx.Err() != nil {
		dialErrors = append(dialErrors, ctx.Err())
	}
	return verifiedHost{}, "", diagnostic(DiagnosticPortBlocked, errors.Join(dialErrors...))
}

func verifyUntilReady(ctx context.Context, endpoint string) (verifiedHost, bool, error) {
	delay := 100 * time.Millisecond
	var lastErr error
	for {
		host, connected, err := verifyOne(ctx, endpoint)
		if err == nil || connected {
			return host, connected, err
		}
		lastErr = err
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
			if delay < time.Second {
				delay *= 2
				if delay > time.Second {
					delay = time.Second
				}
			}
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return verifiedHost{}, false, errors.Join(lastErr, ctx.Err())
		}
	}
}

func verifyOne(ctx context.Context, endpoint string) (verifiedHost, bool, error) {
	conn, err := transport.Dial(ctx, endpoint, transport.DialOptions{})
	if err != nil {
		return verifiedHost{}, false, err
	}
	stopCancellation := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer func() {
		stopCancellation()
		_ = conn.Close()
	}()

	requestID, err := verificationRequestID()
	if err != nil {
		return verifiedHost{}, true, err
	}
	payload, err := (protocol.Control{Type: protocol.TypeHostInfo, RequestID: requestID}).Encode()
	if err != nil {
		return verifiedHost{}, true, fmt.Errorf("encode host-info request: %w", err)
	}
	if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload}); err != nil {
		return verifiedHost{}, true, fmt.Errorf("write host-info request: %w", err)
	}
	frame, err := conn.ReadFrame()
	if err != nil {
		return verifiedHost{}, true, fmt.Errorf("read host-info response: %w", err)
	}
	if frame.Kind != protocol.KindControl {
		return verifiedHost{}, true, fmt.Errorf("host-info response has frame kind %d", frame.Kind)
	}
	response, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return verifiedHost{}, true, fmt.Errorf("decode host-info response: %w", err)
	}
	if response.RequestID != requestID {
		return verifiedHost{}, true, fmt.Errorf("host-info response request ID %q does not match %q", response.RequestID, requestID)
	}
	if response.Type == protocol.TypeError {
		return verifiedHost{}, true, fmt.Errorf("daemon rejected host-info request: %s", response.Message)
	}
	if response.Type != protocol.TypeHostInfoResult || response.Host == nil {
		return verifiedHost{}, true, fmt.Errorf("host-info response has type %q and host %v", response.Type, response.Host != nil)
	}
	if response.Host.Wake != nil {
		if response.Host.Wake.TargetID != response.Host.ID {
			return verifiedHost{}, true, errors.New("wake permission belongs to another host")
		}
		if err := wake.ValidateGrant(*response.Host.Wake, time.Now()); err != nil {
			return verifiedHost{}, true, err
		}
	}
	return verifiedHost{
		Wake:          response.Host.Wake,
		ID:            response.Host.ID,
		MeshIdentity:  response.Host.MeshIdentity,
		TailscaleName: response.Host.TailscaleName,
	}, true, nil
}

func meshEndpoint(address string, port uint16, webSocketPath string) string {
	return (&url.URL{
		Scheme: "ws",
		Host:   net.JoinHostPort(address, fmt.Sprintf("%d", port)),
		Path:   webSocketPath,
	}).String()
}

func verificationRequestID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate verification request ID: %w", err)
	}
	return hex.EncodeToString(random[:]), nil
}
