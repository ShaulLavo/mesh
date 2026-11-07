package execution

import (
	"context"
	"errors"
	"fmt"
	"github.com/shaul/mesh/internal/session"
	"os"
)

type dispatch struct {
	Binding Binding `json:"binding"`
	Key     Key     `json:"key"`
	Intent  Intent  `json:"intent"`
}

// Attempt holds the lifetime fence until the worker's last deferred pathname action.
type Attempt struct {
	files   *Files
	binding Binding
	fence   *Fence
}

func StartAttempt(ctx context.Context, state string, b Binding, i Intent, env []string) (*Attempt, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
	f, err := OpenFiles(state)
	if err != nil {
		return nil, err
	}
	fence, err := f.Lock(ctx, attempt(b, "lifetime.lock"))
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	a := &Attempt{files: f, binding: b, fence: fence}
	if err := a.claim(i, env); err != nil {
		_ = a.Close()
		return nil, err
	}
	return a, nil
}
func (a *Attempt) claim(i Intent, env []string) error {
	b := a.binding
	var ref Binding
	if err := a.files.Read(attempt(b, "reference.json"), &ref); err != nil {
		return err
	}
	if ref != b {
		return fmt.Errorf("execution: attempt reference mismatch")
	}
	var r Receipt
	if err := a.files.Read(requestName(b.RequestHash), &r); err != nil {
		return err
	}
	if err := r.validate(); err != nil {
		return err
	}
	if r.Binding != b || r.Removal != removalPresent || r.Phase != phaseDispatched {
		return ErrProtected
	}
	if err := a.files.Confirm(requestName(b.RequestHash)); err != nil {
		return err
	}
	var d dispatch
	if err := a.files.Read(attempt(b, "dispatch.json"), &d); err != nil {
		return err
	}
	if d.Binding != b || d.Key != r.Key || digest(d.Intent) != digest(i) || NewBinding(d.Key, i, b.Reservation, env) != b {
		return fmt.Errorf("execution: dispatch intent mismatch")
	}
	if err := a.files.Confirm(attempt(b, "dispatch.json")); err != nil {
		return err
	}
	_, err := a.files.Publish(attempt(b, "claim.json"), b, true)
	return err
}
func (a *Attempt) Finish(result Result) error {
	if err := result.validate(a.binding); err != nil {
		return err
	}
	_, err := a.files.Publish(attempt(a.binding, "result.json"), result, true)
	return err
}
func (a *Attempt) Published() error {
	return a.files.Remove("s/"+a.binding.Reservation.ID+"/.launching", attempt(a.binding, "reference.json"))
}
func (a *Attempt) Close() error {
	// Every caller defer, including Unix socket cleanup, has already finished.
	err := a.files.syncDirs("s/" + a.binding.Reservation.ID + "/sock")
	if err == nil {
		err = a.files.Confirm(attempt(a.binding, "reference.json"))
	}
	return errors.Join(err, a.files.Close(), a.fence.Close())
}

// ProtectedSession checks durable origin before catalog markers can authorize deletion.
// Damaged or unreadable origin is protection, not permission to forget evidence.
func ProtectedSession(sessions, id string) bool {
	f, err := OpenFiles(sessions)
	if err != nil {
		return !errors.Is(err, os.ErrNotExist)
	}
	defer func() { _ = f.Close() }()
	var r session.Reservation
	err = f.Read(id+"/.reservation", &r)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	return err != nil || r.Purpose == "execution"
}
