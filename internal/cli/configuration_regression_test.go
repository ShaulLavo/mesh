package cli

import (
	"bytes"
	"testing"

	"github.com/shaul/mesh/internal/update"
)

func TestConfigurationMergeKeepsReadableSizeBound(t *testing.T) {
	base := []byte(`{"version":1,"hosts":[]}`)
	document := append([]byte(nil), base...)
	document = append(document, bytes.Repeat([]byte(" "), clientConfigLimit-len(base)-1)...)
	path := writeClientConfigFixture(t, document)
	err := withFeedConfiguration(t.Context(), func(file update.FeedFile) error { return file.Save(update.FeedValue{Present: true}) })
	if err != nil {
		actual, readErr := readClientConfigTestFile(path)
		if readErr != nil || !bytes.Equal(actual, document) {
			t.Fatalf("rejected write changed original: %v", readErr)
		}
		return
	}
	data, err := readClientConfigTestFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > clientConfigLimit {
		t.Errorf("save succeeded but grew a valid %d-byte book to %d bytes, above the %d-byte reader cap", len(document), len(data), clientConfigLimit)
	}
	if _, err := readDashboardFeed(); err != nil {
		t.Errorf("successful save made the actual dashboard reader fail: %v", err)
	}
}
