package execution

import (
	"context"
	"errors"
	"os"
	"time"
)

func (j *Journal) prepareOutputAdmission(ctx context.Context, k Key) error {
	gate, err := j.lockAdmitted(ctx)
	if err != nil {
		return err
	}
	recordErr := j.files.check(requestName(k.Hash()), false)
	if recordErr == nil {
		return gate.Close()
	}
	if !errors.Is(recordErr, os.ErrNotExist) {
		return errors.Join(recordErr, gate.Close())
	}
	entries, err := j.files.Names("execution/requests")
	err = errors.Join(err, gate.Close())
	if err != nil {
		return err
	}
	// Every slot is one entry, so fewer entries than the active limit can never
	// need eviction. Observing each receipt first made a create read them all.
	if len(entries) < j.maxActive {
		return nil
	}
	candidate, slots := j.outputCandidate(ctx, entries)
	if slots < j.maxActive || candidate.Binding.Reservation.Instance == "" {
		return nil
	}
	if err := admitted(ctx); err != nil {
		return err
	}
	return j.expireOutput(ctx, candidate.Binding)
}
func (j *Journal) outputCandidate(ctx context.Context, entries []os.DirEntry) (Receipt, int) {
	var oldest Receipt
	slots := 0
	for _, entry := range entries {
		name := entry.Name()
		if len(name) != 69 {
			continue
		}
		r, err := j.observe(ctx, name[:64])
		if err != nil {
			slots++
			continue
		}
		if r.Phase != phaseTerminal {
			slots++
			continue
		}
		if r.OutputRetainUntil == nil || r.OutputExpired {
			continue
		}
		slots++
		if oldest.TerminalAt == nil || r.TerminalAt.Before(*oldest.TerminalAt) {
			oldest = r
		}
	}
	return oldest, slots
}
func (j *Journal) expireOutput(ctx context.Context, b Binding) error {
	fence, err := j.files.LockExisting(ctx, attempt(b, "lifetime.lock"))
	if err != nil {
		return err
	}
	defer func() { _ = fence.Close() }()
	if err := j.markOutputExpired(ctx, b); err != nil {
		return err
	}
	return j.deleteOutputBytes(ctx, b)
}
func (j *Journal) markOutputExpired(ctx context.Context, b Binding) error {
	gate, err := j.lock(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = gate.Close() }()
	r, err := j.read(b.RequestHash)
	if err != nil {
		return err
	}
	if r.Binding != b || r.Phase != phaseTerminal || r.Result == nil {
		return ErrProtected
	}
	r.OutputExpired = true
	_, err = j.save(r)
	return err
}
func (j *Journal) deleteOutputBytes(ctx context.Context, b Binding) error {
	for _, stream := range b.Streams() {
		if err := j.deleteStreamBytes(ctx, b, stream); err != nil {
			return err
		}
	}
	return nil
}

func (j *Journal) deleteStreamBytes(ctx context.Context, b Binding, stream string) error {
	s := newStreamStore(j.files, b, stream)
	gate, err := j.files.LockExisting(ctx, s.path("stream.lock"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = gate.Close() }()
	m, err := s.load()
	if err != nil {
		return err
	}
	if err := s.settle(ctx, m); err != nil {
		return err
	}
	if !m.Closed {
		if b.Mode() == KindPTY {
			return ErrProtected
		}
		m.Closed, m.CloseReason = true, interrupted
	}
	m.Expired = true
	m.Earliest = m.End
	m.Segments = nil
	if _, err := j.files.Publish(s.path("manifest.json"), m, false); err != nil {
		return err
	}
	return s.prune(ctx, m)
}
func (j *Journal) retireOutputPayload(ctx context.Context, b Binding) error {
	if err := j.deleteOutputBytes(ctx, b); err != nil {
		return err
	}
	return j.files.Remove(attempt(b, "result.json"), "execution/.barrier")
}
func outputDue(r Receipt, now time.Time) bool {
	return r.Phase == phaseTerminal && r.OutputRetainUntil != nil && (r.OutputExpired || !now.Before(*r.OutputRetainUntil))
}

func (j *Journal) interruptedPipe(ctx context.Context, b Binding) (Result, error) {
	r := Result{Binding: b, Termination: Termination{Kind: interrupted, Reason: "host_reboot"}, Capture: "incomplete"}
	for _, stream := range b.Streams() {
		s := newStreamStore(j.files, b, stream)
		summary, err := s.interrupted(ctx)
		if err != nil {
			return Result{}, err
		}
		r.Streams = append(r.Streams, summary)
	}
	return r, nil
}

func (s *streamStore) interrupted(ctx context.Context) (StreamResult, error) {
	gate, err := s.files.LockExisting(ctx, s.path("stream.lock"))
	if err != nil {
		return StreamResult{}, err
	}
	defer func() { _ = gate.Close() }()
	m, err := s.load()
	if err != nil {
		return StreamResult{}, err
	}
	if !m.Closed {
		m.Closed, m.CloseReason = true, interrupted
		if _, err := s.files.Publish(s.path("manifest.json"), m, false); err != nil {
			return StreamResult{}, err
		}
	}
	return m.summary(), s.prune(ctx, m)
}
