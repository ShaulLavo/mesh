package cli

import (
	"bytes"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/agentresume"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
	"github.com/shaul/mesh/internal/worker"
)

func testHibernation(at time.Time) *recovery.Hibernation {
	return &recovery.Hibernation{Version: 1, At: at, Reason: recovery.HibernateIdle, Provider: agentresume.Claude, ConversationID: "conversation"}
}

func hibernatedRow(id, hostID string, at time.Time) protocol.SessionInfo {
	code := 0
	return protocol.SessionInfo{
		ID: id, HostID: hostID, Command: []string{"claude"}, Cwd: "/work", State: worker.StateExited,
		CreatedAt: commandTestTime.Add(-24 * time.Hour), ExitCode: &code, Hibernated: testHibernation(at),
	}
}

func TestFormatBytes(t *testing.T) {
	for _, test := range []struct {
		bytes uint64
		want  string
	}{
		{0, "-"},
		{512, "512B"},
		{1536, "1.5K"},
		{412 << 20, "412M"},
		{1288490188, "1.2G"},
		{(1 << 20) - 1, "1.0M"},
		{10 << 30, "10G"},
	} {
		if got := formatBytes(test.bytes); got != test.want {
			t.Errorf("formatBytes(%d) = %q, want %q", test.bytes, got, test.want)
		}
	}
}

func TestCompactDurations(t *testing.T) {
	for _, test := range []struct {
		elapsed time.Duration
		want    string
	}{
		{45 * time.Second, "45s"},
		{12*time.Minute + 30*time.Second, "12m"},
		{3*time.Hour + 59*time.Minute, "3h"},
		{50 * time.Hour, "2d"},
		{-time.Minute, "0s"},
	} {
		if got := compactDuration(test.elapsed); got != test.want {
			t.Errorf("compactDuration(%s) = %q, want %q", test.elapsed, got, test.want)
		}
	}
}

func TestSessionIdleAndMemoryCells(t *testing.T) {
	now := commandTestTime
	live := protocol.SessionInfo{ID: "7K3D", State: worker.StateDetached, CreatedAt: now.Add(-3 * time.Hour), MemoryBytes: 412 << 20,
		Recovery: &recovery.Record{LastOutputAt: now.Add(-12 * time.Minute)}}
	silent := protocol.SessionInfo{ID: "91AZ", State: worker.StateRunning, CreatedAt: now.Add(-45 * time.Second)}
	exited := protocol.SessionInfo{ID: "Q2W3", State: worker.StateExited, CreatedAt: now.Add(-time.Hour), MemoryBytes: 1 << 20}
	asleep := hibernatedRow("ZZ99", "host-id", now.Add(-2*24*time.Hour))
	woken := asleep
	woken.ReplacementID = "AB12"
	for _, test := range []struct {
		name                string
		row                 protocol.SessionInfo
		state, idle, memory string
	}{
		{"live since last output", live, "detached", "12m", "412M"},
		{"live never printed", silent, "running", "45s", "-"},
		{"exited", exited, "exited", "-", "-"},
		{"hibernated", asleep, "hibernated", "2d", "-"},
		{"woken is an ordinary exit", woken, "exited", "-", "-"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := displayState(test.row); got != test.state {
				t.Errorf("state = %q, want %q", got, test.state)
			}
			if got := sessionIdle(now, test.row); got != test.idle {
				t.Errorf("idle = %q, want %q", got, test.idle)
			}
			if got := sessionMemory(test.row); got != test.memory {
				t.Errorf("memory = %q, want %q", got, test.memory)
			}
		})
	}
}

