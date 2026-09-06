package sshfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

const maximumSFTPFrame = 256 << 10

const (
	wireInit     = 1
	wireOpen     = 3
	wireWrite    = 6
	wireLstat    = 7
	wireSetstat  = 9
	wireFsetstat = 10
	wireOpendir  = 11
	wireRemove   = 13
	wireMkdir    = 14
	wireRmdir    = 15
	wireRealpath = 16
	wireStat     = 17
	wireRename   = 18
	wireReadlink = 19
	wireSymlink  = 20
	wireStatus   = 101
	wireExtended = 200
)

type guardedChannel struct {
	io.ReadWriteCloser
	incoming []byte
	writeMu  sync.Mutex
	outgoing []byte
}

// RequestServer cleans paths before calling handlers, which would hide a raw
// parent traversal. Check the original packets before giving them to the library.
func guardRequests(channel io.ReadWriteCloser) io.ReadWriteCloser {
	return &guardedChannel{ReadWriteCloser: channel}
}

func (channel *guardedChannel) Read(destination []byte) (int, error) {
	if len(destination) == 0 {
		return 0, nil
	}
	if len(channel.incoming) == 0 {
		packet, err := channel.nextRequest()
		if err != nil {
			return 0, err
		}
		channel.incoming = packet
	}
	n := copy(destination, channel.incoming)
	channel.incoming = channel.incoming[n:]
	return n, nil
}

func (channel *guardedChannel) nextRequest() ([]byte, error) {
	for {
		packet, err := readSFTPFrame(channel.ReadWriteCloser)
		if err != nil {
			return nil, err
		}
		denied, err := deniedSFTPRequest(packet[4:])
		if err != nil {
			return nil, err
		}
		if !denied {
			return packet, nil
		}
		if err := channel.deny(binary.BigEndian.Uint32(packet[5:9])); err != nil {
			return nil, err
		}
	}
}

func readSFTPFrame(reader io.Reader) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, err
	}
	size, err := sftpFrameSize(header[:])
	if err != nil {
		return nil, err
	}
	packet := make([]byte, size)
	copy(packet, header[:])
	if _, err := io.ReadFull(reader, packet[4:]); err != nil {
		return nil, fmt.Errorf("sshfs: read SFTP packet: %w", err)
	}
	return packet, nil
}

func sftpFrameSize(header []byte) (int, error) {
	length := binary.BigEndian.Uint32(header)
	if length < 1 || length > maximumSFTPFrame {
		return 0, fmt.Errorf("sshfs: invalid SFTP packet length %d", length)
	}
	return int(length) + 4, nil
}

func deniedSFTPRequest(packet []byte) (bool, error) {
	if len(packet) < 5 {
		return false, errors.New("sshfs: truncated SFTP request")
	}
	switch packet[0] {
	case wireWrite, wireSetstat, wireFsetstat, wireRemove, wireMkdir, wireRmdir, wireRename, wireSymlink, wireExtended:
		return true, nil
	case wireOpen, wireLstat, wireOpendir, wireRealpath, wireStat, wireReadlink:
		return deniedSFTPPath(packet)
	default:
		return false, nil
	}
}

func deniedSFTPPath(packet []byte) (bool, error) {
	if len(packet) < 9 {
		return false, errors.New("sshfs: truncated SFTP path")
	}
	length := binary.BigEndian.Uint32(packet[5:9])
	if int64(length) > int64(len(packet)-9) {
		return false, errors.New("sshfs: truncated SFTP path")
	}
	end := 9 + int(length)
	path := string(packet[9:end])
	if strings.ContainsAny(path, "\x00\\") || wirePathHasParent(path) {
		return true, nil
	}
	if packet[0] != wireOpen {
		return false, nil
	}
	if len(packet)-end < 8 {
		return false, errors.New("sshfs: truncated SFTP open flags or attributes")
	}
	return binary.BigEndian.Uint32(packet[end:end+4]) != 1, nil
}

func wirePathHasParent(path string) bool {
	for segment := range strings.SplitSeq(path, "/") {
		if segment == ".." {
			return true
		}
	}
	return false
}

// pkg/sftp writes DATA headers and payloads separately. Buffer complete frames
// so a denial sent by Read cannot land between those two writes.
func (channel *guardedChannel) Write(data []byte) (int, error) {
	channel.writeMu.Lock()
	defer channel.writeMu.Unlock()
	consumed := 0
	for len(data) > 0 {
		n, err := channel.writeFragment(data)
		consumed += n
		if err != nil {
			return consumed, err
		}
		data = data[n:]
	}
	return consumed, nil
}

func (channel *guardedChannel) writeFragment(data []byte) (int, error) {
	if len(channel.outgoing) < 4 {
		n := min(4-len(channel.outgoing), len(data))
		channel.outgoing = append(channel.outgoing, data[:n]...)
		return n, nil
	}
	size, err := sftpFrameSize(channel.outgoing[:4])
	if err != nil {
		return 0, err
	}
	n := min(size-len(channel.outgoing), len(data))
	channel.outgoing = append(channel.outgoing, data[:n]...)
	if len(channel.outgoing) < size {
		return n, nil
	}
	err = writeSFTPResponse(channel.ReadWriteCloser, channel.outgoing)
	channel.outgoing = channel.outgoing[:0]
	return n, err
}

func (channel *guardedChannel) deny(id uint32) error {
	// SSH_FXP_STATUS: ID, SSH_FX_PERMISSION_DENIED, message, language tag.
	packet := binary.BigEndian.AppendUint32(nil, 34)
	packet = append(packet, wireStatus)
	packet = binary.BigEndian.AppendUint32(packet, id)
	packet = binary.BigEndian.AppendUint32(packet, 3)
	packet = binary.BigEndian.AppendUint32(packet, 17)
	packet = append(packet, "permission denied"...)
	packet = binary.BigEndian.AppendUint32(packet, 0)
	channel.writeMu.Lock()
	defer channel.writeMu.Unlock()
	return writeSFTPResponse(channel.ReadWriteCloser, packet)
}

func writeSFTPResponse(writer io.Writer, packet []byte) error {
	n, err := writer.Write(packet)
	if err == nil && n != len(packet) {
		return io.ErrShortWrite
	}
	return err
}
