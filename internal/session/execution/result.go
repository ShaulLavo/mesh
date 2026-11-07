package execution

import (
	"context"
	"encoding/json"
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
	fence, err := f.LockExisting(ctx, attempt(b, "lifetime.lock"))
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	a := &Attempt{files: f, binding: b, fence: fence}
	if err := a.claim(i, env); err != nil {
		return nil, errors.Join(err, f.Close(), fence.Close())
	}
	return a, nil
}
func (a *Attempt) claim(i Intent, env []string) error {
	b := a.binding
	if err := a.currentReservation(); err != nil {
		return err
	}
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
		return metadataExecutionOrigin(f, id)
	}
	return err != nil || r.Validate() != nil || r.Purpose == session.Execution
}

func (a *Attempt) currentReservation() error {
	b := a.binding
	for _, name := range []string{".reservation", ".launching"} {
		path := "s/" + b.Reservation.ID + "/" + name
		var ref session.Reservation
		if err := a.files.Read(path, &ref); err != nil {
			return err
		}
		if ref != b.Reservation {
			return fmt.Errorf("execution: session allocation changed")
		}
		if err := a.files.Confirm(path); err != nil {
			return err
		}
	}
	return nil
}

func metadataExecutionOrigin(f *Files, id string) bool {
	var meta map[string]json.RawMessage
	if err := f.Read(id+"/meta.json", &meta); err != nil {
		return false
	}
	raw, exists := meta["reservation"]
	if !exists {
		return false
	}
	var ref session.Reservation
	if err := json.Unmarshal(raw, &ref); err != nil {
		return true
	}
	return ref.Validate() != nil || ref.Purpose == session.Execution
}
