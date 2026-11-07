package execution

import (
	"context"
	"errors"
	"os"
	"time"
)

func (j *Journal) prepareOutputAdmission(ctx context.Context, k Key) error {
	gate, err := j.lock(ctx)
	if err != nil {
		return err
	}
	var candidate Receipt
	if err := j.files.check(requestName(k.Hash()), false); err == nil {
		return gate.Close()
	}
	entries, err := j.files.Names("execution/requests")
	slots := 0
	if err == nil {
		candidate, slots = j.outputCandidate(entries)
	}
	err = errors.Join(err, gate.Close())
	if err != nil {
		return err
	}
	if slots < j.maxActive || candidate.Binding.Reservation.Instance == "" {
		return nil
	}
	return j.expireOutput(ctx, candidate.Binding)
}
func (j *Journal) outputCandidate(entries []os.DirEntry) (Receipt, int) {
	var oldest Receipt
	slots := 0
	for _, entry := range entries {
		name := entry.Name()
		if len(name) != 69 {
			continue
		}
		r, err := j.read(name[:64])
		if err != nil {
			slots++
			continue
		}
		r, err = j.refresh(r)
		if err != nil || r.Phase != phaseTerminal {
			slots++
			continue
		}
		if len(r.Result.Streams) == 0 || r.OutputExpired {
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
	s := newStreamStore(j.files, b)
	gate, err := j.files.LockExisting(ctx, streamPath(b, "stream.lock"))
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
		return ErrProtected
	}
	m.Expired = true
	m.Earliest = m.End
	m.Segments = nil
	if _, err := j.files.Publish(streamPath(b, "manifest.json"), m, false); err != nil {
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
	return r.Phase == phaseTerminal && r.Result != nil && len(r.Result.Streams) > 0 && (r.OutputExpired || (r.OutputRetainUntil != nil && !now.Before(*r.OutputRetainUntil)))
}