func TestProtocolSessionTableShowsHibernationIdleAndMemory(t *testing.T) {
	var output bytes.Buffer
	live := protocol.SessionInfo{ID: "7K3D", HostID: "host-id", Command: []string{"bash"}, Cwd: "/work", State: worker.StateDetached,
		CreatedAt: commandTestTime.Add(-time.Hour), MemoryBytes: 412 << 20}
	if err := writeProtocolSessions(&output, commandTestTime, []HostSessions{{Host: HostRecord{Alias: "pc"}, Sessions: []protocol.SessionInfo{
		live, hibernatedRow("91AZ", "host-id", commandTestTime.Add(-3*time.Hour)),
	}}}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 3 || strings.Join(strings.Fields(lines[0]), " ") != "HOST ID STATE AGE IDLE MEM STARTED IN COMMAND CACHE" {
		t.Fatalf("session table =\n%s", &output)
	}
	if fields := strings.Fields(lines[1]); strings.Join(fields[:6], " ") != "pc 7K3D detached 1h 1h 412M" {
		t.Fatalf("live row = %q", lines[1])
	}
	if fields := strings.Fields(lines[2]); strings.Join(fields[:6], " ") != "pc 91AZ hibernated 1d 3h -" {
		t.Fatalf("hibernated row = %q", lines[2])
	}
}

func TestLocalRowsReadTheHibernationMarker(t *testing.T) {
	setupCommandTestHost(t)
	writeLocalSessionDir(t, "7K3D", worker.StateExited)
	writeLocalSessionDir(t, "91AZ", worker.StateExited)
	dir, err := paths.SessionDir("7K3D")
	if err != nil {
		t.Fatal(err)
	}
	marker := *testHibernation(commandTestTime.Add(-time.Hour))
	if err := recovery.WriteHibernation(dir, marker); err != nil {
		t.Fatal(err)
	}
	rows, err := localSessionRows()
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]protocol.SessionInfo{}
	for _, row := range rows {
		byID[row.ID] = row
	}
	if got := Hibernation(byID["7K3D"]); got == nil || got.Provider != agentresume.Claude || !got.At.Equal(marker.At) {
		t.Fatalf("hibernated local row = %#v", byID["7K3D"].Hibernated)
	}
	if byID["91AZ"].Hibernated != nil {
		t.Fatalf("ordinary exit read as hibernated: %#v", byID["91AZ"].Hibernated)
	}

	current, err := Find("7K3D")
	if err != nil {
		t.Fatal(err)
	}
	if resolvedHibernation(resolvedSession{local: &current}) == nil {
		t.Fatal("attach by ID would not wake the hibernated local session")
	}
	other, err := Find("91AZ")
	if err != nil {
		t.Fatal(err)
	}
	if resolvedHibernation(resolvedSession{local: &other}) != nil {
		t.Fatal("attach by ID would wake an ordinary exit")
	}
}

func TestWakeDecisionFollowsTheCatalogRow(t *testing.T) {
	host := HostRecord{Alias: "pc", ID: "host-id"}
	asleep := hibernatedRow("7K3D", host.ID, commandTestTime)
	woken := asleep
	woken.ReplacementID = "9ABC"
	live := protocol.SessionInfo{ID: "7K3D", State: worker.StateDetached, Hibernated: testHibernation(commandTestTime)}
	for _, test := range []struct {
		name string
		row  protocol.SessionInfo
		wake bool
	}{
		{"hibernated", asleep, true},
		{"already resumed elsewhere", woken, false},
		{"marker on a live row is ignored", live, false},
		{"plain exit", protocol.SessionInfo{ID: "7K3D", State: worker.StateExited}, false},
	} {
		if got := resolvedHibernation(resolvedSession{host: &host, remote: test.row}) != nil; got != test.wake {
			t.Errorf("%s: wake = %v, want %v", test.name, got, test.wake)
		}
	}
}

func TestAttachByIDResumesAHibernatedRemoteSession(t *testing.T) {
	host := setupCommandTestHost(t)
	host.recoverTo = "9ABC"
	host.listRows = func() []protocol.SessionInfo {
		return []protocol.SessionInfo{hibernatedRow("7K3D", host.host.ID, commandTestTime.Add(-time.Hour))}
	}
	_, stderr, err := executeCommand(t, Dependencies{DialHost: host.dial, Now: func() time.Time { return commandTestTime }}, "7K3D")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "resuming hibernated claude conversation") {
		t.Fatalf("stderr = %q", stderr)
	}
	host.mu.Lock()
	recovered, attached := host.recovered, host.attach
	host.mu.Unlock()
	if recovered.SessionID != "7K3D" || recovered.RecoveryAction != string(recovery.ActionDefault) {
		t.Fatalf("recover request = %#v, want default recovery of 7K3D", recovered)
	}
	if attached.SessionID != "9ABC" {
		t.Fatalf("attached %q, want the replacement 9ABC", attached.SessionID)
	}
}

func TestAttachRacingHibernationRetriesThroughRecovery(t *testing.T) {
	host := setupCommandTestHost(t)
	host.recoverTo = "9ABC"
	var stopped atomic.Bool
	host.listRows = func() []protocol.SessionInfo {
		if stopped.Load() {
			return []protocol.SessionInfo{hibernatedRow("7K3D", host.host.ID, commandTestTime)}
		}
		return []protocol.SessionInfo{{ID: "7K3D", HostID: host.host.ID, Command: []string{"claude"}, Cwd: "/work",
			State: worker.StateDetached, CreatedAt: commandTestTime}}
	}
	host.attachError = func(id string) (string, string) {
		if id != "7K3D" {
			return "", ""
		}
		stopped.Store(true)
		return protocol.ReasonHibernating, "session is hibernating; attach again to resume it"
	}
	_, stderr, err := executeCommand(t, Dependencies{DialHost: host.dial, Now: func() time.Time { return commandTestTime }}, "7K3D")
	if err != nil {
		t.Fatalf("attach during hibernation = %v (stderr %q)", err, stderr)
	}
	host.mu.Lock()
	recovered, attached := host.recovered, host.attach
	host.mu.Unlock()
	if recovered.SessionID != "7K3D" || attached.SessionID != "9ABC" {
		t.Fatalf("recovered %q and attached %q, want 7K3D resumed as 9ABC", recovered.SessionID, attached.SessionID)
	}
}

