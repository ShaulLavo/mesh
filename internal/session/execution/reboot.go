package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// captureHostReboot marks output that a power cut or kernel crash lost: an
// earlier-boot stream whose records did not all reach the disk.
const captureHostReboot = "host_reboot"

// LossHostReboot is the loss a stream reports for bytes a reboot lost.
const LossHostReboot = captureHostReboot

// damageError marks a record that is present but unusable: torn, truncated,
// undecodable, or failing its hash. Only an attempt from an earlier boot may
// treat it as "not written"; the message is the underlying error's.
type damageError struct{ err error }

func (e *damageError) Error() string { return e.err.Error() }
func (e *damageError) Unwrap() error { return e.err }

func damaged(err error) error {
	if err == nil {
		return nil
	}
	return &damageError{err}
}

// lostRecord reports whether err means a record is missing or damaged. Lock
// timeouts, cancellation, permission errors and unsafe paths are not loss.
func lostRecord(err error) bool {
	var d *damageError
	return errors.Is(err, os.ErrNotExist) || errors.As(err, &d)
}

// earlierBoot reports whether b was reserved on a boot other than the current
// one. Both IDs must be known; an unknown boot never degrades a read.
func earlierBoot(current string, b Binding) bool {
	return current != "" && b.Reservation.BootID != "" && b.Reservation.BootID != current
}

// rebootGeneration names a manifest rebuilt after its records were lost. It
// is derived from the stream, so every reader of the same loss agrees on it.
func rebootGeneration(b Binding, stream string) string {
	sum := sha256.Sum256([]byte(b.Reservation.Instance + "/" + stream + "/" + captureHostReboot))
	return hex.EncodeToString(sum[:16])
}

// rebuiltManifest stands in for an earlier-boot manifest that is missing or
// damaged: closed, empty, and marked as lost at the reboot.
func (s *streamStore) rebuiltManifest() manifest {
	m := manifest{Binding: s.binding, Generation: rebootGeneration(s.binding, s.stream), Closed: true, CloseReason: interrupted, Loss: captureHostReboot}
	if s.binding.Mode() != KindPTY {
		m.Stream = s.stream
	}
	return m
}

// salvage reads an earlier-boot stream as the verified bytes that survived:
// the longest prefix of segments that verify, ending at the first missing or
// damaged one. A missing or damaged manifest becomes an empty one. Anything
// dropped closes the stream as interrupted with host_reboot loss. Expired
// output stays expired. Callers hold the stream's fence if it still exists.
func (s *streamStore) salvage(ctx context.Context) (manifest, error) {
	m, err := s.load()
	if err != nil {
		if !lostRecord(err) {
			return manifest{}, err
		}
		return s.rebuiltManifest(), nil
	}
	if m.Expired {
		return m, nil
	}
	kept := len(m.Segments)
	for i, seg := range m.Segments {
		if err := ctx.Err(); err != nil {
			return manifest{}, executionBoundaryError("salvage capture", err)
		}
		if err := s.readSegment(seg, nil); err != nil {
			if !lostRecord(err) {
				return manifest{}, err
			}
			kept = i
			break
		}
	}
	if kept == len(m.Segments) {
		return m, nil
	}
	m.Segments = m.Segments[:kept:kept]
	m.End = m.Earliest
	if kept > 0 {
		last := m.Segments[kept-1]
		m.End = last.Start + uint64(last.Size)
	}
	m.Closed, m.EOF, m.CloseReason, m.Loss = true, false, interrupted, captureHostReboot
	return m, nil
}

// rebootFence takes an earlier-boot stream's fence when its lock file
// survived. A lost lock file leaves nothing to coordinate with: the writer
// died with the boot.
func (s *streamStore) rebootFence(ctx context.Context, mode int) (*Fence, error) {
	gate, err := s.readerFence(ctx, mode)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return gate, err
}

// readRebootPage reads a page of an earlier-boot stream from its salvaged
// manifest. Salvage verifies every retained segment, so no settle follows.
func (s *streamStore) readRebootPage(ctx context.Context, value string, size int) (Page, error) {
	gate, err := s.rebootFence(ctx, unix.LOCK_SH)
	if err != nil {
		return Page{}, err
	}
	defer func() { _ = gate.Close() }()
	m, err := s.salvage(ctx)
	if err != nil {
		return Page{}, err
	}
	return s.rebootPage(m, value, size)
}

// rebootPage serves a page of a salvaged stream. A cursor of this job and
// stream whose generation was rebuilt, or whose offset is past an end that
// rolled back, gets the retained bytes from the start with a host_reboot gap
// instead of an error. Cursors of other jobs or streams keep their errors.
func (s *streamStore) rebootPage(m manifest, value string, size int) (Page, error) {
	if value != "" && s.rolledBack(m, value) {
		p, err := s.pageData(m, "", size)
		p.Gap, p.Reason = true, captureHostReboot
		return p, err
	}
	return s.pageData(m, value, size)
}

func (s *streamStore) rolledBack(m manifest, value string) bool {
	c, offset, err := readCursor(value)
	if err != nil {
		return false
	}
	b := s.binding
	if c.Host != b.Host || c.Session != b.Reservation.ID || c.Instance != b.Reservation.Instance || c.Stream != m.streamName() {
		return false
	}
	return c.Generation != m.Generation || offset > m.End
}

// rebootSummaries replaces each stream summary of an earlier-boot result
// with what its verified bytes support, and marks the capture incomplete
// when any stream lost bytes at the reboot.
func (o *Output) rebootSummaries(ctx context.Context, r Result) (Result, error) {
	streams := make([]StreamResult, 0, len(r.Streams))
	for _, expected := range r.Streams {
		summary, err := o.rebootSummary(ctx, r.Binding, expected)
		if err != nil {
			return Result{}, err
		}
		if summary.Loss == captureHostReboot {
			r.Capture = captureIncomplete
		}
		streams = append(streams, summary)
	}
	r.Streams = streams
	return r, nil
}

func (o *Output) rebootSummary(ctx context.Context, b Binding, expected StreamResult) (StreamResult, error) {
	s, err := o.stream(b, expected.Stream)
	if err != nil {
		return StreamResult{}, err
	}
	if err := s.enter(ctx); err != nil {
		return StreamResult{}, err
	}
	defer s.leave()
	gate, err := s.rebootFence(ctx, unix.LOCK_SH)
	if err != nil {
		return StreamResult{}, err
	}
	defer func() { _ = gate.Close() }()
	m, err := s.salvage(ctx)
	if err != nil {
		return StreamResult{}, err
	}
	if m.Expired || (m.Closed && m.summary() == expected) {
		return expected, nil
	}
	// The final manifest rolled back or lost bytes: report what survived.
	m.Closed, m.EOF, m.CloseReason, m.Loss = true, false, interrupted, captureHostReboot
	return m.summary(), nil
}
