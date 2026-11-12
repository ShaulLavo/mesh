package updatebootstrap

import (
	"maps"
	"testing"
)

func managerInvocationFixture() processInvocation {
	return processInvocation{
		Arguments: []string{"/fixture/mesh", "daemon", "--tailnet-port=0"},
		Environment: map[string]string{
			"HOME":                        "/fixture/home",
			"MESH_STATE_DIR":              "/fixture/state",
			"MESH_UPDATE_CACHE_DIR":       "/fixture/cache",
			"CONFIGURED_VALUE":            "original",
			"SYSTEMD_EXEC_PID_CONFIGURED": "retained",
			"INVOCATION_ID":               "first-invocation",
			"JOURNAL_STREAM":              "first-stream",
			"SYSTEMD_EXEC_PID":            "100",
		},
	}
}

func TestInvocationSnapshotNormalizesManagerPerExecutionKeys(t *testing.T) {
	before := managerInvocationFixture()
	after := before
	after.Environment = maps.Clone(before.Environment)
	after.Environment["INVOCATION_ID"] = "second-invocation"
	after.Environment["JOURNAL_STREAM"] = "second-stream"
	after.Environment["SYSTEMD_EXEC_PID"] = "200"
	arguments, environment := invocationSnapshot(before)
	nextArguments, nextEnvironment := invocationSnapshot(after)
	if arguments != nextArguments || environment != nextEnvironment {
		t.Fatal("manager-generated per-execution changes altered preserved service invocation")
	}
}

func TestInvocationSnapshotRetainsConfiguredEnvironmentChanges(t *testing.T) {
	for _, key := range []string{"CONFIGURED_VALUE", "MESH_UPDATE_CACHE_DIR", "SYSTEMD_EXEC_PID_CONFIGURED"} {
		t.Run(key, func(t *testing.T) { testConfiguredInvocationChange(t, key) })
	}
}

func testConfiguredInvocationChange(t *testing.T, key string) {
	before := managerInvocationFixture()
	after := before
	after.Environment = maps.Clone(before.Environment)
	after.Environment["SYSTEMD_EXEC_PID"] = "200"
	after.Environment[key] = "changed-configured-value"
	arguments, environment := invocationSnapshot(before)
	nextArguments, nextEnvironment := invocationSnapshot(after)
	if environment == nextEnvironment {
		t.Fatal("configured environment change was excluded from preservation")
	}
	if snapshotsEqual(ServiceSnapshot{Arguments: arguments, Environment: environment}, ServiceSnapshot{Arguments: nextArguments, Environment: nextEnvironment}) {
		t.Fatal("configured environment change retained the same service snapshot")
	}
}
