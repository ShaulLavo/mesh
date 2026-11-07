package execution

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const captureQueue = 1 << 20
const captureSegment = 64 << 10
const captureSuffix = 16 << 20

type segment struct {
	Name  string `json:"name"`
	Start uint64 `json:"start,string"`
	Size  uint32 `json:"size"`
	Hash  string `json:"hash"`
}
type StreamResult struct {
	Stream      string `json:"stream"`
	Generation  string `json:"generation"`
	Earliest    string `json:"earliest"`
	End         string `json:"end"`
	ObservedEnd string `json:"observedEnd"`
	EOF         bool   `json:"eof"`
	CloseReason string `json:"closeReason"`
	Loss        string `json:"loss"`
}
type manifest struct {
	Binding     Binding   `json:"binding"`
	Generation  string    `json:"generation"`
	Earliest    uint64    `json:"earliest,string"`
	End         uint64    `json:"end,string"`
	Observed    uint64    `json:"observed,string"`
	Closed      bool      `json:"closed"`
	EOF         bool      `json:"eof"`
	Expired     bool      `json:"expired"`
	CloseReason string    `json:"closeReason"`
	Loss        string    `json:"loss"`
	Segments    []segment `json:"segments"`
}

func (m manifest) summary() StreamResult {
	return StreamResult{ptyStream, m.Generation, offsetText(m.Earliest), offsetText(m.End), offsetText(m.Observed), m.EOF, m.CloseReason, m.Loss}
}
func (m manifest) validate(b Binding) error {
	if m.Binding != b || !validToken(m.Generation) || m.Earliest > m.End || m.End > m.Observed || m.End-m.Earliest > captureSuffix || len(m.Segments) > captureSuffix/captureSegment+1 {
		return fmt.Errorf("execution: invalid capture manifest")
	}
	if err := validStreamClosure(m.Closed, m.EOF, m.CloseReason, m.Loss); err != nil {
		return executionBoundaryError("capture", err)
	}
	next := m.Earliest
	for _, s := range m.Segments {
		if s.Start != next || s.Size < 1 || s.Size > captureSegment || !validDigest(s.Hash) || !validToken(s.Name) {
			return fmt.Errorf("execution: invalid capture segment")
		}
		next += uint64(s.Size)
	}
	if next != m.End {
		return fmt.Errorf("execution: noncontiguous capture manifest")
	}
	return nil
}
func validStreamClosure(closed, eof bool, reason, loss string) error {
	if eof && (!closed || reason != captureEOF) {
		return fmt.Errorf("execution: contradictory capture EOF")
	}
	if !closed && reason != "" {
		return fmt.Errorf("execution: open capture has closure")
	}
	if closed && reason != captureEOF && reason != "drain_timeout" && reason != "read_error" && reason != "interrupted" {
		return fmt.Errorf("execution: invalid capture closure")
	}
	if loss != "" && loss != "queue_overflow" && loss != captureStorageLoss && loss != captureRetention {
		return fmt.Errorf("execution: invalid capture loss")
	}
	return nil
}
func validToken(s string) bool {
	raw, err := hex.DecodeString(s)
	return err == nil && len(raw) == 16 && hex.EncodeToString(raw) == s
}
func freshToken() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", executionBoundaryError("capture", err)
	}
	return hex.EncodeToString(raw[:]), nil
}
func streamPath(b Binding, name string) string { return attempt(b, filepath.Join(ptyStream, name)) }

type fileIdentity struct{ info os.FileInfo }

func (i fileIdentity) matches(info os.FileInfo) bool {
	return i.info != nil && os.SameFile(i.info, info) && i.info.Size() == info.Size() && i.info.ModTime().Equal(info.ModTime())
}

type streamStore struct {
	files     *Files
	binding   Binding
	gate      chan struct{}
	confirmed map[string]fileIdentity
}

func newStreamStore(f *Files, b Binding) *streamStore {
	return &streamStore{files: f, binding: b, gate: make(chan struct{}, 1), confirmed: make(map[string]fileIdentity)}
}
func (s *streamStore) load() (manifest, error) {
	var m manifest
	err := s.files.Read(streamPath(s.binding, "manifest.json"), &m)
	if err != nil {
		return m, err
	}
	return m, m.validate(s.binding)
}
func (s *streamStore) identity(name string) (os.FileInfo, error) {
	file, err := s.files.open(name, os.O_RDONLY)
	if err != nil {
		return nil, executionBoundaryError("capture", err)
	}
	info, err := file.Stat()
	return info, errors.Join(err, file.Close())
}

