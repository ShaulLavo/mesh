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

// Listener is one listening TCP socket. UID and Inode let a caller attribute it
// to a process: the kernel reports the socket's owner, and the inode is what
// that process's file descriptor links to.
type Listener struct {
	Address netip.AddrPort
	UID     uint32
	Inode   uint32
}

// TCPListeners returns every binding on port, including wildcard and non-loopback
// addresses. Filtering in the kernel keeps unrelated connections out of the reply.
func TCPListeners(ctx context.Context, port int) ([]Listener, error) {
	if port < 1 || port > math.MaxUint16 {
		return nil, errors.New("TCP listener port must be from 1 to 65535")
	}
	return listeners(ctx, uint16(port))
}

// AllTCPListeners returns every listening TCP socket in this network namespace.
func AllTCPListeners(ctx context.Context) ([]Listener, error) {
	return listeners(ctx, 0)
}

// listeners asks the kernel for LISTEN sockets on port, or on every port when it
// is 0: the kernel skips its source-port filter for a zero port.
func listeners(ctx context.Context, port uint16) ([]Listener, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var found []Listener
	for _, family := range []uint8{unix.AF_INET, unix.AF_INET6} {
		request, err := marshalListenerDiagRequest(port, family)
		if err != nil {
			return nil, err
		}
		err = exchange(ctx, request, func(raw []byte) (bool, error) {
			parsed, done, err := parseListenerDiagReply(raw, port, family)
			found = append(found, parsed...)
			return done, err
		})
		if err != nil {
			return nil, err
		}
	}
	return found, nil
}

func marshalListenerDiagRequest(port uint16, family uint8) ([]byte, error) {
	id := inetDiagSockID{Cookie: [2]uint32{math.MaxUint32, math.MaxUint32}}
	binary.BigEndian.PutUint16(id.SourcePort[:], port)
	return marshalDiagRequest(inetDiagReqV2{Family: family, Protocol: unix.IPPROTO_TCP, States: 1 << tcpListen, ID: id}, true)
}

func parseListenerDiagReply(raw []byte, port uint16, family uint8) ([]Listener, bool, error) {
	if len(raw) == 0 {
		return nil, false, errors.New("empty TCP listener diagnostic reply")
	}
	var found []Listener
	for len(raw) > 0 {
		header, data, err := diagMessage(raw)
		if err != nil {
			return nil, false, err
		}
		listener, done, err := listenerDiagResult(header, data, port, family)
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
			return found, true, nil
		}
		found = append(found, listener)
	}
	return found, false, nil
}

func listenerDiagResult(header unix.NlMsghdr, data []byte, port uint16, family uint8) (Listener, bool, error) {
	if header.Type == unix.NLMSG_ERROR {
		return Listener{}, false, diagError(data)
	}
	if header.Flags&unix.NLM_F_MULTI == 0 {
		return Listener{}, false, errors.New("TCP listener diagnostic reply is not multipart")
	}
	switch header.Type {
	case unix.NLMSG_DONE:
		err := listenerDiagDone(data)
		return Listener{}, err == nil, err
	case unix.SOCK_DIAG_BY_FAMILY:
		listener, err := listenerDiagSocket(data, port, family)
		return listener, false, err
	default:
		return Listener{}, false, errors.New("unexpected TCP listener diagnostic message")
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

func listenerDiagSocket(data []byte, port uint16, family uint8) (Listener, error) {
	var socket inetDiagMsg
	if _, err := binary.Decode(data, binary.NativeEndian, &socket); err != nil {
		return Listener{}, fmt.Errorf("decode TCP listener diagnostics: %w", err)
	}
	sourcePort := binary.BigEndian.Uint16(socket.ID.SourcePort[:])
	if socket.Family != family || port != 0 && sourcePort != port {
		return Listener{}, errors.New("TCP listener diagnostic family or port does not match")
	}
	if socket.State != tcpListen || socket.Inode == 0 {
		return Listener{}, errors.New("live TCP listener not found in diagnostic reply")
	}
	var address netip.Addr
	switch family {
	case unix.AF_INET:
		address = netip.AddrFrom4([4]byte(socket.ID.Source[:4]))
	case unix.AF_INET6:
		address = netip.AddrFrom16(socket.ID.Source)
	default:
		return Listener{}, errors.New("unsupported TCP listener diagnostic family")
	}
	return Listener{Address: netip.AddrPortFrom(address, sourcePort), UID: socket.UID, Inode: socket.Inode}, nil
}
