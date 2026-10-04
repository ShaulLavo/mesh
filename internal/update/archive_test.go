package update

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

//go:embed testdata/archive/*.json
var publishedArchives embed.FS

func publishedArchive(t *testing.T, version string) (*Store, string, []byte) {
	t.Helper()
	data, err := publishedArchives.ReadFile("testdata/archive/" + version + "-run.json")
	if err != nil {
		t.Fatal(err)
	}
	proof, err := publishedArchives.ReadFile("testdata/archive/" + version + "-receipt.json")
	if err != nil {
		t.Fatal(err)
	}
	var receipt struct {
		Version   string `json:"version"`
		RunSHA256 string `json:"runSha256"`
	}
	if err := json.Unmarshal(proof, &receipt); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	if receipt.Version != version || receipt.RunSHA256 != hex.EncodeToString(digest[:]) {
		t.Fatal("published daemon fixture differs from its creation receipt")
	}
	var header struct{ ID string }
	if err := json.Unmarshal(data, &header); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.path(header.ID), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return store, header.ID, data
}

func TestApprovedArchivePublishedRuns(t *testing.T) {
	for _, version := range []string{"v0.1.159", "v0.1.167"} {
		t.Run(version, func(t *testing.T) {
			store, id, original := publishedArchive(t, version)
			run, err := store.Read(id)
			if err != nil {
				t.Fatal(err)
			}
			for _, host := range run.Fleet.Members {
				if host.MachineName != "" || host.Label() != host.ID {
					t.Fatal("archived label became a live machine name")
				}
			}
			response, err := json.Marshal(run)
			if err != nil || bytes.Contains(response, []byte(`"alias"`)) {
				t.Fatalf("archive escaped operation storage: %s %v", response, err)
			}
			before := archiveFields(t, original)
			fleetPath := filepath.Join(t.TempDir(), "fleet.json")
			if err := os.WriteFile(fleetPath, before["fleet"], 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadFleet(fleetPath); err == nil {
				t.Fatal("live fleet reader accepted an archived label")
			}
			changed, err := store.Change(id, func(current *Run) error {
				current.Cached = true
				current.Targets[0].State = Granted
				current.Targets[0].Grant = true
				current.Targets[0].Generation++
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			reopened, err := store.Read(id)
			if err != nil || reopened.Membership != run.Membership || reopened.ReleaseDigest != run.ReleaseDigest || !reopened.Targets[0].Grant || reopened.Targets[0].Generation != changed.Targets[0].Generation {
				t.Fatalf("progress lost approved operation: %#v %v", reopened, err)
			}
			afterData, err := os.ReadFile(store.path(id))
			if err != nil {
				t.Fatal(err)
			}
			after := archiveFields(t, afterData)
			for _, field := range []string{"id", "coordinator", "fleet", "membership", "release", "releaseDigest", "createdAt"} {
				var first, second bytes.Buffer
				if err := json.Compact(&first, before[field]); err != nil {
					t.Fatal(err)
				}
				if err := json.Compact(&second, after[field]); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(first.Bytes(), second.Bytes()) {
					t.Fatalf("progress changed original approval field %s", field)
				}
			}
		})
	}
}

func archiveFields(t *testing.T, data []byte) map[string]json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	return fields
}

func mutatePublishedArchive(t *testing.T, apply func(map[string]any)) (*Store, string) {
	t.Helper()
	store, id, data := publishedArchive(t, "v0.1.167")
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	apply(fields)
	mutated, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.path(id), mutated, 0o600); err != nil {
		t.Fatal(err)
	}
	return store, id
}

func TestApprovedArchiveTargetLabelsHaveNoNamingEffect(t *testing.T) {
	store, id := mutatePublishedArchive(t, func(fields map[string]any) {
		for _, target := range fields["targets"].([]any) {
			target.(map[string]any)["host"].(map[string]any)["alias"] = "arbitrary archived display text"
		}
	})
	run, err := store.Read(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range run.Targets {
		if target.Host.MachineName != "" || target.Host.Label() != target.Host.ID {
			t.Fatal("archived target label changed live naming")
		}
	}
}

func TestApprovedArchiveStrictTamperDenials(t *testing.T) {
	cases := map[string]func(map[string]any){
		"root unknown":  func(f map[string]any) { f["unknown"] = true },
		"fleet unknown": func(f map[string]any) { f["fleet"].(map[string]any)["unknown"] = true },
		"member unknown": func(f map[string]any) {
			f["fleet"].(map[string]any)["members"].([]any)[0].(map[string]any)["unknown"] = true
		},
		"member label digest": func(f map[string]any) {
			f["fleet"].(map[string]any)["members"].([]any)[0].(map[string]any)["alias"] = "tampered"
		},
		"member null label": func(f map[string]any) {
			f["fleet"].(map[string]any)["members"].([]any)[0].(map[string]any)["alias"] = nil
		},
		"member nonstring label": func(f map[string]any) {
			f["fleet"].(map[string]any)["members"].([]any)[0].(map[string]any)["alias"] = 42
		},
		"member missing label": func(f map[string]any) {
			delete(f["fleet"].(map[string]any)["members"].([]any)[0].(map[string]any), "alias")
		},
		"target missing label": func(f map[string]any) {
			delete(f["targets"].([]any)[0].(map[string]any)["host"].(map[string]any), "alias")
		},
		"target unknown": func(f map[string]any) { f["targets"].([]any)[0].(map[string]any)["unknown"] = true },
		"target host unknown": func(f map[string]any) {
			f["targets"].([]any)[0].(map[string]any)["host"].(map[string]any)["unknown"] = true
		},
		"target identity": func(f map[string]any) {
			f["targets"].([]any)[0].(map[string]any)["host"].(map[string]any)["id"] = strings.Repeat("A", 43)
		},
		"target endpoint": func(f map[string]any) {
			f["targets"].([]any)[0].(map[string]any)["host"].(map[string]any)["endpoint"] = "ws://different.invalid/mesh"
		},
		"target dependency": func(f map[string]any) {
			f["targets"].([]any)[0].(map[string]any)["host"].(map[string]any)["dependsOn"] = []string{strings.Repeat("A", 43)}
		},
		"membership":       func(f map[string]any) { f["membership"] = strings.Repeat("f", 64) },
		"release identity": func(f map[string]any) { f["releaseDigest"] = strings.Repeat("f", 64) },
	}
	for name, apply := range cases {
		t.Run(name, func(t *testing.T) {
			store, id := mutatePublishedArchive(t, apply)
			if _, err := store.Read(id); err == nil {
				t.Fatal("accepted corrupt archived approval")
			}
		})
	}
}

func TestApprovedArchiveProgressCannotReapprove(t *testing.T) {
	cases := map[string]func(*Run){
		"id":          func(r *Run) { r.ID = strings.Repeat("f", 32) },
		"coordinator": func(r *Run) { r.Coordinator = strings.Repeat("A", 43) },
		"membership":  func(r *Run) { r.Membership = r.Fleet.Digest() },
		"release":     func(r *Run) { r.Release.Version = "v0.1.168"; r.ReleaseDigest = r.Release.Digest() },
		"fleet and targets": func(r *Run) {
			r.Fleet.Members[0].Endpoint = "ws://different.invalid/mesh"
			for index := range r.Targets {
				if r.Targets[index].Host.ID == r.Fleet.Members[0].ID {
					r.Targets[index].Host.Endpoint = r.Fleet.Members[0].Endpoint
				}
			}
		},
	}
	for name, apply := range cases {
		t.Run(name, func(t *testing.T) {
			store, id, original := publishedArchive(t, "v0.1.167")
			if _, err := store.Change(id, func(run *Run) error { apply(run); return nil }); err == nil {
				t.Fatal("progress rewrote original approved identity")
			}
			after, err := os.ReadFile(store.path(id))
			if err != nil || !bytes.Equal(original, after) {
				t.Fatal("denied progress changed archived operation")
			}
		})
	}
}

func TestApprovedArchiveRoutingAndTargetProgress(t *testing.T) {
	store, id, _ := publishedArchive(t, "v0.1.167")
	run, err := store.Read(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Targets) != 2 || len(run.Targets[0].Host.DependsOn) != 1 {
		t.Fatal("published fixture lost its actual routing dependency")
	}
	run.Targets[0].State, run.Targets[1].State = Offline, Staged
	if canGrant(run, 1) {
		t.Fatal("archived offline dependency authorized router activation")
	}
	changed, err := store.Change(id, func(current *Run) error {
		current.Targets[0], current.Targets[1] = current.Targets[1], current.Targets[0]
		current.Targets[0].Problem = "router progress"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Read(id)
	if err != nil || reopened.Targets[0].Host.ID != changed.Targets[0].Host.ID || reopened.Targets[0].Problem != "router progress" {
		t.Fatalf("progress attached to a different archived target: %#v %v", reopened, err)
	}
}

func TestApprovedArchiveRejectsTrailingData(t *testing.T) {
	store, id, data := publishedArchive(t, "v0.1.167")
	if err := os.WriteFile(store.path(id), append(data, []byte("\n{}")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(id); err == nil {
		t.Fatal("accepted trailing archived operation data")
	}
}
