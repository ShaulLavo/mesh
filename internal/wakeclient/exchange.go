package wakeclient

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

var ErrIdentityChanged = errors.New("wake peer identity changed or is invalid")

func exchange(ctx context.Context, endpoint, expectedID string, request protocol.Control) (protocol.HostInfo, protocol.Control, error) {
	conn, err := transport.DialOnce(ctx, endpoint, transport.DialOptions{})
	if err != nil {
		return protocol.HostInfo{}, protocol.Control{}, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer func() { stop(); _ = conn.Close() }()
	info, err := roundTrip(conn, protocol.Control{Type: protocol.TypeHostInfo})
	if err != nil {
		return protocol.HostInfo{}, protocol.Control{}, err
	}
	if info.Type != protocol.TypeHostInfoResult || info.Host == nil {
		return protocol.HostInfo{}, protocol.Control{}, errors.New("wake peer did not return host identity")
	}
	host := *info.Host
	key, err := base64.RawURLEncoding.DecodeString(host.ID)
	if err != nil || len(key) != 32 || host.MeshIdentity != host.ID || expectedID != "" && host.ID != expectedID {
		return protocol.HostInfo{}, protocol.Control{}, ErrIdentityChanged
	}
	if request.Type == "" {
		return host, info, nil
	}
	response, err := roundTrip(conn, request)
	return host, response, err
}

func roundTrip(conn transport.Conn, request protocol.Control) (protocol.Control, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return protocol.Control{}, err
	}
	request.RequestID = hex.EncodeToString(id[:])
	payload, err := request.Encode()
	if err != nil {
		return protocol.Control{}, err
	}
	if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload}); err != nil {
		return protocol.Control{}, err
	}
	frame, err := conn.ReadFrame()
	if err != nil {
		return protocol.Control{}, err
	}
	if frame.Kind != protocol.KindControl {
		return protocol.Control{}, errors.New("wake response is not a control frame")
	}
	response, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return protocol.Control{}, err
	}
	if response.RequestID != request.RequestID {
		return protocol.Control{}, fmt.Errorf("wake response request ID does not match %s", request.RequestID)
	}
	return response, nil
}
