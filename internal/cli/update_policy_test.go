package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/update"
)

func TestHostPolicyCLILocalRevisionAndOff(t *testing.T) {
	_, local := setupUpdateCLI(t)
	policy := update.HostPolicy{Schema: 1, Revision: 7, Mode: update.HostAutomatic, After: []string{local.ID}, Soak: time.Hour}
	var changes []update.HostPolicyChange
	caller := updateCallFunc(func(_ context.Context, host update.Host, action string, input, output any) error {
		if host.ID != local.ID || !update.IsLocal(host) {
			return errors.New("policy called a remote host")
		}
		if action == "host-policy.set" {
			change := input.(update.HostPolicyChange)
			changes = append(changes, change)
			if change.ExpectedRevision != policy.Revision {
				return errors.New("host policy revision changed")
			}
			policy = change.Policy
			policy.Revision++
		}
		*output.(*update.HostPolicy) = policy
		return nil
	})
	dependencies := Dependencies{UpdateCaller: caller}
	stdout, _, err := executeCommand(t, dependencies, "update", "policy", "show", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got update.HostPolicy
	if err := json.Unmarshal([]byte(stdout), &got); err != nil || got.Revision != 7 {
		t.Fatalf("policy show: %s, %v", stdout, err)
	}
	stdout, _, err = executeCommand(t, dependencies, "update", "policy", "off", "--expected-revision", "7")
	if err != nil || !strings.Contains(stdout, "already granted may finish") {
		t.Fatalf("off failed to describe pending work: %s %v", stdout, err)
	}
	if len(changes) != 1 || changes[0].Policy.Mode != update.HostManual || len(changes[0].Policy.After) != 1 || changes[0].Policy.Soak != time.Hour {
		t.Fatalf("off changed ordering or skipped revision: %+v", changes)
	}
	_, _, err = executeCommand(t, dependencies, "update", "policy", "set", "--expected-revision", "7")
	if err == nil || !strings.Contains(err.Error(), "revision changed") {
		t.Fatalf("stale set accepted: %v", err)
	}
}

func TestHostPolicyCLIRejectsMissingRevisionAndCoordinator(t *testing.T) {
	setupUpdateCLI(t)
	caller := updateCallFunc(func(context.Context, update.Host, string, any, any) error {
		t.Fatal("invalid policy request reached transport")
		return nil
	})
	for _, args := range [][]string{{"update", "policy", "off"}, {"update", "policy", "set"}, {"update", "policy", "show", "--coordinator", "pc"}} {
		if _, _, err := executeCommand(t, Dependencies{UpdateCaller: caller}, args...); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
