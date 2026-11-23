package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/session/execution"
)

// Reserve is the only writer of launch allocations, including explicit recovery.
func Reserve(root string, purpose session.Purpose) (session.Reservation, error) {
	return ReservePreferring(root, purpose, "")
}

// ReservePreferring reserves preferred first when it is a free session ID, so
// a standby worker started under that ID can serve the session, and any
// fresh ID otherwise. Creating the directory decides who gets an ID, so two
// reservations never share one.
func ReservePreferring(root string, purpose session.Purpose, preferred string) (session.Reservation, error) {
	if err := os.MkdirAll(root, 0700); err != nil {
		return session.Reservation{}, workerExecutionError("reservation.go Reserve", err)
	}
	for attempt := range sessionIDAttempts {
		id, err := reservationCandidate(attempt, preferred)
		if err != nil {
			return session.Reservation{}, workerExecutionError("reservation.go Reserve", err)
		}
		ref, err := session.NewReservation(id, BootID(), purpose)
		if err != nil {
			return ref, workerExecutionError("reservation.go Reserve", err)
		}
		err = os.Mkdir(filepath.Join(root, id), 0700)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return ref, workerExecutionError("reservation.go Reserve", err)
		}
		if err := publishReservation(root, ref); err != nil {
			return ref, err
		}
		return ref, nil
	}
	return session.Reservation{}, fmt.Errorf("launch worker: exhausted session IDs")
}
func reservationCandidate(attempt int, preferred string) (string, error) {
	if attempt == 0 && preferred != "" {
		return preferred, nil
	}
	return session.NewID() //nolint:wrapcheck // Reserve names the failure
}

func publishReservation(root string, ref session.Reservation) error {
	if ref.Purpose == session.Managed || ref.Purpose.IsExecution() || ref.Purpose == session.Recovery {
		return strictReservation(root, ref)
	}
	data, err := json.Marshal(ref)
	if err != nil {
		return workerExecutionError("reservation.go publishReservation", err)
	}
	dir := filepath.Join(root, ref.ID)
	if err := os.WriteFile(filepath.Join(dir, ".reservation"), data, 0600); err != nil {
		return workerExecutionError("reservation.go publishReservation", err)
	}
	return workerExecutionError("reservation.go publishReservation", os.WriteFile(filepath.Join(dir, ".launching"), data, 0600))
}
func strictReservation(root string, ref session.Reservation) error {
	f, err := execution.OpenFiles(filepath.Dir(root))
	if err != nil {
		return workerExecutionError("reservation.go strictReservation", err)
	}
	defer func() { _ = f.Close() }()
	dir := filepath.Join(filepath.Base(root), ref.ID)
	if err := f.Ensure(dir); err != nil {
		return workerExecutionError("reservation.go strictReservation", err)
	}
	if _, err := f.Publish(filepath.Join(dir, ".reservation"), ref, true); err != nil {
		return workerExecutionError("reservation.go strictReservation", err)
	}
	_, err = f.Publish(filepath.Join(dir, ".launching"), ref, true)
	return workerExecutionError("reservation.go strictReservation", err)
}
func validateReservation(root string, ref session.Reservation) error {
	if err := ref.Validate(); err != nil {
		return workerExecutionError("reservation.go validateReservation", err)
	}
	f, err := execution.OpenFiles(root)
	if err != nil {
		return workerExecutionError("reservation.go validateReservation", err)
	}
	defer func() { _ = f.Close() }()
	for _, name := range []string{".reservation", ".launching"} {
		var saved session.Reservation
		if err := f.Read(filepath.Join(ref.ID, name), &saved); err != nil {
			return workerExecutionError("reservation.go validateReservation", err)
		}
		if saved != ref {
			return fmt.Errorf("launch worker: reservation identity mismatch")
		}
		if ref.Purpose != session.Managed && !ref.Purpose.IsExecution() && ref.Purpose != session.Recovery {
			continue
		}
		if err := f.Confirm(filepath.Join(ref.ID, name)); err != nil {
			return workerExecutionError("confirm exact reservation", err)
		}
	}
	if ref.BootID != BootID() {
		return fmt.Errorf("launch worker: reservation belongs to another boot")
	}
	return nil
}

// RenewRecovery retains the same attempt token and only explicit recovery's boot policy.
func RenewRecovery(root string, ref session.Reservation) (session.Reservation, error) {
	if err := ref.Validate(); err != nil {
		return ref, workerExecutionError("reservation.go RenewRecovery", err)
	}
	if ref.Purpose != session.Recovery || ref.BootID == "" || BootID() == "" || ref.BootID == BootID() {
		return ref, fmt.Errorf("recovery: cannot renew reservation")
	}
	f, err := execution.OpenFiles(filepath.Dir(root))
	if err != nil {
		return ref, workerExecutionError("reservation.go RenewRecovery", err)
	}
	defer func() { _ = f.Close() }()
	name := filepath.Join(filepath.Base(root), ref.ID, ".reservation")
	var current session.Reservation
	if err := f.Read(name, &current); err != nil {
		return ref, workerExecutionError("reservation.go RenewRecovery", err)
	}
	next := ref
	next.BootID = BootID()
	if current != ref && current != next {
		return ref, fmt.Errorf("recovery: reservation changed")
	}
	if _, err := f.Publish(name, next, false); err != nil {
		return ref, workerExecutionError("reservation.go RenewRecovery", err)
	}
	_, err = f.Publish(filepath.Join(filepath.Base(root), ref.ID, ".launching"), next, false)
	return next, workerExecutionError("reservation.go RenewRecovery", err)
}

// ReservedPurpose reads the durable allocation, which exists even if the worker
// has yet to write metadata or its metadata is damaged.
func ReservedPurpose(dir string) session.Purpose {
	ref, _ := ReservedAllocation(dir)
	return ref.Purpose
}

// PersonSession reports a session a person started: a generic or recovery
// allocation, or an older session with no allocation and no worker evidence
// of one. Keyed jobs, managed service and app workers, and sessions whose
// evidence cannot be read are not, so routes that serve only people's
// sessions fail closed.
func PersonSession(dir string) bool {
	if ref, ok := ReservedAllocation(dir); ok {
		return ref.Purpose == session.Generic || ref.Purpose == session.Recovery
	}
	if _, err := os.Lstat(filepath.Join(dir, ".reservation")); !errors.Is(err, os.ErrNotExist) {
		return false
	}
	meta, err := ReadMeta(dir)
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	return err == nil && meta.Reservation == nil && meta.ExecutionKind == "" && meta.Label == ""
}

// ReservedAllocation reads the durable allocation of dir, including the
// instance that names its execution attempt.
func ReservedAllocation(dir string) (session.Reservation, bool) {
	f, err := execution.OpenFiles(dir)
	if err != nil {
		return session.Reservation{}, false
	}
	defer func() { _ = f.Close() }()
	var ref session.Reservation
	if err := f.Read(".reservation", &ref); err != nil || ref.ID != filepath.Base(dir) || ref.Validate() != nil {
		return session.Reservation{}, false
	}
	return ref, true
}
