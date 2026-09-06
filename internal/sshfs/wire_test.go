package sshfs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"testing/iotest"
)

type wireTestChannel struct {
	io.Reader
	output bytes.Buffer
	closed bool
}

func (channel *wireTestChannel) Write(data []byte) (int, error) {
	return channel.output.Write(data)
}

func (channel *wireTestChannel) Close() error {
	channel.closed = true
	return nil
}

func TestWireGuardPreservesFragmentedRequests(t *testing.T) {
	requests := bytes.Join([][]byte{
		wireTestFrame(wireInit, 3, nil),
		wireTestPath(wireOpen, 1, "/blog/readme", 1),
		wireTestPath(wireOpendir, 2, "/", 0),
		wireTestPath(wireLstat, 3, "blog/./readme", 0),
		wireTestPath(wireRealpath, 4, ".", 0),
		wireTestPath(wireStat, 5, "blog/name..txt", 0),
		wireTestPath(wireReadlink, 6, "/blog/link", 0),
		wireTestFrame(5, 7, nil),
		wireTestFrame(4, 8, nil),
	}, nil)
	underlying := &wireTestChannel{Reader: iotest.OneByteReader(bytes.NewReader(requests))}
	guard := guardRequests(underlying)
	got, err := io.ReadAll(iotest.OneByteReader(guard))
	if err != nil || !bytes.Equal(got, requests) {
		t.Fatalf("forwarded %x, error %v; want %x", got, err, requests)
	}
	if underlying.output.Len() != 0 || underlying.closed {
		t.Fatalf("unexpected response or close: %x, closed %v", underlying.output.Bytes(), underlying.closed)
	}
	if err := guard.Close(); err != nil || !underlying.closed {
		t.Fatalf("close error %v, underlying closed %v", err, underlying.closed)
	}
}

func TestWireGuardDeniesUnsafePathsAndContinues(t *testing.T) {
	operations := []byte{wireOpen, wireLstat, wireOpendir, wireRealpath, wireStat, wireReadlink}
	paths := []string{"..", "/..", "../blog/readme", "/blog/../other", "/blog/nested/../../other", "/blog/name\x00", "/blog/..\\other"}
	for _, operation := range operations {
		for _, path := range paths {
			t.Run(fmt.Sprintf("%d/%q", operation, path), func(t *testing.T) {
				assertWireDenial(t, wireTestPath(operation, 42, path, 1))
			})
		}
	}
}

func TestWireGuardDeniesMutationsAndOpenFlags(t *testing.T) {
	operations := []byte{wireWrite, wireSetstat, wireFsetstat, wireRemove, wireMkdir, wireRmdir, wireRename, wireSymlink, wireExtended}
	for _, operation := range operations {
		t.Run(fmt.Sprint(operation), func(t *testing.T) {
			assertWireDenial(t, wireTestPath(operation, 42, "/blog/readme", 0))
		})
	}
	for _, flags := range []uint32{0, 2, 3, 4, 8, 16, 32, 33, 64, 65, 0xffffffff} {
		t.Run(fmt.Sprintf("flags=%d", flags), func(t *testing.T) {
			assertWireDenial(t, wireTestPath(wireOpen, 42, "/blog/readme", flags))
		})
	}
}

func assertWireDenial(t *testing.T, denied []byte) {
	t.Helper()
	allowed := wireTestPath(wireStat, 43, "/blog/readme", 0)
	underlying := &wireTestChannel{Reader: iotest.OneByteReader(bytes.NewReader(bytes.Join([][]byte{denied, allowed}, nil)))}
	got, err := io.ReadAll(guardRequests(underlying))
	if err != nil || !bytes.Equal(got, allowed) {
		t.Fatalf("forwarded %x, error %v; want %x", got, err, allowed)
	}
	status, err := readSFTPFrame(&underlying.output)
	if err != nil {
		t.Fatal(err)
	}
	if status[4] != wireStatus || binary.BigEndian.Uint32(status[5:9]) != 42 || binary.BigEndian.Uint32(status[9:13]) != 3 {
		t.Fatalf("denial response = %x", status)
	}
	if underlying.output.Len() != 0 || underlying.closed {
		t.Fatalf("unexpected extra response or close: %x, closed %v", underlying.output.Bytes(), underlying.closed)
	}
}

