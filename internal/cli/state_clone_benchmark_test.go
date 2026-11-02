package cli

import (
	"testing"
	"time"

	"github.com/shaul/mesh/internal/agentresume"
	"github.com/shaul/mesh/internal/hostmetrics"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
)

func BenchmarkCloneSessionInfo(b *testing.B) {
	rows := benchmarkSessionRows(64)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = cloneSessionInfo(rows)
	}
}

func BenchmarkAuditCloneSessionInfo(b *testing.B) {
	BenchmarkCloneSessionInfo(b)
}

func BenchmarkStateViewClone(b *testing.B) {
	view := StateView{Sessions: benchmarkSessionRows(64), Services: benchmarkServiceRows(16), Memory: map[string]protocol.SessionMemory{"session": {Bytes: 1}}, Sections: map[string]ObservedSection{"sessions": {Observation: protocol.Observation{AgeMillis: 1}}}, Metrics: benchmarkMetrics()}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = view.Clone()
	}
}

func BenchmarkAuditStateViewClone(b *testing.B) {
	BenchmarkStateViewClone(b)
}

func benchmarkSessionRows(count int) []protocol.SessionInfo {
	rows := make([]protocol.SessionInfo, count)
	for i := range rows {
		now := time.Unix(int64(i), 0)
		exit := i
		rows[i] = protocol.SessionInfo{ID: "session", HostID: "host", Command: []string{"shell", "--session", "id"}, Cwd: "/work/project", State: "running", CreatedAt: now, LastAttachedAt: &now, DetachedAt: &now, ExitCode: &exit, Recovery: &recovery.Record{Agent: &agentresume.Recipe{Version: agentresume.Version, Launch: agentresume.Launch{Provider: agentresume.Claude, Executable: "/bin/claude", ProviderVersion: "1", Directory: "/work/project", DataRoot: "/work/data", Options: []string{"--resume"}}, ConversationID: "conversation", InvocationToken: "token", RegisteredAt: now, Lifecycle: agentresume.Active}, AgentResume: &agentresume.Receipt{SourceSessionID: "7K3D", Provider: agentresume.Claude, ConversationID: "conversation", InvocationToken: "token", VerifiedAt: now}, Version: recovery.Version, HostID: "host", SessionID: "7K3D", CheckpointAt: now, Shell: "bash", ShellDirectory: "/work/project", DirectorySource: recovery.DirectoryLaunch, Lines: []string{"line"}, Command: []string{"shell"}, Restart: &recovery.Command{Argv: []string{"shell"}, Cwd: "/work/project"}, Remote: &recovery.Target{HostID: "remote", SessionID: "9ABC"}}, Hibernated: &recovery.Hibernation{Version: 1, At: now, Reason: recovery.HibernateIdle, Provider: agentresume.Claude, ConversationID: "conversation"}, MemoryBytes: uint64(i)}
	}
	return rows
}

func benchmarkServiceRows(count int) []protocol.ServiceInfo {
	rows := make([]protocol.ServiceInfo, count)
	for i := range rows {
		rows[i] = protocol.ServiceInfo{Name: "service", Kind: "proxy", Target: "http://127.0.0.1:3000", Listens: []protocol.ServiceListen{{Public: 3000, Upstream: 4000}}, Run: &protocol.ServiceRun{Command: "bun run dev", Cwd: "/work/project", Env: []string{"NODE_ENV=development"}, IdleMillis: 1000}, Demand: &protocol.ServiceDemand{State: protocol.DemandRunning, Unbound: []string{"tcp"}}}
	}
	return rows
}

func benchmarkMetrics() *hostmetrics.Snapshot {
	return &hostmetrics.Snapshot{Temperatures: []hostmetrics.ComponentTemperature{{Kind: hostmetrics.KindCPU, Label: "CPU", Celsius: 60}}, Cores: &hostmetrics.Reading[[]float64]{Value: []float64{1, 2, 3}}}
}
