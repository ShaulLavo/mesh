package apps

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"path"
	"slices"
	"strings"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/tailnet"
	"github.com/shaul/mesh/internal/transport"
)

var appTailnetIPv4 = netip.MustParsePrefix("100.64.0.0/10")
var appTailnetIPv6 = netip.MustParsePrefix("fd7a:115c:a1e0::/48")

func validateTailscaleName(name string) error {
	if name == "" || len(name) > 253 || name != strings.ToLower(name) || strings.HasSuffix(name, ".") || strings.TrimSpace(name) != name {
		return errors.New("app: Tailscale name is empty or not canonical")
	}
	for _, label := range strings.Split(name, ".") {
		if !validPeerLabel(label) {
			return errors.New("app: Tailscale name has an invalid DNS label")
		}
	}
	return nil
}
func validPeerLabel(label string) bool {
	if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for _, character := range label {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
			return false
		}
	}
	return true
}

func validateControlPath(webSocketPath string) error {
	parsedPath, err := url.Parse(webSocketPath)
	if err != nil || webSocketPath == "" || webSocketPath[0] != '/' || strings.Contains(webSocketPath, "\\") || path.Clean(webSocketPath) != webSocketPath ||
		parsedPath.Scheme != "" || parsedPath.Host != "" || parsedPath.User != nil || parsedPath.RawQuery != "" || parsedPath.Fragment != "" ||
		parsedPath.RawPath != "" || parsedPath.Opaque != "" || parsedPath.ForceQuery || parsedPath.EscapedPath() != webSocketPath {
		return errors.New("app: WebSocket path is not canonical")
	}
	return nil
}

func ValidateOriginEndpoint(endpoint netip.AddrPort, controlPort uint16) error {
	address := endpoint.Addr().Unmap()
	if !endpoint.IsValid() || endpoint.Port() != controlPort || !appTailnetIPv4.Contains(address) && !appTailnetIPv6.Contains(address) {
		return errors.New("app: resolved origin endpoint is not the configured numeric Tailscale control endpoint")
	}
	return nil
}

func ControlPinner(dial func(context.Context, string) (transport.Conn, error)) func(context.Context, netip.AddrPort, Peer) error {
	return func(ctx context.Context, endpoint netip.AddrPort, origin Peer) error {
		if err := validateControlPath(origin.WebSocketPath); err != nil {
			return err
		}
		var connection transport.Conn
		var err error
		if dial == nil {
			connection, err = transport.DialPinned(ctx, "ws://"+endpoint.String()+origin.WebSocketPath, origin.Identity)
		} else {
			connection, err = dial(ctx, "ws://"+endpoint.String()+origin.WebSocketPath)
		}
		if err != nil {
			return fmt.Errorf("app: connect origin identity check: %w", err)
		}
		defer connection.Close() //nolint:errcheck // the identity check result is authoritative
		return verifyControlIdentity(ctx, connection, origin.Identity)
	}
}

func verifyControlIdentity(ctx context.Context, connection transport.Conn, identity string) error {
	requestID, err := controlRequestID()
	if err != nil {
		return err
	}
	response, err := controlRoundTrip(ctx, connection, protocol.Control{Type: protocol.TypeHostInfo, RequestID: requestID})
	if err != nil {
		return err
	}
	if response.Type != protocol.TypeHostInfoResult || response.Host == nil || response.Host.ID != identity || response.Host.MeshIdentity != identity {
		return errors.New("app: host.info identity does not match its pin")
	}
	return nil
}

func controlRoundTrip(ctx context.Context, connection transport.Conn, request protocol.Control) (protocol.Control, error) {
	payload, err := request.Encode()
	if err != nil {
		return protocol.Control{}, fmt.Errorf("app: encode control request: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stop()
	if err := connection.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload}); err != nil {
		return protocol.Control{}, fmt.Errorf("app: write control request: %w", err)
	}
	frame, err := connection.ReadFrame()
	if err != nil {
		if ctx.Err() != nil {
			return protocol.Control{}, fmt.Errorf("app: read control response: %w", ctx.Err())
		}
		return protocol.Control{}, fmt.Errorf("app: read control response: %w", err)
	}
	if frame.Kind != protocol.KindControl {
		return protocol.Control{}, errors.New("app: response is not a control frame")
	}
	response, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return protocol.Control{}, fmt.Errorf("app: decode control response: %w", err)
	}
	if response.RequestID != request.RequestID {
		return protocol.Control{}, errors.New("app: response request ID does not match")
	}
	return response, nil
}

func controlRequestID() (string, error) {
	var contents [16]byte
	if _, err := io.ReadFull(rand.Reader, contents[:]); err != nil {
		return "", fmt.Errorf("app: generate control request ID: %w", err)
	}
	return hex.EncodeToString(contents[:]), nil
}

func resolveTailscalePeer(ctx context.Context, peers func(context.Context) ([]tailnet.Peer, error), name string, port uint16) (netip.AddrPort, error) {
	if peers == nil {
		return netip.AddrPort{}, errors.New("app: nil Tailscale peer discovery")
	}
	observed, err := peers(ctx)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("app: discover Tailscale peers: %w", err)
	}
	var matched *tailnet.Peer
	for index := range observed {
		peer := &observed[index]
		if peer.Name != name {
			continue
		}
		if matched != nil {
			return netip.AddrPort{}, errors.New("app: Tailscale returned a duplicate allowlisted peer name")
		}
		matched = peer
	}
	if matched == nil {
		return netip.AddrPort{}, errors.New("app: allowlisted Tailscale peer was not found")
	}
	if !matched.Online {
		return netip.AddrPort{}, errors.New("app: allowlisted peer is offline in Tailscale")
	}
	addresses := make([]netip.Addr, 0, len(matched.Addrs))
	for _, value := range matched.Addrs {
		address, err := netip.ParseAddr(value)
		if err != nil || !appTailnetIPv4.Contains(address.Unmap()) && !appTailnetIPv6.Contains(address.Unmap()) {
			return netip.AddrPort{}, errors.New("app: Tailscale returned an invalid peer address")
		}
		addresses = append(addresses, address.Unmap())
	}
	if len(addresses) == 0 {
		return netip.AddrPort{}, errors.New("app: allowlisted peer has no Tailscale address")
	}
	slices.SortFunc(addresses, comparePeerAddresses)
	return netip.AddrPortFrom(addresses[0], port), nil
}
func TailscaleResolver(peers func(context.Context) ([]tailnet.Peer, error)) func(context.Context, Peer) (netip.AddrPort, error) {
	return func(ctx context.Context, peer Peer) (netip.AddrPort, error) {
		return resolveTailscalePeer(ctx, peers, peer.TailscaleName, peer.ControlPort)
	}
}

func comparePeerAddresses(left, right netip.Addr) int {
	if left.Is4() == right.Is4() {
		return left.Compare(right)
	}
	if left.Is4() {
		return -1
	}
	return 1
}
