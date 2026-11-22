package execution

import (
	"context"
	"crypto/subtle"
	"fmt"
)

type admissionKey struct{}

// WithAdmission attaches the caller's grant check to ctx. The journal runs it
// after every lock wait and before its first effect, so a grant revoked while
// a request waited for the journal cannot still change it.
func WithAdmission(ctx context.Context, admit func() error) context.Context {
	return context.WithValue(ctx, admissionKey{}, admit)
}

func admitted(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("execution admission: %w", err)
	}
	if admit, ok := ctx.Value(admissionKey{}).(func() error); ok && admit != nil {
		return admit()
	}
	return nil
}

// lockAdmitted takes the journal lock and rechecks admission behind it.
func (j *Journal) lockAdmitted(ctx context.Context) (*Fence, error) {
	gate, err := j.lock(ctx)
	if err != nil {
		return nil, err
	}
	if err := admitted(ctx); err != nil {
		_ = gate.Close()
		return nil, err
	}
	return gate, nil
}

// SameOwner compares persisted application IDs in constant time.
func SameOwner(stored, presented string) bool {
	return stored != "" && subtle.ConstantTimeCompare([]byte(stored), []byte(presented)) == 1
}

// Peek reads one exact receipt without refreshing it, so a caller can be
// authorized before any journal effect.
func (j *Journal) Peek(ctx context.Context, b Binding) (Receipt, error) {
	r, err := j.snapshot(ctx, b.RequestHash)
	if err != nil {
		return r, err
	}
	if r.Binding != b {
		return Receipt{}, ErrInstance
	}
	return r, nil
}
