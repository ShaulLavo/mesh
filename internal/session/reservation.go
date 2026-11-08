package session

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// Purpose keeps explicit recovery's replay policy separate from keyed commands.
type Purpose string

const (
	Generic   Purpose = "generic"
	Managed   Purpose = "managed"
	Recovery  Purpose = "recovery"
	Execution Purpose = "execution"
)

// Reservation identifies one allocation. It is never inferred from a directory.
type Reservation struct {
	ID       string  `json:"id"`
	Instance string  `json:"instance"`
	BootID   string  `json:"bootId"`
	Purpose  Purpose `json:"purpose"`
}

func NewReservation(id, boot string, purpose Purpose) (Reservation, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return Reservation{}, reservationBoundaryError("reservation.go NewReservation", err)
	}
	r := Reservation{ID: id, Instance: hex.EncodeToString(token[:]), BootID: boot, Purpose: purpose}
	return r, r.Validate()
}

func (r Reservation) Validate() error {
	id, err := ParseID(r.ID)
	if err != nil || id != r.ID {
		return fmt.Errorf("invalid reservation session ID")
	}
	raw, err := hex.DecodeString(r.Instance)
	if err != nil || len(raw) != 16 || hex.EncodeToString(raw) != r.Instance {
		return fmt.Errorf("invalid reservation instance")
	}
	switch r.Purpose {
	case Generic, Managed, Recovery, Execution:
	default:
		return fmt.Errorf("invalid reservation purpose")
	}
	if (r.Purpose == Managed || r.Purpose == Recovery || r.Purpose == Execution) && r.BootID == "" {
		return fmt.Errorf("reservation requires a known boot")
	}
	return nil
}

func reservationBoundaryError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}