// settle is the only certification and pruning authority. Callers hold the exclusive stream lock.
func (s *streamStore) settle(ctx context.Context, m manifest) error {
	keep := make(map[string]fileIdentity, len(m.Segments)+1)
	for _, segment := range m.Segments {
		if err := ctx.Err(); err != nil {
			return executionBoundaryError("capture", err)
		}
		name := streamPath(s.binding, segment.Name)
		identity, err := s.confirmSegment(name, segment)
		if err != nil {
			return executionBoundaryError("capture", err)
		}
		keep[name] = identity
	}
	name := streamPath(s.binding, "manifest.json")
	if err := s.files.Confirm(name); err != nil {
		return executionBoundaryError("capture", err)
	}
	info, err := s.identity(name)
	if err != nil {
		return executionBoundaryError("capture", err)
	}
	keep[name] = fileIdentity{info}
	s.confirmed = keep
	return nil
}
func (s *streamStore) confirmSegment(name string, seg segment) (fileIdentity, error) {
	info, err := s.identity(name)
	if err != nil {
		return fileIdentity{}, err
	}
	if s.confirmed[name].matches(info) {
		return fileIdentity{info}, nil
	}
	if err := s.readSegment(seg, nil); err != nil {
		return fileIdentity{}, err
	}
	if err := s.files.Confirm(name); err != nil {
		return fileIdentity{}, err
	}
	return fileIdentity{info}, nil
}
func (s *streamStore) segmentBytes(seg segment) ([]byte, error) {
	data := make([]byte, 0, int(seg.Size))
	err := s.readSegment(seg, func(_ uint64, chunk []byte) { data = append(data, chunk...) })
	return data, err
}
func (s *streamStore) readSegment(seg segment, consume func(uint64, []byte)) (err error) {
	file, err := s.files.open(streamPath(s.binding, seg.Name), os.O_RDONLY)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, s.files.closeFile(file)) }()
	hash := sha256.New()
	buffer := make([]byte, 32<<10)
	for offset := uint32(0); offset < seg.Size; {
		n := min(uint32(32<<10), seg.Size-offset)
		chunk := buffer[:n]
		if _, err := io.ReadFull(file, chunk); err != nil {
			return executionBoundaryError("read immutable segment", err)
		}
		_, _ = hash.Write(chunk)
		if consume != nil {
			consume(seg.Start+uint64(offset), chunk)
		}
		offset += n
	}
	n, readErr := file.Read(buffer[:1])
	if n != 0 || readErr != io.EOF || hex.EncodeToString(hash.Sum(nil)) != seg.Hash {
		return fmt.Errorf("execution: damaged capture segment")
	}
	return nil
}
func (s *streamStore) confirmedManifest(m manifest) bool {
	names := make([]string, 0, len(m.Segments)+1)
	names = append(names, streamPath(s.binding, "manifest.json"))
	for _, seg := range m.Segments {
		names = append(names, streamPath(s.binding, seg.Name))
	}
	for _, name := range names {
		info, err := s.identity(name)
		if err != nil || !s.confirmed[name].matches(info) {
			return false
		}
	}
	return true
}
func (s *streamStore) prune(ctx context.Context, m manifest) error {
	if err := s.settle(ctx, m); err != nil {
		return executionBoundaryError("capture", err)
	}
	entries, err := s.files.Names(streamPath(s.binding, "."))
	if err != nil {
		return executionBoundaryError("capture", err)
	}
	for _, entry := range entries {
		if !validToken(entry.Name()) && !strings.HasPrefix(entry.Name(), ".tmp-") {
			continue
		}
		name := streamPath(s.binding, entry.Name())
		if _, keep := s.confirmed[name]; keep {
			continue
		}
		if err := s.files.Remove(name, streamPath(s.binding, "manifest.json")); err != nil {
			return executionBoundaryError("capture", err)
		}
	}
	return nil
}

const ptyStream = "pty"
const captureEOF = "eof"
const captureStorageLoss = "storage_failure"
const captureRetention = "retention"
