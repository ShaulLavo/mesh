package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/updateinstall"
)

func TestUpdateCheckUsesReleaseDownloadBudget(t *testing.T) {
	setupUpdateCLI(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// MagicDNS stalls have exceeded the former five-second CLI deadline.
		timer := time.NewTimer(5100 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-r.Context().Done():
			return
		case <-timer.C:
			_ = json.NewEncoder(w).Encode(updateTestManifest())
		}
	}))
	t.Cleanup(server.Close)
	client := release.Client{BaseURL: server.URL, HTTPClient: server.Client()}
	caller := updateCallFunc(func(_ context.Context, host update.Host, _ string, _, output any) error {
		*output.(*update.Info) = update.Info{Health: updateinstall.Health{HostID: host.ID, Build: release.Build{Version: "v0.1.0"}}}
		return nil
	})
	_, _, err := executeCommand(t, Dependencies{UpdateRelease: client, UpdateCaller: caller}, "update", "--local", "--version", "v0.2.0", "--check", "--json")
	if err != nil {
		t.Fatalf("update check rejected a manifest within the release budget: %v", err)
	}
}
