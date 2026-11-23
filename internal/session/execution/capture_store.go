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

func segmentCapacity(b Binding) uint32 {
	if b.Mode() != KindPTY {
		return 256 << 10
	}
	return captureSegment
}

func retentionBytes(b Binding) uint64 {
	switch b.Mode() {
	case KindPipe:
		return 8 << 20
	case KindLine:
		return 1 << 20
	default:
		return captureSuffix
	}
}

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
	Stream      string    `json:"stream,omitempty"`
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
	return StreamResult{m.streamName(), m.Generation, offsetText(m.Earliest), offsetText(m.End), offsetText(m.Observed), m.EOF, m.CloseReason, m.Loss}
}
func (m manifest) streamName() string {
	if m.Stream == "" {
		return KindPTY
	}
	return m.Stream
}

func (m manifest) validate(b Binding) error {
	if m.Binding != b || !validToken(m.Generation) || m.Earliest > m.End || m.End > m.Observed || m.End-m.Earliest > retentionBytes(b) || uint64(len(m.Segments)) > retentionBytes(b)/uint64(segmentCapacity(b))+2 {
		return fmt.Errorf("execution: invalid capture manifest")
	}
	if err := validStreamClosure(m.Closed, m.EOF, m.CloseReason, m.Loss); err != nil {
		return executionBoundaryError("capture", err)
	}
	next := m.Earliest
	if len(m.Segments) > 0 {
		first := m.Segments[0]
		if first.Start > m.Earliest || m.Earliest-first.Start >= uint64(first.Size) {
			return fmt.Errorf("execution: invalid first capture segment")
		}
		next = first.Start
	}
	for _, s := range m.Segments {
		if s.Start != next || s.Size < 1 || s.Size > segmentCapacity(b) || !validDigest(s.Hash) || !validToken(s.Name) {
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
	if closed && reason != captureEOF && reason != "drain_timeout" && reason != "read_error" && reason != interrupted {
		return fmt.Errorf("execution: invalid capture closure")
	}
	if loss != "" && loss != "queue_overflow" && loss != captureStorageLoss && loss != captureRetention && loss != captureHostReboot {
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
func streamPath(b Binding, stream, name string) string {
	return attempt(b, filepath.Join(stream, name))
}

type fileIdentity struct{ info os.FileInfo }

func (i fileIdentity) matches(info os.FileInfo) bool {
	return i.info != nil && os.SameFile(i.info, info) && i.info.Size() == info.Size() && i.info.ModTime().Equal(info.ModTime())
}

type streamStore struct {
	files   *Files
	binding Binding
	stream  string
	// earlier marks an attempt from another boot: its reads salvage the
	// verified bytes instead of failing on records a power cut lost.
	earlier   bool
	gate      chan struct{}
	confirmed map[string]fileIdentity
}

func newStreamStore(f *Files, b Binding, stream string) *streamStore {
	return &streamStore{files: f, binding: b, stream: stream, gate: make(chan struct{}, 1), confirmed: make(map[string]fileIdentity)}
}
func (s *streamStore) path(name string) string { return streamPath(s.binding, s.stream, name) }

func (s *streamStore) load() (manifest, error) {
	var m manifest
	err := s.files.Read(s.path("manifest.json"), &m)
	if err != nil {
		return m, err
	}
	if m.streamName() != s.stream {
		return m, damaged(fmt.Errorf("execution: capture stream mismatch"))
	}
	return m, damaged(m.validate(s.binding))
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
	plan, err := s.planSettle(ctx, m)
	if err != nil {
		return err
	}
	if err := confirmInOrder(s.files, plan.segments, plan.manifests); err != nil {
		return executionBoundaryError("confirm capture", err)
	}
	s.confirmed = plan.keep
	return nil
}

// settlePlan is what settling a manifest needs: the identities to keep, and
// the files among them this store has not yet proved durable. The segments
// are already verified against their hashes.
type settlePlan struct {
	keep map[string]fileIdentity
	// segments are prerequisites of manifests, so they are confirmed in an
	// earlier round: a visible manifest does not prove that its writer
	// finished the segments' barriers.
	segments, manifests []string
}

// confirmInOrder confirms each group in its own round, a group only after
// the previous one is durable. Empty groups take no round.
func confirmInOrder(f *Files, groups ...[]string) error {
	for _, names := range groups {
		if len(names) == 0 {
			continue
		}
		if err := f.ConfirmAll(names...); err != nil {
			return err
		}
	}
	return nil
}

// planSettle verifies a manifest's files for settle. Segments of several
// streams are independent of one another, and so are their manifests, so
// callers may confirm each kind for many streams in one round.
func (s *streamStore) planSettle(ctx context.Context, m manifest) (settlePlan, error) {
	plan := settlePlan{keep: make(map[string]fileIdentity, len(m.Segments)+1)}
	for _, segment := range m.Segments {
		if err := ctx.Err(); err != nil {
			return plan, executionBoundaryError("capture", err)
		}
		name := s.path(segment.Name)
		identity, known, err := s.verifySegment(name, segment)
		if err != nil {
			return plan, executionBoundaryError("capture", err)
		}
		plan.keep[name] = identity
		if !known {
			plan.segments = append(plan.segments, name)
		}
	}
	name := s.path("manifest.json")
	identity, known, err := s.manifestIdentity(name)
	if err != nil {
		return plan, executionBoundaryError("confirm capture manifest", err)
	}
	plan.keep[name] = identity
	if !known {
		plan.manifests = append(plan.manifests, name)
	}
	return plan, nil
}

func (s *streamStore) confirmManifest(name string) (fileIdentity, error) {
	identity, known, err := s.manifestIdentity(name)
	if err != nil || known {
		return identity, err
	}
	return identity, s.files.Confirm(name)
}

// manifestIdentity reports whether a pipe manifest is unchanged since this
// store proved it durable.
func (s *streamStore) manifestIdentity(name string) (fileIdentity, bool, error) {
	info, err := s.identity(name)
	if err != nil {
		return fileIdentity{}, false, err
	}
	return fileIdentity{info}, s.binding.Mode() != KindPTY && s.confirmed[name].matches(info), nil
}

// Successful publication already synchronized both the file and its directory.
func (s *streamStore) rememberPublished(name string) {
	if s.binding.Mode() == KindPTY {
		return
	}
	if info, err := s.identity(name); err == nil {
		s.confirmed[name] = fileIdentity{info}
	}
}

// verifySegment checks a segment this store has not yet proved durable
// against its hash, and reports whether the store already had.
func (s *streamStore) verifySegment(name string, seg segment) (fileIdentity, bool, error) {
	info, err := s.identity(name)
	if err != nil {
		return fileIdentity{}, false, err
	}
	if s.confirmed[name].matches(info) {
		return fileIdentity{info}, true, nil
	}
	if err := s.readSegment(seg, nil); err != nil {
		return fileIdentity{}, false, err
	}
	return fileIdentity{info}, false, nil
}
func (s *streamStore) segmentBytes(seg segment) ([]byte, error) {
	data := make([]byte, 0, int(seg.Size))
	err := s.readSegment(seg, func(_ uint64, chunk []byte) { data = append(data, chunk...) })
	return data, err
}
func (s *streamStore) readSegment(seg segment, consume func(uint64, []byte)) (err error) {
	file, err := s.files.open(s.path(seg.Name), os.O_RDONLY)
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
			err = executionBoundaryError("read immutable segment", err)
			if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
				err = damaged(err)
			}
			return err
		}
		_, _ = hash.Write(chunk)
		if consume != nil {
			consume(seg.Start+uint64(offset), chunk)
		}
		offset += n
	}
	n, readErr := file.Read(buffer[:1])
	if n != 0 || readErr != io.EOF || hex.EncodeToString(hash.Sum(nil)) != seg.Hash {
		return damaged(fmt.Errorf("execution: damaged capture segment"))
	}
	return nil
}
func (s *streamStore) confirmedManifest(m manifest) bool {
	names := make([]string, 0, len(m.Segments)+1)
	names = append(names, s.path("manifest.json"))
	for _, seg := range m.Segments {
		names = append(names, s.path(seg.Name))
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
	entries, err := s.files.Names(s.path("."))
	if err != nil {
		return executionBoundaryError("capture", err)
	}
	for _, entry := range entries {
		if !validToken(entry.Name()) && !strings.HasPrefix(entry.Name(), ".tmp-") {
			continue
		}
		name := s.path(entry.Name())
		if _, keep := s.confirmed[name]; keep {
			continue
		}
		if err := s.files.Remove(name, s.path("manifest.json")); err != nil {
			return executionBoundaryError("capture", err)
		}
	}
	return nil
}

const captureEOF = "eof"
const captureStorageLoss = "storage_failure"
const captureRetention = "retention"
