package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/agentresume"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/worker"
)

func hibernationCandidateDir(t *testing.T, state string, detachedAt *time.Time, lifecycle agentresume.Lifecycle, lastOutput time.Time) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "7K3D")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC)
	if err := worker.WriteMeta(dir, worker.Meta{ID: "7K3D", PID: 4321, Command: []string{"/bin/bash"}, State: state, CreatedAt: created, DetachedAt: detachedAt}); err != nil {
		t.Fatal(err)
	}
	record := recovery.Record{Version: recovery.Version, HostID: "host-one", SessionID: "7K3D", Shell: "/bin/bash",
		ShellDirectory: "/project", DirectorySource: recovery.DirectoryLaunch, Command: []string{"/bin/bash"},
		CheckpointAt: lastOutput, LastOutputAt: lastOutput}
	if lifecycle != "" {
		record.Agent = &agentresume.Recipe{Version: 1, Launch: agentresume.Launch{Provider: agentresume.Claude, Executable: "/bin/claude",
			ProviderVersion: "2.1.281 (Claude Code)", Directory: "/project", DataRoot: "/home/user/.claude"},
			ConversationID: "exact", InvocationToken: "token", RegisteredAt: created, Lifecycle: lifecycle}
	}
	if err := recovery.Write(dir, record); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestHibernationCandidateNeedsADetachedQuietActiveAgent(t *testing.T) {
	now := time.Date(2026, 9, 24, 20, 0, 0, 0, time.UTC)
	longAgo := now.Add(-7 * time.Hour)
	recently := now.Add(-time.Hour)
	for _, tc := range []struct {
		name       string
		state      string
		detachedAt *time.Time
		lifecycle  agentresume.Lifecycle
		lastOutput time.Time
		want       bool
	}{
		{"idle agent", worker.StateDetached, &longAgo, agentresume.Active, longAgo, true},
		{"attached", worker.StateRunning, nil, agentresume.Active, longAgo, false},
		{"detached recently", worker.StateDetached, &recently, agentresume.Active, longAgo, false},
		{"older worker without detach time", worker.StateDetached, nil, agentresume.Active, longAgo, false},
		{"still producing output", worker.StateDetached, &longAgo, agentresume.Active, recently, false},
		{"agent closed by the user", worker.StateDetached, &longAgo, agentresume.Closed, longAgo, false},
		{"explicit binding", worker.StateDetached, &longAgo, agentresume.Explicit, longAgo, false},
		{"plain shell", worker.StateDetached, &longAgo, "", longAgo, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := hibernationCandidateDir(t, tc.state, tc.detachedAt, tc.lifecycle, tc.lastOutput)
			if got := hibernationCandidate(dir, "host-one", 6*time.Hour, now); got != tc.want {
				t.Fatalf("candidate = %v, want %v", got, tc.want)
			}
		})
	}
	dir := hibernationCandidateDir(t, worker.StateDetached, &longAgo, agentresume.Active, longAgo)
	if hibernationCandidate(dir, "another-host", 6*time.Hour, now) {
		t.Fatal("a checkpoint owned by another host was accepted")
	}
}

func TestListReportsDetachTimeAndUnwokenHibernation(t *testing.T) {
	detachedAt := time.Date(2026, 9, 24, 8, 0, 0, 0, time.UTC)
	dir := hibernationCandidateDir(t, worker.StateDetached, &detachedAt, agentresume.Active, detachedAt)
	meta, err := worker.ReadMeta(dir)
	info := protocol.SessionInfo{ID: "7K3D", State: string(storage.StateDetached)}
	addHibernationInfo(dir, meta, err, &info)
	if info.DetachedAt == nil || !info.DetachedAt.Equal(detachedAt) || info.Hibernated != nil {
		t.Fatalf("detached info = %+v", info)
	}

	marker := recovery.Hibernation{Version: 1, At: detachedAt.Add(6 * time.Hour), Reason: recovery.HibernateIdle, Provider: agentresume.Claude, ConversationID: "exact"}
	if err := recovery.WriteHibernation(dir, marker); err != nil {
		t.Fatal(err)
	}
	info = protocol.SessionInfo{ID: "7K3D", State: string(storage.StateExited)}
	addHibernationInfo(dir, meta, err, &info)
	if info.Hibernated == nil || info.Hibernated.ConversationID != "exact" || info.DetachedAt != nil {
		t.Fatalf("hibernated info = %+v", info)
	}
	woken := protocol.SessionInfo{ID: "7K3D", State: string(storage.StateExited), ReplacementID: "8XYZ"}
	addHibernationInfo(dir, meta, err, &woken)
	if woken.Hibernated != nil {
		t.Fatal("a session already woken into a replacement still reads as hibernated")
	}
}
