package execution

import (
	"fmt"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/session"
)

func TestJournalListPageByteBoundedAndKeysetCursor(t *testing.T) {
	j, err := Open(t.TempDir(), "boot-1", "owner-1")
	if err != nil {
		t.Fatal(err)
	}
	host := "khI9qfAZ1eqQXe4C2JhMIfS8lwSL_GC5Aef-MsKEYZE"
	baseTime := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)

	saveOne := func(class, principal, opaque, id string, instNum int, kind string, at time.Time) {
		k := Key{Host: host, Class: class, Principal: principal, Opaque: opaque}
		stdinMode := StdinClosed
		if kind == KindLine {
			stdinMode = StdinOpen
		}
		intent := Intent{Kind: kind, Command: []string{"/bin/true"}, Cwd: "/tmp", Stdin: stdinMode}
		ref := session.Reservation{ID: id, Instance: fmt.Sprintf("%032x", instNum), BootID: "boot-1", Purpose: PurposeForKind(kind)}
		j.now = func() time.Time { return at }
		if _, _, err := j.Reserve(t.Context(), k, intent, func() (Binding, error) {
			return NewBinding(k, intent, ref, nil), nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	ids := []string{"7K3A", "7K3B", "7K3C", "7K3D", "7K3E"}
	for i, id := range ids {
		saveOne("device", "device-a", fmt.Sprintf("job-a-%d", i), id, i+1, KindPipe, baseTime.Add(time.Duration(i)*time.Second))
	}
	saveOne("device", "device-b", "job-b-0", "8B3B", 99, KindPipe, baseTime.Add(10*time.Second))
	saveOne(InternalServiceClass, "internal-svc", "line-0", "8B3C", 100, KindLine, baseTime.Add(11*time.Second))

	// Force byte-bounded paging: each receipt reports 600 bytes with MaxBytes=1000 => 1 per page.
	var seen []string
	cursor := ""
	for pageNum := 0; pageNum < 10; pageNum++ {
		res, err := j.ListPage(t.Context(), ListQuery{
			Host:         host,
			Class:        "device",
			Principal:    "device-a",
			IncludeEnded: true,
			Limit:        10,
			Cursor:       cursor,
			MaxBytes:     1000,
		}, func(Receipt) int { return 600 })
		if err != nil {
			t.Fatalf("page %d: %v", pageNum, err)
		}
		if len(res.Receipts) != 1 {
			t.Fatalf("page %d len=%d, want 1 (byte-bounded)", pageNum, len(res.Receipts))
		}
		if res.Receipts[0].AcceptedAt.IsZero() {
			t.Fatal("expected non-zero AcceptedAt")
		}
		seen = append(seen, res.Receipts[0].Binding.Reservation.ID)
		if pageNum == 1 {
			// Insert a brand-new job while paging: keyset cursor must not repeat or skip older pages.
			saveOne("device", "device-a", "job-a-new", "9N3W", 200, KindPipe, baseTime.Add(30*time.Second))
		}
		if res.NextCursor == nil {
			break
		}
		// Cursor from ownerA must be rejected for ownerB or different IncludeEnded filter.
		if _, err := j.ListPage(t.Context(), ListQuery{Host: host, Class: "device", Principal: "device-b", IncludeEnded: true, Cursor: *res.NextCursor}, nil); err == nil {
			t.Fatal("expected cursor bound to device-a to fail for device-b")
		}
		if _, err := j.ListPage(t.Context(), ListQuery{Host: host, Class: "device", Principal: "device-a", IncludeEnded: false, Cursor: *res.NextCursor}, nil); err == nil {
			t.Fatal("expected cursor bound to all=true to fail for all=false")
		}
		cursor = *res.NextCursor
	}
	want := []string{"7K3E", "7K3D", "7K3C", "7K3B", "7K3A"}
	if fmt.Sprint(seen) != fmt.Sprint(want) {
		t.Fatalf("paged ids = %v, want %v", seen, want)
	}
}
