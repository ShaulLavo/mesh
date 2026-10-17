// Package fleetshare shares local connection books between explicitly trusted
// controllers. Imported connections remain attributed to their source.
package fleetshare

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
)

const maximumEntries = 256

// Peer is a controller allowed to exchange its locally saved connections.
type Peer struct {
	Identity string `json:"identity"`
	Endpoint string `json:"endpoint"`
}

type enrollment struct {
	Peer
	Generation string               `json:"generation"`
	Hosts      []protocol.FleetHost `json:"hosts,omitempty"`
}

type book struct {
	Version int          `json:"version"`
	Peers   []enrollment `json:"peers"`
}

// Peers returns the current explicit controller policy.
func Peers(configDir string) ([]Peer, error) {
	state, err := readState(configDir)
	if err != nil {
		return nil, err
	}
	var peers []Peer
	for _, entry := range state.Peers {
		peers = append(peers, entry.Peer)
	}
	return peers, nil
}

// Enroll records local consent. A new generation invalidates in-flight pulls.
func Enroll(configDir string, peer Peer) error {
	if err := validatePeer(peer); err != nil {
		return err
	}
	var generation [16]byte
	if _, err := rand.Read(generation[:]); err != nil {
		return fmt.Errorf("fleet: generate enrollment ID: %w", err)
	}
	entry := enrollment{Peer: peer, Generation: hex.EncodeToString(generation[:])}
	return updateState(context.Background(), configDir, func(state *book) error {
		return enroll(state, entry)
	})
}

func enroll(state *book, entry enrollment) error {
	for i := range state.Peers {
		if state.Peers[i].Peer == entry.Peer {
			return nil
		}
		if state.Peers[i].Identity == entry.Identity {
			state.Peers[i] = entry
			return nil
		}
	}
	if len(state.Peers) >= maximumEntries {
		return fmt.Errorf("fleet: controller count exceeds %d", maximumEntries)
	}
	state.Peers = append(state.Peers, entry)
	return nil
}

// Remove revokes a controller and discards its imported connections.
func Remove(configDir, id string) error {
	if _, err := identity.IdentityKey(id); err != nil {
		return fmt.Errorf("fleet: invalid controller identity: %w", err)
	}
	return updateState(context.Background(), configDir, func(state *book) error {
		remove(state, id)
		return nil
	})
}

func remove(state *book, id string) {
	for i, entry := range state.Peers {
		if entry.Identity == id {
			state.Peers = append(state.Peers[:i], state.Peers[i+1:]...)
			return
		}
	}
}

// Hosts reads retained snapshots only from currently enrolled controllers.
func Hosts(configDir string) ([]protocol.FleetHost, error) {
	state, err := readState(configDir)
	if err != nil {
		return nil, err
	}
	return mergedHosts(&state), nil
}

func mergedHosts(state *book) []protocol.FleetHost {
	seen := make(map[string]bool)
	var hosts []protocol.FleetHost
	for _, entry := range state.Peers {
		hosts = appendUnique(hosts, seen, entry.Hosts)
	}
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].ID < hosts[j].ID })
	return hosts
}

func appendUnique(hosts []protocol.FleetHost, seen map[string]bool, incoming []protocol.FleetHost) []protocol.FleetHost {
	for _, host := range incoming {
		if seen[host.ID] {
			continue
		}
		seen[host.ID] = true
		host.Addresses = append([]string(nil), host.Addresses...)
		hosts = append(hosts, host)
	}
	return hosts
}

func validateBook(state *book) error {
	if state.Version != 1 {
		return fmt.Errorf("fleet: unsupported configuration version %d", state.Version)
	}
	if len(state.Peers) > maximumEntries {
		return fmt.Errorf("fleet: controller count exceeds %d", maximumEntries)
	}
	seen := make(map[string]bool, len(state.Peers))
	for _, entry := range state.Peers {
		if err := validateEnrollment(entry, seen); err != nil {
			return err
		}
	}
	sort.Slice(state.Peers, func(i, j int) bool { return state.Peers[i].Identity < state.Peers[j].Identity })
	if len(mergedHosts(state)) > maximumEntries {
		return fmt.Errorf("fleet: imported host count exceeds %d", maximumEntries)
	}
	return nil
}

func validateEnrollment(entry enrollment, seen map[string]bool) error {
	if err := validatePeer(entry.Peer); err != nil {
		return err
	}
	if seen[entry.Identity] {
		return fmt.Errorf("fleet: duplicate controller %s", entry.Identity)
	}
	seen[entry.Identity] = true
	generation, err := hex.DecodeString(entry.Generation)
	if err != nil || len(generation) != 16 {
		return errors.New("fleet: invalid enrollment generation")
	}
	return validateHosts(entry.Hosts)
}

func replaceSnapshot(ctx context.Context, configDir string, source enrollment, hosts []protocol.FleetHost) error {
	if err := validateHosts(hosts); err != nil {
		return err
	}
	return updateState(ctx, configDir, func(state *book) error {
		replaceCurrentSnapshot(state, source, hosts)
		return nil
	})
}

func replaceCurrentSnapshot(state *book, source enrollment, hosts []protocol.FleetHost) {
	for i, current := range state.Peers {
		if current.Identity == source.Identity && current.Generation == source.Generation {
			state.Peers[i].Hosts = hosts
			return
		}
	}
}
