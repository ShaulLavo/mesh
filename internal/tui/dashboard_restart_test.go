package tui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/updateinstall"
	"github.com/shaul/mesh/internal/usagefeed"
)

func TestDashboardRestartNoticeStaysOnOneLine(t *testing.T) {
	for _, usage := range []bool{false, true} {
		input := cli.DashboardInput{Wall: true, Notice: "Dashboard restart failed: permission denied"}
		if usage {
			input.UsageWatch = func(_ context.Context, _ func(usagefeed.Result)) error { return nil }
		}
		model := newDashboard(input, time.Now())
		model.width, model.height = 140, 40
		lines := strings.Split(ansi.Strip(model.render()), "\n")
		if !strings.Contains(lines[len(lines)-1], input.Notice) {
			t.Fatalf("restart notice missing from final row: %q", lines[len(lines)-1])
		}
		if strings.Count(strings.Join(lines, "\n"), input.Notice) != 1 {
			t.Fatal("restart notice repeated")
		}
	}
}

func TestDashboardRejectedReplacementRendersPlainNotice(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "mesh")
	healthy := []byte("healthy new mesh")
	sum := sha256.Sum256(healthy)
	build := release.Build{Digest: hex.EncodeToString(sum[:])}
	if err := os.WriteFile(path, healthy, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "update"), 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(updateinstall.Status{Schema: 1, Phase: updateinstall.Committed, Settings: updateinstall.Settings{Executable: path}, Verified: &updateinstall.Health{HostID: "local", Build: build}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "update", "installation.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(root, "replacement")
	if err := os.WriteFile(replacement, []byte("unvalidated replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	err = updateinstall.WithCommittedExecutable(t.Context(), root, "local", build, func(string) error { t.Fatal("executed replacement before its healthy commit"); return nil })
	if !errors.Is(err, release.ErrExecutableChecksumMismatch) {
		t.Fatalf("handoff lost internal integrity detail: %v", err)
	}
	const expected = "Dashboard restart failed: verify installed mesh: executable differs from the healthy daemon"
	notice := "Dashboard restart failed: " + err.Error()
	if notice != expected {
		t.Fatalf("handoff human notice changed: %q", notice)
	}
	for _, wall := range []bool{false, true} {
		model := newDashboard(cli.DashboardInput{Wall: wall, Notice: notice}, time.Now())
		model.width, model.height = 140, 40
		frame := ansi.Strip(model.render())
		lines := strings.Split(frame, "\n")
		footer := strings.TrimSpace(lines[len(lines)-1])
		if footer != expected || strings.Count(frame, expected) != 1 || strings.Contains(frame, release.ErrExecutableChecksumMismatch.Error()) {
			t.Fatalf("wall=%t leaked internal detail or lost plain notice: %q", wall, footer)
		}
		t.Logf("wall=%t rendered footer: %s", wall, footer)
	}
}
