// Package sockdiag inspects Linux TCP sockets without reading host-wide proc tables.
package sockdiag

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"golang.org/x/sys/unix"
)

const (
	diagSequence    = 1
	tcpEstablished  = 1
	inetDiagReqSize = 56
)

// Ports and addresses are network-endian inside otherwise native-endian netlink
// structures. Byte arrays preserve that distinction without unsafe casts.
type inetDiagSockID struct {
	SourcePort      [2]byte
	DestinationPort [2]byte
	Source          [16]byte
	Destination     [16]byte
	Interface       uint32
	Cookie          [2]uint32
}

type inetDiagReqV2 struct {
	Family     uint8
	Protocol   uint8
	Extensions uint8
	Pad        uint8
	States     uint32
	ID         inetDiagSockID
}

type inetDiagMsg struct {
	Family       uint8
	State        uint8
	Timer        uint8
	Retransmits  uint8
	ID           inetDiagSockID
	Expires      uint32
	ReceiveQueue uint32
	SendQueue    uint32
	UID          uint32
	Inode        uint32
}

func marshalDiagRequest(request inetDiagReqV2, dump bool) ([]byte, error) {
	flags := uint16(unix.NLM_F_REQUEST)
	if dump {
		flags |= unix.NLM_F_DUMP
	}
	raw, err := binary.Append(nil, binary.NativeEndian, struct {
		Header  unix.NlMsghdr
		Request inetDiagReqV2
	}{
		Header:  unix.NlMsghdr{Len: unix.NLMSG_HDRLEN + inetDiagReqSize, Type: unix.SOCK_DIAG_BY_FAMILY, Flags: flags, Seq: diagSequence},
		Request: request,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal TCP socket diagnostic request: %w", err)
	}
	return raw, nil
}

func exchange(ctx context.Context, request []byte, receive func([]byte) (bool, error)) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("query TCP socket diagnostics: %w", err)
	}
	fd, err := openDiag(request)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	reply := make([]byte, 32768)
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("query TCP socket diagnostics: %w", err)
		}
		raw, err := receiveDiag(fd, reply)
		if err != nil {
			return err
		}
		done, err := receive(raw)
		if err != nil {
			return err
		}
		if done {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("complete TCP socket diagnostics: %w", err)
			}
			return nil
		}
	}
}

func openDiag(request []byte) (int, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_SOCK_DIAG)
	if err != nil {
		return -1, fmt.Errorf("open TCP socket diagnostics: %w", err)
	}
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Sec: 1}); err != nil {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("bound TCP socket diagnostic wait: %w", err)
	}
	if err := unix.Sendto(fd, request, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("send TCP socket diagnostic query: %w", err)
	}
	return fd, nil
}

func receiveDiag(fd int, reply []byte) ([]byte, error) {
	n, _, flags, sender, err := unix.Recvmsg(fd, reply, nil, 0)
	if err != nil {
		return nil, fmt.Errorf("receive TCP socket diagnostics: %w", err)
	}
	if err := validateDiagSender(sender, flags); err != nil {
		return nil, err
	}
	return reply[:n], nil
}

func validateDiagSender(sender unix.Sockaddr, flags int) error {
	kernel, ok := sender.(*unix.SockaddrNetlink)
	if !ok || kernel.Pid != 0 || kernel.Groups != 0 || flags&unix.MSG_TRUNC != 0 {
		return errors.New("invalid TCP socket diagnostic sender or truncated reply")
	}
	return nil
}

func diagMessage(raw []byte) (unix.NlMsghdr, []byte, error) {
	var header unix.NlMsghdr
	if _, err := binary.Decode(raw, binary.NativeEndian, &header); err != nil {
		return header, nil, fmt.Errorf("decode TCP socket diagnostic header: %w", err)
	}
	if header.Len < unix.NLMSG_HDRLEN || uint64(header.Len) > uint64(len(raw)) || header.Seq != diagSequence {
		return header, nil, errors.New("invalid TCP socket diagnostic header")
	}
	// An interrupted dump cannot prove that every binding on the port was safe.
	if header.Flags&unix.NLM_F_DUMP_INTR != 0 {
		return header, nil, errors.New("TCP socket diagnostic dump interrupted")
	}
	return header, raw[unix.NLMSG_HDRLEN:header.Len], nil
}

func diagError(data []byte) error {
	if len(data) < 4+unix.NLMSG_HDRLEN {
		return errors.New("incomplete TCP socket diagnostic error")
	}
	code := binary.NativeEndian.Uint32(data[:4])
	if code <= math.MaxInt32 {
		return errors.New("unexpected TCP socket diagnostic acknowledgement")
	}
	return fmt.Errorf("query TCP socket diagnostics: %w", unix.Errno(^code+1))
}
