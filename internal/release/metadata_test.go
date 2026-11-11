package release

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func testUpdateMetadata() UpdateMetadata {
	return UpdateMetadata{Schema: 1, Epoch: 1, Trust: 1, Version: "v0.1.4", Manifest: strings.Repeat("a", 64),
		PublishedAt: time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC), Position: Position{Anchor: true, Ordinal: 2},
		Direct:   DirectSources{AnchorsFrom: 0, Since: []string{"v0.1.2", "v0.1.3"}},
		Rollback: []RollbackRef{{Forward: strings.Repeat("b", 64), Rollback: strings.Repeat("c", 64)}},
		Page:     AssetRef{Tag: "v0.1.3", Name: "mesh-anchors-0.json", SHA256: strings.Repeat("d", 64)}}
}

func TestUpdateMetadataContract(t *testing.T) {
	metadata := testUpdateMetadata()
	contents, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeUpdateMetadata(contents)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Position != metadata.Position || decoded.Page != metadata.Page || decoded.Manifest != metadata.Manifest {
		t.Fatal("metadata identity changed during round trip")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(contents, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"schema", "epoch", "trust", "version", "manifest", "publishedAt", "position", "direct", "rollback", "page"} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("missing reviewed field %s", key)
		}
	}
	if len(fields) != 10 {
		t.Fatal("serialized metadata contract changed")
	}
}

func TestUpdateMetadataRejectsInvalidIdentity(t *testing.T) {
	cases := map[string]func(*UpdateMetadata){
		"schema":             func(m *UpdateMetadata) { m.Schema++ },
		"epoch":              func(m *UpdateMetadata) { m.Epoch = 0 },
		"trust":              func(m *UpdateMetadata) { m.Trust = 0 },
		"manifest":           func(m *UpdateMetadata) { m.Manifest = strings.Repeat("A", 64) },
		"time":               func(m *UpdateMetadata) { m.PublishedAt = time.Time{} },
		"anchor range":       func(m *UpdateMetadata) { m.Direct.AnchorsFrom = 3 },
		"future page":        func(m *UpdateMetadata) { m.Page.Tag = "v0.1.5" },
		"source target":      func(m *UpdateMetadata) { m.Direct.Since = []string{m.Version} },
		"source duplicate":   func(m *UpdateMetadata) { m.Direct.Since = []string{"v0.1.2", "v0.1.2"} },
		"source order":       func(m *UpdateMetadata) { m.Direct.Since = []string{"v0.1.3", "v0.1.2"} },
		"rollback digest":    func(m *UpdateMetadata) { m.Rollback[0].Rollback = "bad" },
		"rollback duplicate": func(m *UpdateMetadata) { m.Rollback = append(m.Rollback, m.Rollback[0]) },
		"oversize":           func(m *UpdateMetadata) { m.Rollback = make([]RollbackRef, 600) },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) { assertInvalidMetadata(t, change) })
	}
}

func TestUpdateMetadataCurrentReleasePage(t *testing.T) {
	metadata := testUpdateMetadata()
	metadata.Page.Tag = metadata.Version
	if err := metadata.Validate(); err != nil {
		t.Fatal(err)
	}
}

func assertInvalidMetadata(t *testing.T, change func(*UpdateMetadata)) {
	t.Helper()
	metadata := testUpdateMetadata()
	change(&metadata)
	if err := metadata.Validate(); err == nil {
		t.Fatal("accepted invalid metadata")
	}
}

func TestMetadataByteBounds(t *testing.T) {
	metadata := testUpdateMetadata()
	contents, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	padded := contents
	padded = append(padded, []byte(strings.Repeat(" ", MaxMetadataBytes-len(contents)))...)
	if _, err := DecodeUpdateMetadata(padded); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeUpdateMetadata(append(padded, ' ')); err == nil {
		t.Fatal("accepted oversized document")
	}
	for index := range 500 {
		metadata.Rollback = append(metadata.Rollback, RollbackRef{Forward: fmtDigest(index), Rollback: strings.Repeat("a", 64)})
	}
	if err := metadata.Validate(); err == nil {
		t.Fatal("producer accepted oversized metadata")
	}
}

