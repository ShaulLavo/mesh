package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/worker"
)

func listViewRows() []HostSessions {
	return []HostSessions{{Host: HostRecord{Alias: "pc"}, Sessions: []protocol.SessionInfo{
		{ID: "L1VE", Command: []string{"bash"}, Cwd: "/work", State: worker.StateDetached, CreatedAt: commandTestTime},
		{ID: "D0NE", Command: []string{"bash"}, Cwd: "/work", State: worker.StateExited, CreatedAt: commandTestTime},
		{ID: "L0ST", Command: []string{"bash"}, Cwd: "/work", State: worker.StateInterrupted, CreatedAt: commandTestTime},
		hibernatedRow("SL3P", "host-id", commandTestTime),
	}}}
}

func TestSessionListHidesEndedSessionsUnlessAll(t *testing.T) {
	var output bytes.Buffer
	hidden, err := writeSessionList(&output, commandTestTime, listViewRows(), listView{})
	if err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if hidden != 2 || !strings.Contains(got, "L1VE") || !strings.Contains(got, "SL3P") || strings.Contains(got, "D0NE") || strings.Contains(got, "L0ST") {
		t.Fatalf("default list (hidden %d) =\n%s", hidden, got)
	}
	output.Reset()
	if hidden, err = writeSessionList(&output, commandTestTime, listViewRows(), listView{all: true}); err != nil || hidden != 0 || !strings.Contains(output.String(), "D0NE") {
		t.Fatalf("--all list (hidden %d, %v) =\n%s", hidden, err, &output)
	}
	var notice bytes.Buffer
	if err := reportHiddenSessions(&notice, 2, nil); err != nil || notice.String() != "2 ended sessions hidden; mesh ls --all lists them\n" {
		t.Fatalf("hidden notice = %q, %v", notice.String(), err)
	}
}

func TestSessionListCutsLongCommandsToTheTerminal(t *testing.T) {
	rows := []HostSessions{{Host: HostRecord{Alias: "pc"}, Sessions: []protocol.SessionInfo{
		{ID: "L0NG", Command: []string{"bash", "-ic", strings.Repeat("for id in abc; ", 60)}, Cwd: "/work", State: worker.StateRunning, CreatedAt: commandTestTime},
	}}}
	var output bytes.Buffer
	if _, err := writeSessionList(&output, commandTestTime, rows, listView{width: 80}); err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(strings.TrimSpace(output.String()), "\n") {
		if width := len([]rune(line)); width > 80 {
			t.Fatalf("line is %d columns wide: %q", width, line)
		}
	}
	if !strings.Contains(output.String(), "L0NG") || !strings.Contains(output.String(), "…") {
		t.Fatalf("cut list =\n%s", &output)
	}
}

func TestListOmitsThisHostFromTheFanOut(t *testing.T) {
	stateDir := t.TempDir()
	self, _, err := identity.LoadOrCreate(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	hosts, alias := withoutThisHost(stateDir, []HostRecord{{Alias: "omarchy", ID: self.ID}, {Alias: "mac", ID: "mac-id"}})
	if alias != "omarchy" || len(hosts) != 1 || hosts[0].Alias != "mac" {
		t.Fatalf("withoutThisHost = %+v, %q", hosts, alias)
	}
	if _, alias := withoutThisHost(stateDir, []HostRecord{{Alias: "mac", ID: "mac-id"}}); alias != localHostAlias {
		t.Fatalf("unadopted self alias = %q", alias)
	}
}
