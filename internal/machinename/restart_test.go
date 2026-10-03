package machinename

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/shaul/mesh/internal/identity"
)

func TestRestartRecoversDirectoryDurabilityBeforePublishingOrRetrying(t *testing.T) {
	for _, operation := range []string{"initialization", "rename"} {
		t.Run(operation, func(t *testing.T) {
			directory := t.TempDir()
			host, _, err := identity.LoadOrCreate(directory)
			if err != nil {
				t.Fatal(err)
			}
			failed := errors.New("fixture directory sync failed")
			failSync := func(string) error { return failed }
			var initial *Store
			switch operation {
			case "initialization":
				initial, err = openStore(t.Context(), directory, host.ID, "office-pc", failSync)
				if initial != nil || !errors.Is(err, failed) {
					t.Fatal("failed initialization directory sync published a store")
				}
			case "rename":
				initial, err = Open(t.Context(), directory, host.ID, "office-pc")
				if err != nil {
					t.Fatal(err)
				}
				initial.syncDirectory = failSync
				if _, _, err := initial.Rename(t.Context(), host.ID, "travel-pc", 1); !errors.Is(err, failed) {
					t.Fatal("failed rename directory sync acknowledged a name")
				}
				if initial.Current().Revision != 1 {
					t.Fatal("failed rename published the visible replacement")
				}
			}
			visible, err := readRecord(filepath.Join(directory, stateName))
			if err != nil {
				t.Fatal(err)
			}
			wantRevision := uint64(1)
			if operation == "rename" {
				wantRevision = 2
			}
			if visible.Revision != wantRevision {
				t.Fatal("control did not leave the intended replacement visible")
			}

			t.Run("failed-recovery", func(t *testing.T) {
				syncs := 0
				restarted, err := openStore(t.Context(), directory, host.ID, "os-host", func(string) error {
					syncs++
					return failed
				})
				if syncs != 1 || restarted != nil || !errors.Is(err, failed) {
					t.Fatalf("failed recovery exposed an unsynced claim: syncs=%d storeReturned=%t err=%v", syncs, restarted != nil, err)
				}
			})
			t.Run("successful-recovery", func(t *testing.T) {
				syncs := 0
				restarted, err := openStore(t.Context(), directory, host.ID, "os-host", func(path string) error {
					syncs++
					if path != directory {
						t.Fatal("recovery synced another directory")
					}
					return syncDirectory(path)
				})
				if err != nil || restarted == nil || syncs != 1 {
					t.Fatalf("restart returned before directory durability: syncs=%d storeReturned=%t err=%v", syncs, restarted != nil, err)
				}
				if restarted.Current() != visible.Claim {
					t.Fatal("recovery changed the visible claim")
				}
				previous := visible.Revision
				if operation == "rename" {
					previous = 1
				}
				retried, changed, err := restarted.Rename(t.Context(), host.ID, visible.MachineName, previous)
				if err != nil || changed || retried != visible.Claim || syncs != 1 {
					t.Fatalf("recovered retry differs: changed=%t syncs=%d err=%v", changed, syncs, err)
				}
			})
		})
	}
}
