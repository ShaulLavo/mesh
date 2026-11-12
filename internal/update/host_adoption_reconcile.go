package update

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"

	"github.com/shaul/mesh/internal/updateinstall"
)

// Only the confirmation writer publishes adoption-origin evidence. The matching
// projection leaves its unrelated operation details opaque and unchanged.
func (s *Store) adoptionOwnsCommit(journal updateinstall.Status) (bool, error) {
	var envelope HostAdoption
	err := s.readAuthority("host-adoption.json", &envelope)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := envelope.validate(); err != nil {
		return false, err
	}
	if envelope.Confirmed || envelope.Operation != journal.Request.ID {
		return false, nil
	}
	var operation struct {
		Schema   int                    `json:"schema"`
		ID       string                 `json:"id"`
		Pin      string                 `json:"pin"`
		Request  updateinstall.Request  `json:"request"`
		Settings updateinstall.Settings `json:"settings"`
	}
	if err := json.Unmarshal(envelope.Details, &operation); err != nil {
		return false, hostError(err)
	}
	if operation.Schema != 1 || operation.ID != envelope.Operation || operation.Pin != envelope.Pin || operation.Request.ID != operation.ID {
		return false, errors.New("update: adoption confirmation binding differs from its canonical operation and pin")
	}
	if operation.Request.Generation != journal.Request.Generation {
		return false, nil
	}
	if !reflect.DeepEqual(operation.Request, journal.Request) || operation.Settings != journal.Settings {
		return false, errors.New("update: committed journal differs from pinned adoption request")
	}
	return true, nil
}