func TestWireGuardRejectsMalformedAndOversizedRequests(t *testing.T) {
	badPath := wireTestFrame(wireStat, 1, binary.BigEndian.AppendUint32(nil, 0xffffffff))
	open := wireTestPath(wireOpen, 1, "/blog/readme", 1)
	open = open[:len(open)-4]
	binary.BigEndian.PutUint32(open[:4], uint32(len(open)-4)) //nolint:gosec // the fixture is smaller than 100 bytes
	tests := map[string][]byte{
		"partial header":  {0, 0},
		"empty packet":    {0, 0, 0, 0},
		"oversized":       binary.BigEndian.AppendUint32(nil, maximumSFTPFrame+1),
		"max uint32":      binary.BigEndian.AppendUint32(nil, 0xffffffff),
		"truncated body":  {0, 0, 0, 5, wireStat},
		"missing ID":      {0, 0, 0, 1, wireStat},
		"missing path":    wireTestFrame(wireStat, 1, nil),
		"oversized path":  badPath,
		"open attributes": open,
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			underlying := &wireTestChannel{Reader: bytes.NewReader(input)}
			got, err := io.ReadAll(guardRequests(underlying))
			if err == nil || len(got) != 0 || underlying.output.Len() != 0 {
				t.Fatalf("forwarded %x, error %v, response %x", got, err, underlying.output.Bytes())
			}
		})
	}
}

func TestWireGuardBoundsPacketsBeforeReadingBody(t *testing.T) {
	input := wireTestFrame(wireInit, 3, make([]byte, maximumSFTPFrame-5))
	underlying := &wireTestChannel{Reader: bytes.NewReader(input)}
	got, err := io.ReadAll(guardRequests(underlying))
	if err != nil || !bytes.Equal(got, input) {
		t.Fatalf("maximum packet: forwarded %d bytes, error %v", len(got), err)
	}
	underlying.Reader = bytes.NewReader(binary.BigEndian.AppendUint32(nil, maximumSFTPFrame+1))
	_, err = io.ReadAll(guardRequests(underlying))
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("oversized packet length should fail before reading body, got %v", err)
	}
}

func TestWireGuardSerializesDenialsBetweenWholeResponses(t *testing.T) {
	denied := wireTestPath(wireStat, 42, "/blog/../other", 0)
	underlying := &wireTestChannel{Reader: bytes.NewReader(denied)}
	guard := guardRequests(underlying)
	response := wireTestFrame(103, 7, []byte("a file payload"))
	if _, err := guard.Write(response[:9]); err != nil {
		t.Fatal(err)
	}
	if underlying.output.Len() != 0 {
		t.Fatal("partial response escaped the frame buffer")
	}
	var operations sync.WaitGroup
	operations.Go(func() {
		if _, err := io.ReadAll(guard); err != nil {
			t.Error(err)
		}
	})
	operations.Go(func() {
		for _, value := range response[9:] {
			if _, err := guard.Write([]byte{value}); err != nil {
				t.Error(err)
			}
		}
	})
	operations.Wait()
	seen := make(map[uint32][]byte)
	for underlying.output.Len() > 0 {
		packet, err := readSFTPFrame(&underlying.output)
		if err != nil {
			t.Fatal(err)
		}
		seen[binary.BigEndian.Uint32(packet[5:9])] = packet
	}
	if len(seen) != 2 || !bytes.Equal(seen[7], response) || len(seen[42]) == 0 || seen[42][4] != wireStatus {
		t.Fatalf("interleaved responses: %x", seen)
	}
}

func TestWireGuardWritesMultipleResponsesInOneCall(t *testing.T) {
	underlying := &wireTestChannel{Reader: bytes.NewReader(nil)}
	guard := guardRequests(underlying)
	response := bytes.Repeat(wireTestFrame(wireStatus, 7, nil), 3)
	n, err := guard.Write(response)
	if err != nil || n != len(response) || !bytes.Equal(underlying.output.Bytes(), response) {
		t.Fatalf("wrote %d bytes, error %v, response %x", n, err, underlying.output.Bytes())
	}
}

func wireTestPath(operation byte, id uint32, path string, flags uint32) []byte {
	payload := binary.BigEndian.AppendUint32(nil, uint32(len(path))) //nolint:gosec // test paths are bounded fixtures
	payload = append(payload, path...)
	if operation == wireOpen {
		payload = binary.BigEndian.AppendUint32(payload, flags)
		payload = binary.BigEndian.AppendUint32(payload, 0)
	}
	return wireTestFrame(operation, id, payload)
}

func wireTestFrame(operation byte, id uint32, payload []byte) []byte {
	packet := binary.BigEndian.AppendUint32(nil, uint32(5+len(payload))) //nolint:gosec // test packets are bounded fixtures
	packet = append(packet, operation)
	packet = binary.BigEndian.AppendUint32(packet, id)
	return append(packet, payload...)
}
