package sockdiag

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"net"
	"net/netip"
	"testing"

	"golang.org/x/sys/unix"
)

func TestListenerDiagRequestFiltersStateAndSourcePort(t *testing.T) {
	for _, family := range []uint8{unix.AF_INET, unix.AF_INET6} {
		raw, err := marshalListenerDiagRequest(3000, family)
		if err != nil {
			t.Fatal(err)
		}
		header, data, err := diagMessage(raw)
		if err != nil {
			t.Fatal(err)
		}
		var request inetDiagReqV2
		if _, err := binary.Decode(data, binary.NativeEndian, &request); err != nil {
			t.Fatal(err)
		}
		if header.Len != unix.NLMSG_HDRLEN+inetDiagReqSize || header.Type != unix.SOCK_DIAG_BY_FAMILY ||
			header.Flags != unix.NLM_F_REQUEST|unix.NLM_F_DUMP || request.Family != family ||
			request.Protocol != unix.IPPROTO_TCP || request.States != 1<<tcpListen ||
			request.ID.SourcePort != [2]byte{0x0b, 0xb8} || request.ID.DestinationPort != [2]byte{} ||
			request.ID.Source != [16]byte{} || request.ID.Destination != [16]byte{} ||
			request.ID.Cookie != [2]uint32{math.MaxUint32, math.MaxUint32} {
			t.Fatalf("listener query does not filter LISTEN and port 3000: %+v %+v", header, request)
		}
	}
}

func listenerDiagReply(t *testing.T, family uint8, address string) []byte {
	t.Helper()
	id := inetDiagSockID{}
	binary.BigEndian.PutUint16(id.SourcePort[:], 3000)
	parsed := netip.MustParseAddr(address)
	if parsed.Is4() {
		ip := parsed.As4()
		copy(id.Source[:], ip[:])
	} else {
		id.Source = parsed.As16()
	}
	raw := peerDiagReply(t, inetDiagMsg{Family: family, State: tcpListen, ID: id, Inode: 101})
	binary.NativeEndian.PutUint16(raw[6:8], unix.NLM_F_MULTI)
	return raw
}

