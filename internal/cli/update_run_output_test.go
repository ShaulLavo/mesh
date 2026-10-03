package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/updatebootstrap"
	"github.com/shaul/mesh/internal/updateinstall"
)

func TestFirstCoordinatorBootstrapFailurePresentation(t *testing.T) {
	for _, mode := range []string{"default", "details", "json"} {
		t.Run(mode, func(t *testing.T) {
			stateDir, local, _, file := firstCoordinatorFleet(t)
			client, _ := updateTestRelease(t)
			store, err := update.OpenStore(stateDir)
			if err != nil {
				t.Fatal(err)
			}
			policy := filepath.Join(stateDir, "updates", "administrators.json")
			original := []byte(`{"version":1,"keys":[]}`)
			if err := os.WriteFile(policy, original, 0o600); err != nil {
				t.Fatal(err)
			}
			var runID, cause string
			caller := updateCallFunc(func(_ context.Context, host update.Host, action string, input, output any) error {
				if action == "info" {
					return legacyCoordinatorInfo(local, host, output)
				}
				if action != "status" {
					t.Fatalf("unexpected action: %s", action)
				}
				run, err := store.Read(input.(update.Operation).ID)
				if err != nil {
					return fmt.Errorf("read fixture update: %w", err)
				}
				*output.(*update.Run) = run
				return nil
			})
			bootstrap := func(_ context.Context, request updatebootstrap.Request, config updatebootstrap.Config) (updateinstall.Status, error) {
				runID = request.ID
				err := config.Enroll(stateDir, request.CoordinatorID)
				if err == nil {
					t.Fatal("explicit policy unexpectedly allowed enrollment")
				}
				err = fmt.Errorf("enroll fixture coordinator: %w", err)
				cause = err.Error()
				return updateinstall.Status{}, err
			}
			dependencies := Dependencies{UpdateBuild: updateTestBuild, UpdateInspect: firstCoordinatorObservation(local), UpdateRelease: client, UpdateCaller: caller, UpdateBootstrap: bootstrap}
			args := []string{"update", "--fleet", file, "--yes"}
			if mode != "default" {
				args = append(args, "--"+mode)
			}
			stdout, stderr, err := executeCommand(t, dependencies, args...)
			if code, ok := StatusCode(err); !ok || code != 1 || runID == "" {
				t.Fatalf("bootstrap failure was swallowed: %v, run %q", err, runID)
			}
			run, err := store.Read(runID)
			if err != nil {
				t.Fatal(err)
			}
			if !run.Stopped || run.Problem != "Coordinator bootstrap failed: "+cause {
				t.Fatalf("durable failure changed: %+v", run)
			}
			after, err := os.ReadFile(policy) //nolint:gosec // Policy path belongs to this test's temporary state directory.
			if err != nil || string(after) != string(original) {
				t.Fatalf("failure rewrote explicit policy: %v", err)
			}
			assertBootstrapFailureOutput(t, mode, stdout, stderr, run)
			statusArgs := []string{"update", "status", runID}
			if mode != "default" {
				statusArgs = append(statusArgs, "--"+mode)
			}
			stdout, stderr, err = executeCommand(t, dependencies, statusArgs...)
			if code, ok := StatusCode(err); !ok || code != 1 {
				t.Fatalf("saved status lost failure: %v", err)
			}
			assertBootstrapFailureOutput(t, mode, stdout, stderr, run)
		})
	}
}

func assertBootstrapFailureOutput(t *testing.T, mode, stdout, stderr string, run update.Run) {
	t.Helper()
	if mode == "json" {
		encoded, err := json.Marshal(run)
		if err != nil {
			t.Fatal(err)
		}
		if stdout != string(encoded)+"\n" || stderr != "" {
			t.Errorf("JSON operational record changed: %s; stderr %q", stdout, stderr)
		}
		return
	}
	if mode == "details" {
		if !strings.Contains(stdout, run.Problem) {
			t.Errorf("details lost original cause: %s", stdout)
		}
		return
	}
	t.Logf("BOOTSTRAP FAILURE DEFAULT\n%s", stdout)
	if strings.Contains(stdout+stderr, run.Problem) || strings.Contains(stdout+stderr, strings.TrimPrefix(run.Problem, "Coordinator bootstrap failed: ")) {
		t.Errorf("default exposed internal bootstrap cause: %s; stderr %q", stdout, stderr)
	}
	if !strings.Contains(stdout, "Mesh could not finish this update.") {
		t.Errorf("default hid failure: %s", stdout)
	}
	action := "Run mesh update status " + run.ID + " --details to review this saved update."
	if !strings.Contains(stdout, action) || strings.Count(stdout, "Run mesh ") != 1 {
		t.Errorf("expected one saved-run action %q: %s", action, stdout)
	}
}
