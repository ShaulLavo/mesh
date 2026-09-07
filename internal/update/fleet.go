package update

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/shaul/mesh/internal/release"
)

const MaximumHosts = 256

type Host struct {
	ID        string           `json:"id"`
	Alias     string           `json:"alias"`
	Endpoint  string           `json:"endpoint"`
	Platform  release.Platform `json:"platform"`
	DependsOn []string         `json:"dependsOn,omitempty"`
}

type Fleet struct {
	Version  int    `json:"version"`
	Name     string `json:"name"`
	Revision uint64 `json:"revision"`
	Members  []Host `json:"members"`
}

func PublicKey(id string) (ed25519.PublicKey, error) {
	key, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil || len(key) != ed25519.PublicKeySize || base64.RawURLEncoding.EncodeToString(key) != id {
		return nil, errors.New("update: invalid host identity")
	}
	return ed25519.PublicKey(key), nil
}

func (h Host) Validate() error {
	if _, err := PublicKey(h.ID); err != nil {
		return err
	}
	if h.Alias == "" || len(h.Alias) > 128 || strings.ContainsAny(h.Alias, "\x00\r\n\t") {
		return errors.New("update: invalid host alias")
	}
	u, err := url.Parse(h.Endpoint)
	if err != nil || u.User != nil || u.Fragment != "" {
		return errors.New("update: invalid host endpoint")
	}
	switch u.Scheme {
	case "ws", "wss":
		if u.Host == "" {
			return errors.New("update: empty network endpoint")
		}
	case "unix":
		if u.Host != "" || !strings.HasPrefix(u.Path, "/") || u.RawQuery != "" {
			return errors.New("update: invalid local endpoint")
		}
	default:
		return errors.New("update: host endpoint must use ws, wss, or unix")
	}
	return nil
}

func (f Fleet) Validate() error {
	if f.Version != 1 || f.Revision == 0 || f.Name == "" || len(f.Name) > 128 {
		return errors.New("update: invalid fleet version, revision, or name")
	}
	if len(f.Members) == 0 || len(f.Members) > MaximumHosts {
		return errors.New("update: fleet must contain between 1 and 256 hosts")
	}
	ids := make(map[string]bool, len(f.Members))
	aliases := make(map[string]bool, len(f.Members))
	for _, host := range f.Members {
		if err := host.Validate(); err != nil {
			return fmt.Errorf("update: fleet member %q: %w", host.Alias, err)
		}
		if ids[host.ID] || aliases[host.Alias] {
			return errors.New("update: duplicate fleet identity or alias")
		}
		ids[host.ID], aliases[host.Alias] = true, true
	}
	_, err := f.Order("")
	return err
}

func (f Fleet) Digest() string {
	data, _ := json.Marshal(f)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

// Order puts a dependent before its router, and the coordinator last.
func (f Fleet) Order(coordinator string) ([]Host, error) {
	hosts := make(map[string]Host, len(f.Members))
	edges := make(map[string]map[string]bool, len(f.Members))
	incoming := make(map[string]int, len(f.Members))
	for _, h := range f.Members {
		hosts[h.ID] = h
		incoming[h.ID] = 0
		edges[h.ID] = make(map[string]bool)
	}
	for _, h := range f.Members {
		if err := addDependencies(h, hosts, edges, incoming); err != nil {
			return nil, err
		}
		if h.ID != coordinator && hosts[coordinator].ID != "" && !edges[h.ID][coordinator] {
			edges[h.ID][coordinator] = true
			incoming[coordinator]++
		}
	}
	var ordered []Host
	for len(incoming) != 0 {
		ready := readyHosts(incoming)
		if len(ready) == 0 {
			return nil, errors.New("update: routing dependencies conflict or contain a cycle")
		}
		id := ready[0]
		ordered = append(ordered, hosts[id])
		delete(incoming, id)
		for next := range edges[id] {
			incoming[next]--
		}
	}
	return ordered, nil
}

func addDependencies(h Host, hosts map[string]Host, edges map[string]map[string]bool, incoming map[string]int) error {
	for _, dependency := range h.DependsOn {
		if dependency == h.ID || hosts[dependency].ID == "" {
			return fmt.Errorf("update: host %s has an unresolved route dependency", h.Alias)
		}
		if !edges[h.ID][dependency] {
			edges[h.ID][dependency] = true
			incoming[dependency]++
		}
	}
	return nil
}

func readyHosts(incoming map[string]int) []string {
	var ready []string
	for id, count := range incoming {
		if count == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)
	return ready
}

func ReadFleet(path string) (Fleet, error) {
	var fleet Fleet
	if err := readJSON(path, &fleet); err != nil {
		return Fleet{}, err
	}
	return fleet, fleet.Validate()
}

func SaveFleet(path string, fleet Fleet) error {
	if err := fleet.Validate(); err != nil {
		return err
	}
	return writeJSON(path, fleet)
}

func IsLocal(h Host) bool { return strings.HasPrefix(h.Endpoint, "unix:") }

func CacheDir() (string, error) {
	if configured := os.Getenv("MESH_UPDATE_CACHE_DIR"); configured != "" {
		return configured, nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return base + "/mesh/updates", nil
}
