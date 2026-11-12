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
			"executable_boothash":         "first-boot-image",
			"executable_cdhash":           "first-code-image",
			"executable_file":             "first-image-file",
		},
	}
}

func TestInvocationSnapshotNormalizesLaunchdPerImageKeys(t *testing.T) {
	before := managerInvocationFixture()
	after := before
	after.Environment = maps.Clone(before.Environment)
	after.Environment["executable_boothash"] = "second-boot-image"
	after.Environment["executable_cdhash"] = "second-code-image"
	after.Environment["executable_file"] = "second-image-file"
	arguments, environment := invocationSnapshot(before)
	nextArguments, nextEnvironment := invocationSnapshot(after)
	if arguments != nextArguments || environment != nextEnvironment {
		t.Fatal("launchd per-image changes altered preserved service invocation")
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
	for _, key := range []string{"CONFIGURED_VALUE", "MESH_UPDATE_CACHE_DIR", "SYSTEMD_EXEC_PID_CONFIGURED", "executable_boothash_CONFIGURED", "executable_cdhash_extra", "prefix_executable_file"} {
		t.Run(key, func(t *testing.T) { testConfiguredInvocationChange(t, key) })
	}
}

func testConfiguredInvocationChange(t *testing.T, key string) {
	before := managerInvocationFixture()
	after := before
	after.Environment = maps.Clone(before.Environment)
	after.Environment["SYSTEMD_EXEC_PID"] = "200"
	after.Environment["executable_boothash"] = "second-boot-image"
	after.Environment["executable_cdhash"] = "second-code-image"
	after.Environment["executable_file"] = "second-image-file"
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

func TestInvocationSnapshotManagerNormalizationKeepsArgumentsAndDefinitionGuards(t *testing.T) {
	invocation := managerInvocationFixture()
	arguments, environment := invocationSnapshot(invocation)
	before := ServiceSnapshot{Definition: "original-definition", Arguments: arguments, Environment: environment}
	changedDefinition := before
	changedDefinition.Definition = "changed-definition"
	if snapshotsEqual(before, changedDefinition) {
		t.Fatal("environment normalization waived service definition preservation")
	}
	invocation.Arguments = []string{"/fixture/mesh", "daemon", "--tailnet-port=1"}
	changedArguments := before
	changedArguments.Arguments, changedArguments.Environment = invocationSnapshot(invocation)
	if snapshotsEqual(before, changedArguments) {
		t.Fatal("environment normalization waived configured argument preservation")
	}
}
