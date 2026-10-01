package sockdiag

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"time"

	"golang.org/x/sys/unix"
)

const tcpListen = 10

// TCPListeners returns every binding on port, including wildcard and non-loopback
// addresses. Filtering in the kernel keeps unrelated connections out of the reply.
func TCPListeners(ctx context.Context, port int) ([]netip.Addr, error) {
	if port < 1 || port > math.MaxUint16 {
		return nil, errors.New("TCP listener port must be from 1 to 65535")
	}
	selectedPort := uint16(port)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var addresses []netip.Addr
	for _, family := range []uint8{unix.AF_INET, unix.AF_INET6} {
		request, err := marshalListenerDiagRequest(selectedPort, family)
		if err != nil {
			return nil, err
		}
		err = exchange(ctx, request, func(raw []byte) (bool, error) {
			parsed, done, err := parseListenerDiagReply(raw, selectedPort, family)
			addresses = append(addresses, parsed...)
			return done, err
		})
		if err != nil {
			return nil, err
		}
	}
	return addresses, nil
}

func marshalListenerDiagRequest(port uint16, family uint8) ([]byte, error) {
	id := inetDiagSockID{Cookie: [2]uint32{math.MaxUint32, math.MaxUint32}}
	binary.BigEndian.PutUint16(id.SourcePort[:], port)
	return marshalDiagRequest(inetDiagReqV2{Family: family, Protocol: unix.IPPROTO_TCP, States: 1 << tcpListen, ID: id}, true)
}

func parseListenerDiagReply(raw []byte, port uint16, family uint8) ([]netip.Addr, bool, error) {
	if len(raw) == 0 {
		return nil, false, errors.New("empty TCP listener diagnostic reply")
	}
	var addresses []netip.Addr
	for len(raw) > 0 {
		header, data, err := diagMessage(raw)
		if err != nil {
			return nil, false, err
		}
		address, done, err := listenerDiagResult(header, data, port, family)
		if err != nil {
			return nil, false, err
		}
		length := (int(header.Len) + unix.NLMSG_ALIGNTO - 1) & ^(unix.NLMSG_ALIGNTO - 1)
		if length > len(raw) {
			return nil, false, errors.New("incomplete TCP listener diagnostic alignment")
		}
		raw = raw[length:]
		if done {
			if len(raw) != 0 {
				return nil, false, errors.New("TCP listener diagnostics continue after completion")
			}
			return addresses, true, nil
		}
		addresses = append(addresses, address)
	}
	return addresses, false, nil
}

func listenerDiagResult(header unix.NlMsghdr, data []byte, port uint16, family uint8) (netip.Addr, bool, error) {
	if header.Type == unix.NLMSG_ERROR {
		return netip.Addr{}, false, diagError(data)
	}
	if header.Flags&unix.NLM_F_MULTI == 0 {
		return netip.Addr{}, false, errors.New("TCP listener diagnostic reply is not multipart")
	}
	switch header.Type {
	case unix.NLMSG_DONE:
		err := listenerDiagDone(data)
		return netip.Addr{}, err == nil, err
	case unix.SOCK_DIAG_BY_FAMILY:
		address, err := listenerDiagAddress(data, port, family)
		return address, false, err
	default:
		return netip.Addr{}, false, errors.New("unexpected TCP listener diagnostic message")
	}
}

func listenerDiagDone(data []byte) error {
	if len(data) < 4 {
		return errors.New("incomplete TCP listener diagnostic completion")
	}
	code := binary.NativeEndian.Uint32(data[:4])
	if code == 0 {
		return nil
	}
	if code <= math.MaxInt32 {
		return errors.New("invalid TCP listener diagnostic completion status")
	}
	return fmt.Errorf("complete TCP listener diagnostics: %w", unix.Errno(^code+1))
}

func listenerDiagAddress(data []byte, port uint16, family uint8) (netip.Addr, error) {
	var socket inetDiagMsg
	if _, err := binary.Decode(data, binary.NativeEndian, &socket); err != nil {
		return netip.Addr{}, fmt.Errorf("decode TCP listener diagnostics: %w", err)
	}
	if socket.Family != family || binary.BigEndian.Uint16(socket.ID.SourcePort[:]) != port {
		return netip.Addr{}, errors.New("TCP listener diagnostic family or port does not match")
	}
	if socket.State != tcpListen || socket.Inode == 0 {
		return netip.Addr{}, errors.New("live TCP listener not found in diagnostic reply")
	}
	switch family {
	case unix.AF_INET:
		return netip.AddrFrom4([4]byte(socket.ID.Source[:4])), nil
	case unix.AF_INET6:
		return netip.AddrFrom16(socket.ID.Source), nil
	default:
		return netip.Addr{}, errors.New("unsupported TCP listener diagnostic family")
	}
}