func listenerDoneReply(t *testing.T, code uint32) []byte {
	t.Helper()
	raw, err := binary.Append(nil, binary.NativeEndian, struct {
		Header unix.NlMsghdr
		Code   uint32
	}{unix.NlMsghdr{Len: unix.NLMSG_HDRLEN + 4, Type: unix.NLMSG_DONE, Flags: unix.NLM_F_MULTI, Seq: diagSequence}, code})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestListenerDiagReplyIncludesUnsafeAndMappedAddresses(t *testing.T) {
	for _, tt := range []struct {
		family    uint8
		addresses []string
	}{
		{unix.AF_INET, []string{"127.0.0.1", "0.0.0.0", "100.64.0.2", "127.0.0.2"}},
		{unix.AF_INET6, []string{"::1", "::", "::ffff:127.0.0.1", "::ffff:0.0.0.0"}},
	} {
		var raw []byte
		for _, address := range tt.addresses {
			raw = append(raw, listenerDiagReply(t, tt.family, address)...)
		}
		raw = append(raw, listenerDoneReply(t, 0)...)
		addresses, done, err := parseListenerDiagReply(raw, 3000, tt.family)
		if err != nil || !done || len(addresses) != len(tt.addresses) {
			t.Fatalf("listeners = %v, done %t, error %v", addresses, done, err)
		}
		for i, expected := range tt.addresses {
			if addresses[i] != netip.MustParseAddr(expected) {
				t.Fatalf("address %d = %v; want %s", i, addresses[i], expected)
			}
		}
	}
}

func TestListenerDiagReplyRequiresCompletion(t *testing.T) {
	addresses, done, err := parseListenerDiagReply(listenerDiagReply(t, unix.AF_INET, "127.0.0.1"), 3000, unix.AF_INET)
	if err != nil || done || len(addresses) != 1 {
		t.Fatalf("partial dump = %v, %t, %v", addresses, done, err)
	}
	addresses, done, err = parseListenerDiagReply(listenerDoneReply(t, 0), 3000, unix.AF_INET)
	if err != nil || !done || len(addresses) != 0 {
		t.Fatalf("empty completed dump = %v, %t, %v", addresses, done, err)
	}
	if _, done, err := parseListenerDiagReply(listenerDoneReply(t, ^uint32(unix.EINTR)+1), 3000, unix.AF_INET); done || !errors.Is(err, unix.EINTR) {
		t.Fatalf("failed dump completion = %t, %v", done, err)
	}
}

func TestListenerDiagReplyRejectsMalformedMessages(t *testing.T) {
	valid := listenerDiagReply(t, unix.AF_INET, "127.0.0.1")
	for _, tt := range []struct {
		name   string
		change func([]byte) []byte
	}{
		{"empty", func([]byte) []byte { return nil }},
		{"short header", func(raw []byte) []byte { return raw[:15] }},
		{"short socket", func(raw []byte) []byte { binary.NativeEndian.PutUint32(raw[:4], 20); return raw[:20] }},
		{"oversized length", func(raw []byte) []byte { binary.NativeEndian.PutUint32(raw[:4], math.MaxUint32); return raw }},
		{"wrong sequence", func(raw []byte) []byte { binary.NativeEndian.PutUint32(raw[8:12], diagSequence+1); return raw }},
		{"not multipart", func(raw []byte) []byte { binary.NativeEndian.PutUint16(raw[6:8], 0); return raw }},
		{"dump interrupted", func(raw []byte) []byte {
			binary.NativeEndian.PutUint16(raw[6:8], unix.NLM_F_MULTI|unix.NLM_F_DUMP_INTR)
			return raw
		}},
		{"overrun", func(raw []byte) []byte { binary.NativeEndian.PutUint16(raw[4:6], unix.NLMSG_OVERRUN); return raw }},
		{"wrong family", func(raw []byte) []byte { raw[16] = unix.AF_INET6; return raw }},
		{"established", func(raw []byte) []byte { raw[17] = tcpEstablished; return raw }},
		{"TIME_WAIT", func(raw []byte) []byte { raw[17] = 6; return raw }},
		{"wrong port", func(raw []byte) []byte { raw[21]++; return raw }},
		{"no inode", func(raw []byte) []byte { binary.NativeEndian.PutUint32(raw[84:88], 0); return raw }},
		{"trailing garbage", func(raw []byte) []byte { return append(raw, 0) }},
		{"data after done", func(raw []byte) []byte { return append(listenerDoneReply(t, 0), raw...) }},
		{"short done", func([]byte) []byte {
			raw := listenerDoneReply(t, 0)
			binary.NativeEndian.PutUint32(raw[:4], 16)
			return raw[:16]
		}},
		{"positive done status", func([]byte) []byte { return listenerDoneReply(t, 1) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			addresses, done, err := parseListenerDiagReply(tt.change(append([]byte(nil), valid...)), 3000, unix.AF_INET)
			if err == nil || done || len(addresses) != 0 {
				t.Fatalf("malformed reply accepted = %v, %t, %v", addresses, done, err)
			}
		})
	}
}

func TestListenerDiagKernelErrorWithoutMultipartFlag(t *testing.T) {
	raw, err := binary.Append(nil, binary.NativeEndian, struct {
		Header  unix.NlMsghdr
		Code    uint32
		Request unix.NlMsghdr
	}{Header: unix.NlMsghdr{Len: unix.NLMSG_HDRLEN*2 + 4, Type: unix.NLMSG_ERROR, Seq: diagSequence}, Code: ^uint32(unix.EPERM) + 1})
	if err != nil {
		t.Fatal(err)
	}
	if addresses, done, err := parseListenerDiagReply(raw, 3000, unix.AF_INET); len(addresses) != 0 || done || !errors.Is(err, unix.EPERM) {
		t.Fatalf("kernel error = %v, %t, %v", addresses, done, err)
	}
}

func TestDiagSenderRejectsUserspaceAndTruncation(t *testing.T) {
	for _, tt := range []struct {
		sender unix.Sockaddr
		flags  int
	}{
		{nil, 0},
		{&unix.SockaddrInet4{}, 0},
		{&unix.SockaddrNetlink{Pid: 12}, 0},
		{&unix.SockaddrNetlink{Groups: 1}, 0},
		{&unix.SockaddrNetlink{}, unix.MSG_TRUNC},
	} {
		if err := validateDiagSender(tt.sender, tt.flags); err == nil {
			t.Fatalf("accepted diagnostic sender %+v, flags %d", tt.sender, tt.flags)
		}
	}
	if err := validateDiagSender(&unix.SockaddrNetlink{}, 0); err != nil {
		t.Fatal(err)
	}
}

func TestTCPListenersFindsEveryBindingAcrossDatagrams(t *testing.T) {
	const count = 384
	port := 0
	for range count {
		fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, unix.IPPROTO_TCP)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = unix.Close(fd) })
		if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEPORT, 1); err != nil {
			t.Fatal(err)
		}
		if err := unix.Bind(fd, &unix.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}}); err != nil {
			t.Fatal(err)
		}
		if err := unix.Listen(fd, 1); err != nil {
			t.Fatal(err)
		}
		local, err := unix.Getsockname(fd)
		if err != nil {
			t.Fatal(err)
		}
		port = local.(*unix.SockaddrInet4).Port
	}
	other, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	addresses, err := TCPListeners(context.Background(), port)
	if err != nil || len(addresses) != count {
		t.Fatalf("multipart listeners = %d, %v; want %d", len(addresses), err, count)
	}
	for _, address := range addresses {
		if address != netip.MustParseAddr("127.0.0.1") {
			t.Fatalf("unrelated listener returned %v", address)
		}
	}
}

func TestTCPListenersCancellationAndInvalidPort(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if addresses, err := TCPListeners(ctx, 3000); len(addresses) != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled query = %v, %v", addresses, err)
	}
	if _, err := TCPListeners(context.Background(), 0); err == nil {
		t.Fatal("zero port accepted")
	}
}
