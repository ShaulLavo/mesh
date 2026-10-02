package cli

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/shaul/mesh/internal/usagefeed"
)

func TestDashboardUsageConfigRoundTrip(t *testing.T) {
	for _, feedURL := range []string{"", "https://feed.example.test/v1.json"} {
		t.Run(feedURL, func(t *testing.T) {
			t.Setenv("MESH_CONFIG_DIR", t.TempDir())
			config := hostConfig{Version: hostConfigVersion, Dashboard: &DashboardSettings{Theme: "oled", UsageFeedURL: feedURL}}
			if err := writeHostConfig(config); err != nil {
				t.Fatal(err)
			}
			if err := SaveHost(HostRecord{Alias: "pc", ID: "host-key", MeshIdentity: "host-key", Endpoint: "ws://100.64.0.2:7777/mesh"}); err != nil {
				t.Fatal(err)
			}
			loaded, err := loadHostConfig()
			if err != nil {
				t.Fatal(err)
			}
			if loaded.Dashboard == nil || *loaded.Dashboard != *config.Dashboard {
				t.Fatalf("dashboard configuration changed: %+v", loaded.Dashboard)
			}
		})
	}
}

func TestDashboardUsageConfigValidation(t *testing.T) {
	for _, feedURL := range []string{"file:///private/feed", "https://user:secret@feed.example.test/v1.json", "https://feed.example.test/v1.json?token=secret", "https://feed.example.test/v1.json#fragment"} {
		config := hostConfig{Version: hostConfigVersion, Dashboard: &DashboardSettings{UsageFeedURL: feedURL}}
		if _, err := validateHostConfig(config, "hosts.json"); err == nil {
			t.Fatal("unsafe feed URL accepted")
		}
	}
	if _, err := validateHostConfig(hostConfig{Version: hostConfigVersion}, "hosts.json"); err != nil {
		t.Fatalf("absent dashboard configuration: %v", err)
	}
}

func TestDashboardUsageConstructionIsPassive(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	feed, err := usagefeed.New(usagefeed.Config{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	input := DashboardInput{UsageWatch: feed.Run}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := input.UsageWatch(ctx, func(usagefeed.Result) { t.Fatal("cancelled feed published") }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("construction or cancelled startup requested the feed")
	}
}
