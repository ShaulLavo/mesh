package recovery

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/shaul/mesh/internal/agentresume"
)

// HibernationFile names the marker a worker writes before it ends an idle
// agent on purpose. It separates "stopped to free memory, resume on attach"
// from an ordinary exit, which the exit metadata alone cannot express.
const HibernationFile = "hibernation.json"

// Hibernation reasons.
const (
	HibernateIdle    = "idle"
	HibernateRequest = "request"
)

// Hibernation is the durable record of an agent session stopped while its
// conversation stayed resumable.
type Hibernation struct {
	Version        int                  `json:"version"`
	At             time.Time            `json:"at"`
	Reason         string               `json:"reason"`
	Provider       agentresume.Provider `json:"provider"`
	ConversationID string               `json:"conversationId"`
}

func (h Hibernation) validate() error {
	if h.Version != 1 || h.At.IsZero() {
		return errors.New("recovery: invalid hibernation record")
	}
	if h.Reason != HibernateIdle && h.Reason != HibernateRequest {
		return fmt.Errorf("recovery: invalid hibernation reason %q", h.Reason)
	}
	if err := agentresume.ValidateProvider(h.Provider); err != nil {
		return err
	}
	if h.ConversationID == "" || !validField(h.ConversationID) {
		return errors.New("recovery: invalid hibernated conversation")
	}
	return nil
}

// WriteHibernation durably records the marker before the worker stops its
// process, so a crash mid-stop still reads as hibernated rather than exited.
func WriteHibernation(dir string, hibernation Hibernation) error {
	if err := hibernation.validate(); err != nil {
		return err
	}
	return writeTransaction(dir, HibernationFile, hibernation)
}

// ReadHibernation returns os.ErrNotExist for a session that never hibernated.
func ReadHibernation(dir string) (Hibernation, error) {
	var hibernation Hibernation
	if err := readTransaction(filepath.Join(dir, HibernationFile), &hibernation); err != nil {
		return Hibernation{}, err
	}
	if err := hibernation.validate(); err != nil {
		return Hibernation{}, err
	}
	return hibernation, nil
}

// RemoveHibernation clears a marker whose stop never happened.
func RemoveHibernation(dir string) error {
	if err := os.Remove(filepath.Join(dir, HibernationFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("recovery: remove hibernation marker: %w", err)
	}
	return nil
}
