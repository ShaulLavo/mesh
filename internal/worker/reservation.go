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
	if err := os.MkdirAll(root, 0700); err != nil {
		return session.Reservation{}, workerExecutionError("reservation.go Reserve", err)
	}
	for range sessionIDAttempts {
		id, err := session.NewID()
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
func publishReservation(root string, ref session.Reservation) error {
	if ref.Purpose == session.Execution || ref.Purpose == session.Recovery {
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
		if ref.Purpose != session.Execution && ref.Purpose != session.Recovery {
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
