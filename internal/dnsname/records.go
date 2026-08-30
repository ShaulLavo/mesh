// Package dnsname manages Mesh DNS records and private certificates.
package dnsname

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

const (
	// Zone is the authoritative DNS zone used by Mesh.
	Zone = "shaulavo.dev"
	// PrivateZone contains direct tailnet-only origin names.
	PrivateZone = "mesh." + Zone
	// WildcardName is the certificate name installed on private origins.
	WildcardName = "*." + PrivateZone

	ManagedARecordComment   = "mesh:private-origin"
	ManagedTXTRecordComment = "mesh:acme-dns01"
	DefaultRecordTTL        = 60
)

var tailnetIPv4Prefix = netip.MustParsePrefix("100.64.0.0/10")

// RecordType identifies the DNS record kinds Mesh manages.
type RecordType string

const (
	RecordA   RecordType = "A"
	RecordTXT RecordType = "TXT"
)

// Record is the provider-independent form of one DNS record.
type Record struct {
	ID      string
	Type    RecordType
	Name    string
	Content string
	TTL     int
	Proxied bool
	Comment string
}

// RecordInput is a complete record definition for create or update.
type RecordInput struct {
	Type    RecordType
	Name    string
	Content string
	TTL     int
	Proxied bool
	Comment string
}

// Provider is the DNS mechanism shared by private names and the public edge.
type Provider interface {
	ListRecords(context.Context, string, RecordType) ([]Record, error)
	CreateRecord(context.Context, RecordInput) (Record, error)
	UpdateRecord(context.Context, string, RecordInput) (Record, error)
	DeleteRecord(context.Context, string) error
}

// HostAddress binds one private DNS label to its current tailnet IPv4 address.
type HostAddress struct {
	Name    string
	Address netip.Addr
}

// ChallengeRecord identifies the exact TXT record created for one DNS-01
// challenge. Cleanup never searches by value or deletes another owner.
type ChallengeRecord struct {
	ID    string
	Name  string
	Value string
}

// ReconcileHostA converges one Mesh-owned A record without modifying records
// whose comment is not the exact Mesh ownership marker.
func ReconcileHostA(ctx context.Context, provider Provider, host HostAddress) (Record, error) {
	if ctx == nil {
		return Record{}, errors.New("dnsname: reconcile host with nil context")
	}
	if provider == nil {
		return Record{}, errors.New("dnsname: reconcile host with nil provider")
	}
	name, err := privateHostName(host.Name)
	if err != nil {
		return Record{}, err
	}
	if !host.Address.Is4() || !tailnetIPv4Prefix.Contains(host.Address) {
		return Record{}, fmt.Errorf("dnsname: host %s address %s is not a tailnet IPv4 address", host.Name, host.Address)
	}
	records, err := provider.ListRecords(ctx, name, RecordA)
	if err != nil {
		return Record{}, fmt.Errorf("dnsname: list A record %s: %w", name, err)
	}
	managed := make([]Record, 0, len(records))
	for _, record := range records {
		if record.Comment != ManagedARecordComment {
			return Record{}, fmt.Errorf("dnsname: A record %s is unmanaged; refusing to change it", name)
		}
		managed = append(managed, record)
	}
	desired := RecordInput{
		Type: RecordA, Name: name, Content: host.Address.String(), TTL: DefaultRecordTTL,
		Proxied: false, Comment: ManagedARecordComment,
	}
	if len(managed) == 0 {
		record, err := provider.CreateRecord(ctx, desired)
		if err != nil {
			return Record{}, fmt.Errorf("dnsname: create A record %s: %w", name, err)
		}
		return record, nil
	}

	slices.SortFunc(managed, func(a, b Record) int { return strings.Compare(a.ID, b.ID) })
	kept := managed[0]
	if !recordMatches(kept, desired) {
		kept, err = provider.UpdateRecord(ctx, kept.ID, desired)
		if err != nil {
			return Record{}, fmt.Errorf("dnsname: update A record %s: %w", name, err)
		}
	}
	for _, duplicate := range managed[1:] {
		if err := provider.DeleteRecord(ctx, duplicate.ID); err != nil {
			return Record{}, fmt.Errorf("dnsname: delete duplicate A record %s (%s): %w", name, duplicate.ID, err)
		}
	}
	return kept, nil
}

