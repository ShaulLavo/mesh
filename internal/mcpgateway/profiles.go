package mcpgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Section 7.1 and 7.2 bounds.
const (
	ProfilesFileName   = "mcp-profiles.json"
	SchemaVersion      = 1
	MaxProfileServices = 32
	MaxExposedTools    = 2000
	MaxOverflowNames   = 65536
	maxPatterns        = 64
	maxPatternLength   = 128
)

// NativeGroups are the native tool families a profile can select.
var NativeGroups = []string{"hosts", "contexts", "exec", "sessions", "services", "terminal", "files", "fleet"}

var (
	// ErrConflict means the file changed since the expected revision.
	ErrConflict = errors.New("profiles file revision conflict")
	// ErrNotFound means the named profile or entry does not exist.
	ErrNotFound = errors.New("profile entry not found")
	// ErrInvalid marks a malformed profile, entry or argument.
	ErrInvalid = errors.New("invalid profile")
	// ErrLimit marks a profile or registry beyond its section 7 bound.
	ErrLimit = errors.New("profile limit reached")

	namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
)

// File is the versioned private mcp-profiles.json.
type File struct {
	SchemaVersion int                `json:"schemaVersion"`
	Revision      uint64             `json:"revision"`
	Profiles      map[string]Profile `json:"profiles"`
	Overflow      Overflow           `json:"overflow"`
}

// Profile selects native tool groups and hosted services, in order.
type Profile struct {
	// Native lists the native groups. Null or absent means every group;
	// empty means only mesh_skill.
	Native   []string    `json:"native"`
	Services []Selection `json:"services"`
}

// Selection binds one hosted service by stable host and service IDs.
type Selection struct {
	Alias     string `json:"alias"`
	HostID    string `json:"hostId"`
	ServiceID string `json:"serviceId"`
	// ServiceName is the name the service had when it was selected. Repair
	// uses it to find a re-published service; routing never does.
	ServiceName string   `json:"serviceName,omitempty"`
	Instance    string   `json:"instance,omitempty"`
	Include     []string `json:"include"`
	Exclude     []string `json:"exclude"`
}

// Overflow is the hx_ registry for names longer than MaxNameLength.
type Overflow struct {
	Next  uint64              `json:"next"`
	Names map[string]Identity `json:"names"`
}

// NewFile returns an empty current file.
func NewFile() File {
	return File{SchemaVersion: SchemaVersion, Profiles: map[string]Profile{}, Overflow: Overflow{Next: 1, Names: map[string]Identity{}}}
}

// Validate checks the whole file, including every name and pattern.
func (f File) Validate() error {
	if f.SchemaVersion != SchemaVersion {
		return fmt.Errorf("%w: schemaVersion %d is not %d", ErrInvalid, f.SchemaVersion, SchemaVersion)
	}
	for _, name := range slices.Sorted(maps.Keys(f.Profiles)) {
		if err := ValidateProfileName(name); err != nil {
			return err
		}
		if err := f.Profiles[name].validate(); err != nil {
			return fmt.Errorf("profile %s: %w", name, err)
		}
	}
	if len(f.Overflow.Names) > MaxOverflowNames {
		return fmt.Errorf("%w: %d overflow names exceed %d", ErrLimit, len(f.Overflow.Names), MaxOverflowNames)
	}
	for name, id := range f.Overflow.Names {
		if err := validateOverflow(name, id, f.Overflow.Next); err != nil {
			return err
		}
	}
	return nil
}

func validateOverflow(name string, id Identity, next uint64) error {
	n, err := overflowNumber(name)
	if err != nil || n >= next {
		return fmt.Errorf("%w: overflow name %q", ErrInvalid, name)
	}
	if _, _, err := EncodedName(id); err != nil {
		return fmt.Errorf("%w: overflow name %s: %w", ErrInvalid, name, err)
	}
	return nil
}

func overflowNumber(name string) (uint64, error) {
	digits, ok := strings.CutPrefix(name, overflowPrefix)
	if !ok || digits == "" || digits[0] == '0' {
		return 0, ErrUnknownName
	}
	n, err := strconv.ParseUint(digits, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("overflow name %s: %w", name, err)
	}
	return n, nil
}

// ValidateProfileName checks a profile name or alias.
func ValidateProfileName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("%w: %q must be 1 to 32 lowercase letters, digits or hyphens", ErrInvalid, name)
	}
	return nil
}

func (p Profile) validate() error {
	for _, group := range p.Native {
		if !slices.Contains(NativeGroups, group) {
			return fmt.Errorf("%w: unknown native group %q (want %s)", ErrInvalid, group, strings.Join(NativeGroups, ", "))
		}
	}
	if len(p.Services) > MaxProfileServices {
		return fmt.Errorf("%w: %d services exceed %d per profile", ErrLimit, len(p.Services), MaxProfileServices)
	}
	aliases := map[string]bool{}
	routes := map[[2]string]bool{}
	for _, s := range p.Services {
		if err := s.validate(); err != nil {
			return err
		}
		if aliases[s.Alias] {
			return fmt.Errorf("%w: alias %s is used twice", ErrInvalid, s.Alias)
		}
		aliases[s.Alias] = true
		key := [2]string{s.HostID, s.ServiceID}
		if routes[key] {
			return fmt.Errorf("%w: service %s on host %s is selected twice", ErrInvalid, s.ServiceID, s.HostID)
		}
		routes[key] = true
	}
	return nil
}

