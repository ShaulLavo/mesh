package update

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/shaul/mesh/internal/release"
)

// Pre-naming approved operations retain their original serialized membership digest.
// Archived labels stay inside this store boundary and never enter live Host values.
type archivedHost struct {
	ID        string           `json:"id"`
	Label     *string          `json:"alias"`
	Endpoint  string           `json:"endpoint"`
	Platform  release.Platform `json:"platform"`
	DependsOn []string         `json:"dependsOn,omitempty"`
}

type archivedFleet struct {
	Version  int            `json:"version"`
	Name     string         `json:"name"`
	Revision uint64         `json:"revision"`
	Members  []archivedHost `json:"members"`
}

type archivedTarget struct {
	Target
	Host archivedHost `json:"host"`
}

type archivedRun struct {
	Run
	Fleet   archivedFleet    `json:"fleet"`
	Targets []archivedTarget `json:"targets"`
}

type operationArchive struct {
	fleet   archivedFleet
	targets map[string]archivedHost
}

type operationRecord struct{ Run }

func strictOperation(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("update: decode operation record: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("update: trailing operation data")
	}
	return nil
}

func (record *operationRecord) UnmarshalJSON(data []byte) error {
	var current Run
	if err := strictOperation(data, &current); err == nil {
		record.Run = current
		return nil
	}
	var archived archivedRun
	if err := strictOperation(data, &archived); err != nil {
		return err
	}
	run := archived.Run
	run.Fleet = Fleet{Version: archived.Fleet.Version, Name: archived.Fleet.Name, Revision: archived.Fleet.Revision}
	archive := &operationArchive{fleet: archived.Fleet, targets: make(map[string]archivedHost, len(archived.Targets))}
	labels := make(map[string]string, len(archived.Fleet.Members))
	for _, host := range archived.Fleet.Members {
		if host.Label == nil {
			return errors.New("update: archived membership requires its original label field")
		}
		labels[host.ID] = *host.Label
		run.Fleet.Members = append(run.Fleet.Members, host.live())
	}
	for _, target := range archived.Targets {
		if target.Host.Label == nil {
			return errors.New("update: archived target requires its original label field")
		}
		label, ok := labels[target.Host.ID]
		if !ok || *target.Host.Label != label {
			return errors.New("update: archived target label differs from its approved membership")
		}
		live := target.Target
		live.Host = target.Host.live()
		run.Targets = append(run.Targets, live)
		archive.targets[target.Host.ID] = target.Host
	}
	run.archive = archive
	record.Run = run
	return nil
}

func (host archivedHost) live() Host {
	return Host{ID: host.ID, Endpoint: host.Endpoint, Platform: host.Platform, DependsOn: host.DependsOn}
}

func (r Run) approvedMembership() string {
	if r.archive == nil {
		return r.Fleet.Digest()
	}
	data, _ := json.Marshal(r.archive.fleet)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

type operationApproval struct {
	id, coordinator, membership, releaseDigest, fleet, release, createdAt string
}

func (r Run) approvalIdentity() operationApproval {
	return operationApproval{r.ID, r.Coordinator, r.Membership, r.ReleaseDigest,
		r.Fleet.Digest(), r.Release.Digest(), r.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")}
}

func writeOperation(path string, run Run) error {
	if run.archive == nil {
		return writeJSON(path, run)
	}
	archived := archivedRun{Run: run, Fleet: run.archive.fleet}
	for _, target := range run.Targets {
		archived.Targets = append(archived.Targets, archivedTarget{Target: target, Host: run.archive.targets[target.Host.ID]})
	}
	return writeJSON(path, archived)
}
