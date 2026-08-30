package dnsname

import (
	"context"
	"errors"
	"net/netip"
	"reflect"
	"strconv"
	"testing"
)

func TestReconcileHostAConvergesManagedRecord(t *testing.T) {
	provider := &memoryProvider{}
	host := HostAddress{Name: "pc", Address: netip.MustParseAddr("100.88.7.9")}

	created, err := ReconcileHostA(context.Background(), provider, host)
	if err != nil {
		t.Fatal(err)
	}
	if created.Name != "pc.mesh.shaulavo.dev" || created.Content != "100.88.7.9" || created.Proxied || created.Comment != ManagedARecordComment {
		t.Fatalf("created record = %#v", created)
	}
	if _, err := ReconcileHostA(context.Background(), provider, host); err != nil {
		t.Fatal(err)
	}
	if provider.creates != 1 || provider.updates != 0 || provider.deletes != 0 {
		t.Fatalf("idempotent operations = create %d, update %d, delete %d", provider.creates, provider.updates, provider.deletes)
	}

	host.Address = netip.MustParseAddr("100.99.8.10")
	updated, err := ReconcileHostA(context.Background(), provider, host)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != created.ID || updated.Content != "100.99.8.10" || provider.updates != 1 {
		t.Fatalf("updated record = %#v, updates = %d", updated, provider.updates)
	}
}

func TestReconcileHostARefusesUnmanagedRecordWithoutWrites(t *testing.T) {
	provider := &memoryProvider{records: []Record{{ID: "human", Type: RecordA, Name: "pc.mesh.shaulavo.dev", Content: "192.0.2.1"}}}
	_, err := ReconcileHostA(context.Background(), provider, HostAddress{Name: "pc", Address: netip.MustParseAddr("100.88.7.9")})
	if err == nil {
		t.Fatal("unmanaged record was overwritten")
	}
	if provider.creates != 0 || provider.updates != 0 || provider.deletes != 0 {
		t.Fatalf("unmanaged conflict caused writes: %#v", provider)
	}
}

func TestReconcileHostARejectsNonTailnetAddresses(t *testing.T) {
	for _, address := range []string{"192.0.2.1", "fd7a:115c:a1e0::1"} {
		t.Run(address, func(t *testing.T) {
			provider := &memoryProvider{}
			_, err := ReconcileHostA(context.Background(), provider, HostAddress{Name: "pc", Address: netip.MustParseAddr(address)})
			if err == nil || provider.creates != 0 {
				t.Fatalf("address %s error = %v, creates = %d", address, err, provider.creates)
			}
		})
	}
}

func TestTXTChallengeCleanupDeletesOnlyExactManagedRecord(t *testing.T) {
	provider := &memoryProvider{records: []Record{
		{ID: "human", Type: RecordTXT, Name: "_acme-challenge.mesh.shaulavo.dev", Content: "keep"},
		{ID: "other", Type: RecordTXT, Name: "_acme-challenge.mesh.shaulavo.dev", Content: "other", Comment: ManagedTXTRecordComment},
	}}
	challenge, err := PresentTXT(context.Background(), provider, "_acme-challenge.mesh.shaulavo.dev", "wanted")
	if err != nil {
		t.Fatal(err)
	}
	if err := CleanupTXT(context.Background(), provider, challenge); err != nil {
		t.Fatal(err)
	}
	if err := CleanupTXT(context.Background(), provider, challenge); err != nil {
		t.Fatalf("retry cleanup: %v", err)
	}
	want := []Record{
		{ID: "human", Type: RecordTXT, Name: "_acme-challenge.mesh.shaulavo.dev", Content: "keep"},
		{ID: "other", Type: RecordTXT, Name: "_acme-challenge.mesh.shaulavo.dev", Content: "other", Comment: ManagedTXTRecordComment},
	}
	if !reflect.DeepEqual(provider.records, want) {
		t.Fatalf("records after cleanup = %#v, want %#v", provider.records, want)
	}
}

func TestPresentTXTAdoptsOnlyExactManagedComment(t *testing.T) {
	name := "_acme-challenge.mesh.shaulavo.dev"
	provider := &memoryProvider{records: []Record{{
		ID: "unmanaged", Type: RecordTXT, Name: name, Content: "wanted", Comment: "human note",
	}}}
	challenge, err := PresentTXT(context.Background(), provider, name, "wanted")
	if err != nil {
		t.Fatal(err)
	}
	if challenge.ID == "unmanaged" || provider.creates != 1 {
		t.Fatalf("unmanaged TXT was adopted: challenge = %#v, creates = %d", challenge, provider.creates)
	}
	if _, err := PresentTXT(context.Background(), provider, name, "wanted"); err != nil {
		t.Fatal(err)
	}
	if provider.creates != 1 {
		t.Fatalf("exact managed TXT was not adopted; creates = %d", provider.creates)
	}
}

type memoryProvider struct {
	records                   []Record
	creates, updates, deletes int
	nextID                    int
	fail                      error
}

func (p *memoryProvider) ListRecords(_ context.Context, name string, recordType RecordType) ([]Record, error) {
	if p.fail != nil {
		return nil, p.fail
	}
	var records []Record
	for _, record := range p.records {
		if record.Name == name && record.Type == recordType {
			records = append(records, cloneRecord(record))
		}
	}
	return records, nil
}

func (p *memoryProvider) CreateRecord(_ context.Context, input RecordInput) (Record, error) {
	if p.fail != nil {
		return Record{}, p.fail
	}
	p.creates++
	p.nextID++
	record := recordFromInput("created-"+strconv.Itoa(p.nextID), input)
	p.records = append(p.records, record)
	return cloneRecord(record), nil
}

func (p *memoryProvider) UpdateRecord(_ context.Context, id string, input RecordInput) (Record, error) {
	p.updates++
	for index := range p.records {
		if p.records[index].ID == id {
			p.records[index] = recordFromInput(id, input)
			return cloneRecord(p.records[index]), nil
		}
	}
	return Record{}, errors.New("not found")
}

func (p *memoryProvider) DeleteRecord(_ context.Context, id string) error {
	p.deletes++
	for index := range p.records {
		if p.records[index].ID == id {
			p.records = append(p.records[:index], p.records[index+1:]...)
			return nil
		}
	}
	return errors.New("not found")
}

func recordFromInput(id string, input RecordInput) Record {
	return Record{ID: id, Type: input.Type, Name: input.Name, Content: input.Content, TTL: input.TTL, Proxied: input.Proxied, Comment: input.Comment}
}

func cloneRecord(record Record) Record {
	return record
}