func TestResumeFlagWakesAHibernatedSessionOnlyWhenNothingIsLive(t *testing.T) {
	for _, test := range []struct {
		name         string
		rows         func(hostID string) []protocol.SessionInfo
		wantRecover  bool
		wantAttached string
	}{
		{
			name: "only hibernated",
			rows: func(hostID string) []protocol.SessionInfo {
				older := hibernatedRow("A1B2", hostID, commandTestTime.Add(-time.Hour))
				newer := hibernatedRow("C3D4", hostID, commandTestTime.Add(-2*time.Hour))
				attached := commandTestTime.Add(-10 * time.Hour)
				newer.LastAttachedAt = &attached
				return []protocol.SessionInfo{older, newer}
			},
			wantRecover: true, wantAttached: "9ABC",
		},
		{
			name: "live session wins",
			rows: func(hostID string) []protocol.SessionInfo {
				return []protocol.SessionInfo{
					hibernatedRow("C3D4", hostID, commandTestTime.Add(-time.Hour)),
					{ID: "E5F6", HostID: hostID, Command: []string{"bash"}, Cwd: "/work", State: worker.StateDetached, CreatedAt: commandTestTime.Add(-48 * time.Hour)},
				}
			},
			wantAttached: "E5F6",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			host := setupCommandTestHost(t)
			host.recoverTo = "9ABC"
			host.listRows = func() []protocol.SessionInfo { return test.rows(host.host.ID) }
			if _, _, err := executeCommand(t, Dependencies{DialHost: host.dial, Now: func() time.Time { return commandTestTime }}, "pc", "-r"); err != nil {
				t.Fatal(err)
			}
			host.mu.Lock()
			recovered, attached := host.recovered, host.attach
			host.mu.Unlock()
			if (recovered.SessionID != "") != test.wantRecover || attached.SessionID != test.wantAttached {
				t.Fatalf("recovered %q, attached %q", recovered.SessionID, attached.SessionID)
			}
			if test.wantRecover && recovered.SessionID != "C3D4" {
				t.Fatalf("woke %q, want the most recently active C3D4", recovered.SessionID)
			}
		})
	}
}

func TestHibernateCommandReportsEachOutcome(t *testing.T) {
	host := setupCommandTestHost(t)
	stdout, _, err := executeCommand(t, Dependencies{DialHost: host.dial, Now: func() time.Time { return commandTestTime }}, "hibernate", "7k3d", "ZZZZ")
	if !strings.Contains(stdout, "hibernated 7K3D") {
		t.Fatalf("stdout = %q", stdout)
	}
	if err == nil || !strings.Contains(err.Error(), "ZZZZ") {
		t.Fatalf("missing failure for the unknown session: %v", err)
	}
	if request := host.actedOn(); request.Type != protocol.TypeHibernate || request.SessionID != "7K3D" || request.HibernateIdleMillis != 0 {
		t.Fatalf("hibernate request = %#v, want an explicit request for 7K3D", request)
	}

	host.hibernateError = "worker: session 7K3D: " + worker.ErrNotHibernatable.Error()
	_, _, err = executeCommand(t, Dependencies{DialHost: host.dial, Now: func() time.Time { return commandTestTime }}, "hibernate", "7K3D")
	if err == nil || err.Error() != "session 7K3D on pc: no running agent conversation is registered in this session" {
		t.Fatalf("refusal = %v", err)
	}
}

func TestHibernationRefusalsReadWell(t *testing.T) {
	for _, test := range []struct{ message, want string }{
		{"worker: session 7K3D: session is attached; detach it first", "session 7K3D: session is attached; detach it first"},
		{"no running agent conversation is registered in this session", "session 7K3D: no running agent conversation is registered in this session"},
		{"daemon: unknown control \"session.hibernate\"", "session 7K3D: its host predates hibernation; update Mesh there"},
		{"expected session.attach", "session 7K3D: its worker predates hibernation; kill it instead, or leave it running"},
		{"", "session 7K3D: hibernation was refused"},
	} {
		if got := hibernationRefused("7K3D", test.message).Error(); got != test.want {
			t.Errorf("hibernationRefused(%q) = %q, want %q", test.message, got, test.want)
		}
	}
}
