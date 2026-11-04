package daemon

import (
	"fmt"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/agentresume"
	"github.com/shaul/mesh/internal/hostmetrics"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
)

func BenchmarkStateBrokerSnapshot(b *testing.B) {
	broker := newStateBroker(1, time.Now)
	broker.sessions = make(map[string]protocol.SessionInfo, 64)
	broker.services = make(map[string]protocol.ServiceInfo, 16)
	broker.memory = make(map[string]protocol.SessionMemory, 64)
	for i := 0; i < 64; i++ {
		row := cloneBenchmarkSession(i)
		broker.sessions[row.ID] = row
		broker.memory[row.ID] = protocol.SessionMemory{Bytes: uint64(i), Available: i%2 == 0}
	}
	for i := 0; i < 16; i++ {
		service := protocol.ServiceInfo{Name: "service-" + string(rune('a'+i)), Kind: "proxy", Target: "http://127.0.0.1:3000", Listens: []protocol.ServiceListen{{Public: 3000 + i, Upstream: 4000 + i}}, Run: &protocol.ServiceRun{Command: "bun run dev", Cwd: "/work/project", Env: []string{"NODE_ENV=development"}, IdleMillis: 1000}, Demand: &protocol.ServiceDemand{State: protocol.DemandRunning, Unbound: []string{"tcp"}}}
		broker.services[service.Name] = service
	}
	sub := &stateSubscriber{topics: map[string]bool{protocol.TopicHost: true, protocol.TopicSessions: true, protocol.TopicServices: true, protocol.TopicMetrics: true}}
	broker.metrics = &hostmetrics.Snapshot{Temperatures: []hostmetrics.ComponentTemperature{{Kind: hostmetrics.KindCPU, Label: "CPU", Celsius: 60}}, Cores: &hostmetrics.Reading[[]float64]{Value: []float64{1, 2, 3}}}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = broker.snapshotLocked(sub)
	}
}

func cloneBenchmarkSession(i int) protocol.SessionInfo {
	now := time.Unix(int64(i), 0)
	exit := i
	return protocol.SessionInfo{ID: fmt.Sprintf("session-%02d", i), HostID: "host", Command: []string{"shell", "--session", "id"}, Cwd: "/work/project", State: "running", CreatedAt: now, LastAttachedAt: &now, DetachedAt: &now, ExitCode: &exit, Recovery: &recovery.Record{Agent: &agentresume.Recipe{Version: agentresume.Version, Launch: agentresume.Launch{Provider: agentresume.Claude, Executable: "/bin/claude", ProviderVersion: "1", Directory: "/work/project", DataRoot: "/work/data", Options: []string{"--resume"}}, ConversationID: "conversation", InvocationToken: "token", RegisteredAt: now, Lifecycle: agentresume.Active}, AgentResume: &agentresume.Receipt{SourceSessionID: "7K3D", Provider: agentresume.Claude, ConversationID: "conversation", InvocationToken: "token", VerifiedAt: now}, Version: recovery.Version, HostID: "host", SessionID: "7K3D", CheckpointAt: now, Shell: "bash", ShellDirectory: "/work/project", DirectorySource: recovery.DirectoryLaunch, Lines: []string{"line"}, Command: []string{"shell"}, Restart: &recovery.Command{Argv: []string{"shell"}, Cwd: "/work/project"}, Remote: &recovery.Target{HostID: "remote", SessionID: "9ABC"}}, Hibernated: &recovery.Hibernation{Version: 1, At: now, Reason: recovery.HibernateIdle, Provider: agentresume.Claude, ConversationID: "conversation"}, MemoryBytes: 1024}
}