// PresentTXT creates or adopts the Mesh-owned record for one DNS-01 value.
// Other TXT values at the same name remain untouched.
func PresentTXT(ctx context.Context, provider Provider, name, value string) (ChallengeRecord, error) {
	if ctx == nil {
		return ChallengeRecord{}, errors.New("dnsname: present TXT with nil context")
	}
	if provider == nil {
		return ChallengeRecord{}, errors.New("dnsname: present TXT with nil provider")
	}
	name, err := canonicalDNSName(name)
	if err != nil {
		return ChallengeRecord{}, err
	}
	if value == "" || strings.ContainsAny(value, "\x00\r\n") {
		return ChallengeRecord{}, errors.New("dnsname: DNS-01 value is empty or contains control characters")
	}
	records, err := provider.ListRecords(ctx, name, RecordTXT)
	if err != nil {
		return ChallengeRecord{}, fmt.Errorf("dnsname: list TXT record %s: %w", name, err)
	}
	for _, record := range records {
		if record.Comment == ManagedTXTRecordComment && record.Content == value {
			return ChallengeRecord{ID: record.ID, Name: name, Value: value}, nil
		}
	}
	record, err := provider.CreateRecord(ctx, RecordInput{
		Type: RecordTXT, Name: name, Content: value, TTL: DefaultRecordTTL,
		Proxied: false, Comment: ManagedTXTRecordComment,
	})
	if err != nil {
		return ChallengeRecord{}, fmt.Errorf("dnsname: create TXT record %s: %w", name, err)
	}
	return ChallengeRecord{ID: record.ID, Name: name, Value: value}, nil
}

// CleanupTXT deletes only the Mesh-owned record identified by challenge.
// Missing records make retry cleanup succeed.
func CleanupTXT(ctx context.Context, provider Provider, challenge ChallengeRecord) error {
	if ctx == nil {
		return errors.New("dnsname: clean TXT with nil context")
	}
	if provider == nil {
		return errors.New("dnsname: clean TXT with nil provider")
	}
	if challenge.ID == "" {
		return nil
	}
	records, err := provider.ListRecords(ctx, challenge.Name, RecordTXT)
	if err != nil {
		return fmt.Errorf("dnsname: list TXT record %s for cleanup: %w", challenge.Name, err)
	}
	for _, record := range records {
		if record.ID != challenge.ID {
			continue
		}
		if record.Comment != ManagedTXTRecordComment {
			return fmt.Errorf("dnsname: TXT record %s (%s) lost its Mesh ownership comment", challenge.Name, challenge.ID)
		}
		if err := provider.DeleteRecord(ctx, challenge.ID); err != nil {
			return fmt.Errorf("dnsname: delete TXT record %s (%s): %w", challenge.Name, challenge.ID, err)
		}
		return nil
	}
	return nil
}

func privateHostName(label string) (string, error) {
	if err := validateDNSLabel(label); err != nil {
		return "", fmt.Errorf("dnsname: invalid private host name: %w", err)
	}
	return label + "." + PrivateZone, nil
}

func canonicalDNSName(name string) (string, error) {
	name = strings.TrimSuffix(name, ".")
	if name == "" || len(name) > 253 || name != strings.ToLower(name) {
		return "", fmt.Errorf("dnsname: invalid canonical DNS name %q", name)
	}
	for _, label := range strings.Split(name, ".") {
		if err := validateDNSOwnerLabel(label); err != nil {
			return "", fmt.Errorf("dnsname: invalid DNS name %q: %w", name, err)
		}
	}
	return name, nil
}

func validateDNSOwnerLabel(label string) error {
	if label == "" || len(label) > 63 || label != strings.ToLower(label) || label[0] == '-' || label[len(label)-1] == '-' {
		return fmt.Errorf("label %q must be 1 to 63 lowercase DNS characters with no edge hyphen", label)
	}
	for _, character := range label {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '-' || character == '_' {
			continue
		}
		return fmt.Errorf("label %q contains unsupported characters", label)
	}
	return nil
}

func validateDNSLabel(label string) error {
	if label == "" || len(label) > 63 || label != strings.ToLower(label) || label[0] == '-' || label[len(label)-1] == '-' {
		return fmt.Errorf("label %q must be 1 to 63 lowercase DNS characters with no edge hyphen", label)
	}
	for _, character := range label {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '-' {
			continue
		}
		return fmt.Errorf("label %q contains unsupported characters", label)
	}
	return nil
}

func recordMatches(record Record, desired RecordInput) bool {
	return record.Type == desired.Type && record.Name == desired.Name && record.Content == desired.Content &&
		record.TTL == desired.TTL && record.Proxied == desired.Proxied && record.Comment == desired.Comment
}
