package updateinstall

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/updategate"
)

func TestUnsafeServiceConfigurationFailsBeforeGrantOrGate(t *testing.T) {
	f := newFixture(t)
	root := t.TempDir()
	command := []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$MESH_TEST_SERVICE_AUDIT\"\nprintf '%s\\n' control-group\n")
	if err := os.WriteFile(filepath.Join(root, "systemctl"), command, 0700); err != nil { //nolint:gosec // isolated executable service-manager fixture
		t.Fatal(err)
	}
	audit := filepath.Join(root, "audit")
	t.Setenv("PATH", root+":"+os.Getenv("PATH"))
	t.Setenv("MESH_TEST_SERVICE_AUDIT", audit)
	f.engine.cfg.Service = &SystemService{Spec: ServiceSpec{Kind: "systemd", Name: "mesh.service"}}
	if _, err := f.engine.Stage(context.Background(), f.request); err == nil || !strings.Contains(err.Error(), "KillMode=process") {
		t.Fatalf("unsafe service accepted: %v", err)
	}
	if _, err := f.engine.Read(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsafe service received an installation journal: %v", err)
	}
	if err := updategate.Check(f.engine.cfg.StateDir); err != nil {
		t.Fatalf("unsafe service blocked worker launches: %v", err)
	}
	data, err := os.ReadFile(audit) //nolint:gosec // explicit audit path beneath this test's directory
	if err != nil || strings.Contains(string(data), "stop") {
		t.Fatalf("preflight stopped daemon: %s %v", data, err)
	}
	f.roundTrip()
}
