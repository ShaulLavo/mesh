package tailnet

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

const maximumPeerTableBytes = 4 << 20

type systemPeerUIDLookup struct{}

func ProxyForwarderUIDs() ([]uint32, error) {
	for _, ipv6 := range []bool{false, true} {
		if _, err := readPeerTable(ipv6); err != nil {
			if ipv6 && errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("tailnet: PROXY forwarder authentication unavailable: %w", err)
		}
	}
	uid := int64(os.Getuid())
	if uid < 0 || uid > math.MaxUint32 {
		return nil, fmt.Errorf("tailnet: invalid Mesh UID %d", uid)
	}
	return []uint32{0, uint32(uid)}, nil
}

func (systemPeerUIDLookup) PeerUID(c net.Conn) (uint32, error) {
	peer, err := netip.ParseAddrPort(c.RemoteAddr().String())
	if err != nil {
		return 0, fmt.Errorf("parse peer socket address: %w", err)
	}
	local, err := netip.ParseAddrPort(c.LocalAddr().String())
	if err != nil {
		return 0, fmt.Errorf("parse local socket address: %w", err)
	}
	peer = netip.AddrPortFrom(peer.Addr().Unmap(), peer.Port())
	local = netip.AddrPortFrom(local.Addr().Unmap(), local.Port())
	for _, table := range []struct {
		path string
		ipv6 bool
	}{{"/proc/net/tcp", false}, {"/proc/net/tcp6", true}} {
		raw, err := readPeerTable(table.ipv6)
		if err != nil {
			if table.ipv6 && errors.Is(err, os.ErrNotExist) {
				continue
			}
			return 0, err
		}
		uid, found, err := parsePeerUID(raw, peer, local, table.ipv6)
		if err != nil {
			return 0, fmt.Errorf("inspect %s: %w", table.path, err)
		}
		if found {
			return uid, nil
		}
	}
	return 0, errors.New("live peer socket owner not found")
}

func readPeerTable(ipv6 bool) ([]byte, error) {
	path := "/proc/net/tcp"
	if ipv6 {
		path = "/proc/net/tcp6"
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(io.LimitReader(file, maximumPeerTableBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(raw) > maximumPeerTableBytes {
		return nil, fmt.Errorf("inspect %s: TCP table exceeds inspection limit", path)
	}
	return raw, nil
}

func parsePeerUID(raw []byte, peer, local netip.AddrPort, ipv6 bool) (uint32, bool, error) {
	if len(raw) > maximumPeerTableBytes {
		return 0, false, errors.New("TCP table exceeds inspection limit")
	}
	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 || fields[0] == "sl" {
			continue
		}
		if len(fields) < 10 {
			return 0, false, errors.New("malformed kernel TCP table")
		}
		// TIME_WAIT rows have no owning socket and may report UID 0. They must
		// never authenticate as tailscaled after a client closes its socket.
		if fields[3] != "01" || fields[9] == "0" {
			continue
		}
		client, err := linuxSocketAddress(fields[1], ipv6)
		if err != nil {
			return 0, false, err
		}
		server, err := linuxSocketAddress(fields[2], ipv6)
		if err != nil {
			return 0, false, err
		}
		// The accepted socket belongs to Mesh. Only the reverse tuple identifies
		// the forwarder's socket and its UID.
		if client != peer || server != local {
			continue
		}
		uid, err := strconv.ParseUint(fields[7], 10, 32)
		if err != nil {
			return 0, false, fmt.Errorf("parse peer socket UID: %w", err)
		}
		return uint32(uid), true, nil
	}
	if err := scanner.Err(); err != nil {
		return 0, false, fmt.Errorf("scan kernel TCP table: %w", err)
	}
	return 0, false, nil
}

func linuxSocketAddress(raw string, ipv6 bool) (netip.AddrPort, error) {
	address, port, ok := strings.Cut(raw, ":")
	expected := 8
	if ipv6 {
		expected = 32
	}
	if !ok || len(address) != expected {
		return netip.AddrPort{}, errors.New("malformed kernel socket address")
	}
	decoded := make([]byte, expected/2)
	for start := 0; start < len(decoded); start += 4 {
		word, err := strconv.ParseUint(address[start*2:start*2+8], 16, 32)
		if err != nil {
			return netip.AddrPort{}, fmt.Errorf("parse kernel socket IP: %w", err)
		}
		binary.NativeEndian.PutUint32(decoded[start:start+4], uint32(word))
	}
	number, err := strconv.ParseUint(port, 16, 16)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("parse kernel socket port: %w", err)
	}
	ip, _ := netip.AddrFromSlice(decoded)
	return netip.AddrPortFrom(ip.Unmap(), uint16(number)), nil
}
