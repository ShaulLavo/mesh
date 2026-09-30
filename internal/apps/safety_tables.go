package apps

import (
	"encoding/hex"
	"errors"
	"net/netip"
	"strconv"
	"strings"
)

const maximumListenerTableBytes = 4 << 20

func statAvailableBytes(blocks, size uint64) uint64 {
	if size == 0 {
		return 0
	}
	if blocks > ^uint64(0)/size {
		return ^uint64(0)
	}
	return blocks * size
}
func parseLinuxListeners(raw []byte, port int, ipv6 bool) ([]netip.Addr, error) {
	if len(raw) > maximumListenerTableBytes {
		return nil, errors.New("app: kernel TCP listener table exceeds inspection limit")
	}
	var addresses []netip.Addr
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] == "sl" {
			continue
		}
		if len(fields) < 4 {
			return nil, errors.New("app: malformed kernel TCP listener table")
		}
		if fields[3] != "0A" {
			continue
		}
		local := strings.Split(fields[1], ":")
		if len(local) != 2 {
			return nil, errors.New("app: malformed kernel listener address")
		}
		number, err := strconv.ParseUint(local[1], 16, 16)
		if err != nil {
			return nil, errors.New("app: malformed kernel listener port")
		}
		if int(number) != port {
			continue
		}
		address, err := linuxListenerAddress(local[0], ipv6)
		if err != nil {
			return nil, err
		}
		addresses = append(addresses, address)
	}
	return addresses, nil
}
func linuxListenerAddress(raw string, ipv6 bool) (netip.Addr, error) {
	expected := 8
	if ipv6 {
		expected = 32
	}
	if len(raw) != expected {
		return netip.Addr{}, errors.New("app: malformed kernel listener IP")
	}
	decoded, err := hex.DecodeString(raw)
	if err != nil {
		return netip.Addr{}, errors.New("app: malformed kernel listener IP")
	}
	// Linux renders each native 32-bit word as hex on the supported little-endian
	// amd64 and arm64 targets, including four words for IPv6 addresses.
	for start := 0; start < len(decoded); start += 4 {
		decoded[start], decoded[start+3] = decoded[start+3], decoded[start]
		decoded[start+1], decoded[start+2] = decoded[start+2], decoded[start+1]
	}
	address, ok := netip.AddrFromSlice(decoded)
	if !ok {
		return netip.Addr{}, errors.New("app: malformed kernel listener IP")
	}
	return address, nil
}
func parseDarwinListeners(raw []byte, port int) ([]netip.Addr, error) {
	if len(raw) > maximumListenerTableBytes {
		return nil, errors.New("app: macOS TCP listener table exceeds inspection limit")
	}
	var addresses []netip.Addr
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.HasPrefix(fields[0], "tcp") {
			continue
		}
		if len(fields) < 6 {
			return nil, errors.New("app: malformed macOS TCP listener table")
		}
		if fields[5] != "LISTEN" {
			continue
		}
		local := fields[3]
		separator := strings.LastIndex(local, ".")
		if separator < 0 {
			return nil, errors.New("app: malformed macOS listener address")
		}
		number, err := strconv.ParseUint(local[separator+1:], 10, 16)
		if err != nil {
			return nil, errors.New("app: malformed macOS listener port")
		}
		if int(number) != port {
			continue
		}
		rawAddress := local[:separator]
		if rawAddress == "*" {
			addresses = append(addresses, netip.IPv4Unspecified())
			continue
		}
		address, err := netip.ParseAddr(rawAddress)
		if err != nil {
			return nil, errors.New("app: nonnumeric macOS listener address")
		}
		addresses = append(addresses, address)
	}
	return addresses, nil
}

type listenerTableBuffer struct{ bytes []byte }

func (b *listenerTableBuffer) Write(raw []byte) (int, error) {
	if len(raw) > maximumListenerTableBytes-len(b.bytes) {
		return 0, errors.New("app: listener inspection exceeds byte limit")
	}
	b.bytes = append(b.bytes, raw...)
	return len(raw), nil
}