func (s Selection) validate() error {
	if err := ValidateProfileName(s.Alias); err != nil {
		return err
	}
	if _, err := hostBytes(s.HostID); err != nil {
		return fmt.Errorf("%w: alias %s: %w", ErrInvalid, s.Alias, err)
	}
	if _, err := serviceBytes(s.ServiceID); err != nil {
		return fmt.Errorf("%w: alias %s: %w", ErrInvalid, s.Alias, err)
	}
	if len(s.Include)+len(s.Exclude) > maxPatterns {
		return fmt.Errorf("%w: alias %s has more than %d patterns", ErrLimit, s.Alias, maxPatterns)
	}
	for _, pattern := range slices.Concat(s.Include, s.Exclude) {
		if _, err := path.Match(pattern, ""); err != nil || pattern == "" || len(pattern) > maxPatternLength {
			return fmt.Errorf("%w: alias %s: bad pattern %q", ErrInvalid, s.Alias, pattern)
		}
	}
	return nil
}

// Allows reports whether an upstream tool or prompt name passes the
// selection's include and exclude patterns. No include list means all.
func (s Selection) Allows(name string) bool {
	included := len(s.Include) == 0
	for _, pattern := range s.Include {
		if ok, _ := path.Match(pattern, name); ok {
			included = true
			break
		}
	}
	if !included {
		return false
	}
	for _, pattern := range s.Exclude {
		if ok, _ := path.Match(pattern, name); ok {
			return false
		}
	}
	return true
}

// Lookup returns the selection for one hosted route.
func (p Profile) Lookup(hostID, serviceID string) (Selection, bool) {
	for _, s := range p.Services {
		if s.HostID == hostID && s.ServiceID == serviceID {
			return s, true
		}
	}
	return Selection{}, false
}

// NativeEnabled reports whether a native group is selected.
func (p Profile) NativeEnabled(group string) bool {
	return p.Native == nil || slices.Contains(p.Native, group)
}

// OverflowName returns the hx_ allocation of id, if any.
func (f File) OverflowName(id Identity) (string, bool) {
	for name, existing := range f.Overflow.Names {
		if existing == id {
			return name, true
		}
	}
	return "", false
}

// allocate gives id the next hx_ name. Allocations are never reused.
func (f *File) allocate(id Identity) (string, error) {
	if name, ok := f.OverflowName(id); ok {
		return name, nil
	}
	if len(f.Overflow.Names) >= MaxOverflowNames {
		return "", fmt.Errorf("%w: the hx_ registry holds %d names", ErrLimit, MaxOverflowNames)
	}
	if f.Overflow.Next == 0 {
		f.Overflow.Next = 1
	}
	name := overflowPrefix + strconv.FormatUint(f.Overflow.Next, 10)
	f.Overflow.Next++
	if f.Overflow.Names == nil {
		f.Overflow.Names = map[string]Identity{}
	}
	f.Overflow.Names[name] = id
	return name, nil
}

// ExposedName returns the exposed name for id, allocating an hx_ name under
// the store lock when the deterministic form is too long.
func (s *Store) ExposedName(ctx context.Context, id Identity) (string, error) {
	name, fits, err := EncodedName(id)
	if err != nil || fits {
		return name, err
	}
	current, err := s.Load()
	if err != nil {
		return "", err
	}
	if existing, ok := current.OverflowName(id); ok {
		return existing, nil
	}
	var allocated string
	_, err = s.Update(ctx, nil, func(f *File) error {
		allocated, err = f.allocate(id)
		return err
	})
	return allocated, err
}

// ResolveName maps an exposed hosted name back to its identity.
func ResolveName(f File, name string) (Identity, error) {
	if !IsOverflowName(name) {
		return DecodeName(name)
	}
	id, ok := f.Overflow.Names[name]
	if !ok {
		return Identity{}, fmt.Errorf("%w: %q has no allocation", ErrUnknownName, name)
	}
	return id, nil
}

func decodeFile(raw []byte) (File, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	f := NewFile()
	if err := decoder.Decode(&f); err != nil {
		return File{}, fmt.Errorf("%w: decode %s: %w", ErrInvalid, ProfilesFileName, err)
	}
	if f.Profiles == nil {
		f.Profiles = map[string]Profile{}
	}
	if f.Overflow.Names == nil {
		f.Overflow.Names = map[string]Identity{}
	}
	if err := f.Validate(); err != nil {
		return File{}, err
	}
	return f, nil
}