func fmtDigest(index int) string {
	const digits = "0123456789abcdef"
	return strings.Repeat("0", 61) + string([]byte{digits[(index>>8)&15], digits[(index>>4)&15], digits[index&15]})
}

func TestAssetRefBounds(t *testing.T) {
	for _, name := range []string{"", ".", "..", "../page", "page/name", "page\\name", "page?query", "page%2fpath", "page name", "页面", strings.Repeat("a", 129)} {
		ref := testUpdateMetadata().Page
		ref.Name = name
		if err := ref.Validate(); err == nil {
			t.Errorf("accepted asset name %q", name)
		}
	}
	ref := testUpdateMetadata().Page
	ref.Name = strings.Repeat("a", MaxAssetNameBytes)
	if err := ref.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestUpdaterReaderCoordinates(t *testing.T) {
	for _, kind := range []string{"normal", "waypoint", "absent", "schema", "skipped epoch", "old epoch", "normal ordinal", "normal anchor", "waypoint ordinal", "waypoint anchor", "publication anchor", "overflow"} {
		t.Run(kind, func(t *testing.T) { testUpdaterCoordinates(t, kind) })
	}
}

func testUpdaterCoordinates(t *testing.T, kind string) {
	m := testUpdateMetadata()
	m.Updater = &UpdaterReader{Schema: 1, Epoch: m.Epoch, Position: m.Position}
	switch kind {
	case "absent":
		m.Updater = nil
	case "schema":
		m.Updater.Schema = 2
	case "skipped epoch":
		m.Updater.Epoch = m.Epoch + 2
	case "old epoch":
		m.Epoch = 2
		m.Updater.Epoch = 1
	case "normal ordinal":
		m.Updater.Position.Ordinal++
	case "normal anchor":
		m.Updater.Position.Anchor = false
	case "waypoint", "waypoint ordinal", "waypoint anchor", "publication anchor", "overflow":
		prepareWaypointMetadata(&m, kind)
	}
	err := m.Validate()
	valid := kind == "normal" || kind == "waypoint" || kind == "absent"
	if valid && err != nil {
		t.Fatal(err)
	}
	if !valid && err == nil {
		t.Fatal("accepted inconsistent updater source coordinates")
	}
}

func prepareWaypointMetadata(m *UpdateMetadata, kind string) {
	m.Updater.Epoch = m.Epoch + 1
	m.Updater.Position = Position{Anchor: true, Ordinal: 0}
	switch kind {
	case "waypoint ordinal":
		m.Updater.Position.Ordinal = 1
	case "waypoint anchor":
		m.Updater.Position.Anchor = false
	case "publication anchor":
		m.Position.Anchor = false
	case "overflow":
		m.Epoch = ^uint32(0)
		m.Updater.Epoch = 0
	}
}

func TestUpdateMetadataIgnoresCompatibleUnknownFields(t *testing.T) {
	m := testUpdateMetadata()
	m.Updater = &UpdaterReader{Schema: 1, Epoch: m.Epoch, Position: m.Position}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatal(err)
	}
	object["future"] = json.RawMessage(`{"notRequiredByThisReader":true}`)
	object["updater"] = json.RawMessage(`{"schema":1,"epoch":1,"position":{"anchor":true,"ordinal":2},"future":"compatible"}`)
	data, err = json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeUpdateMetadata(data)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Updater == nil || *decoded.Updater != *m.Updater || decoded.Manifest != m.Manifest {
		t.Fatal("compatible future metadata changed known coordinates")
	}
	if _, err := DecodeUpdateMetadata(append(data, []byte(` {}`)...)); err == nil {
		t.Fatal("trailing document was treated as an unknown field")
	}
}
