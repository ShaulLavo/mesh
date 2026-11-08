package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/domainpolicy"
	"github.com/shaul/mesh/internal/update"
)

func TestConfigurationFeedMergePreservesExcludedBytes(t *testing.T) {
	original := []byte("{\n \"version\" : 1, \"hosts\": [],\n \"dashboard\" : { \"theme\" : \"rose-pine\", \"usageFeedURL\" : \"http://localhost/old\" }\n}\n")
	path := writeClientConfigFixture(t, original)
	err := withFeedConfiguration(t.Context(), func(file update.FeedFile) error {
		if file.Value() != (update.FeedValue{Present: true, URL: "http://localhost/old"}) {
			t.Fatal(file.Value())
		}
		if err := file.Save(update.FeedValue{Present: true, URL: ""}); err != nil {
			return fmt.Errorf("save fixture feed: %w", err)
		}
		if err := file.Settle(); err != nil {
			return fmt.Errorf("settle fixture feed: %w", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	actual, err := readClientConfigTestFile(path)
	if err != nil {
		t.Fatal(err)
	}
	expected := bytes.Replace(original, []byte(`"http://localhost/old"`), []byte(`""`), 1)
	if !bytes.Equal(actual, expected) {
		t.Fatalf("excluded bytes changed: %s", actual)
	}
}
func TestConfigurationFeedRejectsMalformedDocumentsWithoutWriting(t *testing.T) {
	for _, document := range []string{`{"version":1,"version":1,"hosts":[]}`, `{"version":1,"hosts":[],"secret":"not-an-allowed-key"}`, `{"version":1,"hosts":[],"dashboard":{"theme":"invalid"}}`} {
		t.Run(document, func(t *testing.T) {
			testInvalidConfigurationDocument(t, document)
		})
	}
}
func testInvalidConfigurationDocument(t *testing.T, document string) {
	t.Helper()
	path := writeClientConfigFixture(t, []byte(document))
	err := withFeedConfiguration(t.Context(), func(file update.FeedFile) error { return file.Save(update.FeedValue{Present: true}) })
	if err == nil {
		t.Fatal("invalid supported document was accepted")
	}
	actual, _ := readClientConfigTestFile(path)
	if string(actual) != document {
		t.Fatal("invalid document was changed")
	}
}
func TestDeliveredFeedUsesPrivateHTTPSContract(t *testing.T) {
	for _, url := range []string{"", "https://feed." + domainpolicy.Primary() + "/quota.json"} {
		if err := update.ValidateDeliveredFeed(url); err != nil {
			t.Fatal(err)
		}
	}
	for _, url := range []string{"http://feed." + domainpolicy.Primary() + "/quota", "https://example.com/", "https://feed." + domainpolicy.Primary() + "/?token=no", "https://user:secret@feed." + domainpolicy.Primary() + "/"} {
		if err := update.ValidateDeliveredFeed(url); err == nil {
			t.Fatal("accepted", url)
		}
	}
}
func TestConfigurationCancelledWriterDoesNotPublish(t *testing.T) {
	path := writeClientConfigFixture(t, []byte(`{"version":1,"hosts":[]}`))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := withFeedConfiguration(ctx, func(file update.FeedFile) error { return file.Save(update.FeedValue{Present: true}) })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	data, _ := readClientConfigTestFile(path)
	if strings.Contains(string(data), "usageFeedURL") {
		t.Fatal(string(data))
	}
}

func TestConfigurationExplicitDisableSurvivesSupportedHostEdit(t *testing.T) {
	writeClientConfigFixture(t, []byte(`{"version":1,"hosts":[],"dashboard":{"theme":"oled","usageFeedURL":""}}`))
	if err := SaveHost(HostRecord{ID: "khI9qfAZ1eqQXe4C2JhMIfS8lwSL_GC5Aef-MsKEYZE", MeshIdentity: "khI9qfAZ1eqQXe4C2JhMIfS8lwSL_GC5Aef-MsKEYZE", Endpoint: "ws://100.64.0.2:7777/mesh"}); err != nil {
		t.Fatal(err)
	}
	value, err := readDashboardFeed()
	if err != nil || value != (update.FeedValue{Present: true}) {
		t.Fatalf("unrelated host edit changed selected disable projection: %+v %v", value, err)
	}
}
