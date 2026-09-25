//go:build linux

package worker

import (
	"slices"
	"testing"
)

func TestUnifiedCgroupPathReadsV2Line(t *testing.T) {
	contents := "1:name=systemd:/legacy\n0::/user.slice/user-1000.slice/user@1000.service/app.slice/mesh.service\n"
	if got := unifiedCgroupPath(contents); got != "/user.slice/user-1000.slice/user@1000.service/app.slice/mesh.service" {
		t.Fatalf("unifiedCgroupPath = %q", got)
	}
}

func TestParentSliceKeepsLauncherSlice(t *testing.T) {
	cases := map[string]string{
		"/user.slice/user-1000.slice/user@1000.service/app.slice/mesh.service": "app.slice",
		"/user.slice/user-1000.slice/session-3.scope":                          "user-1000.slice",
		"/user.slice/user-1000.slice/user@1000.service/init.scope":             "",
		"/": "",
	}
	for path, want := range cases {
		if got := parentSlice(path); got != want {
			t.Errorf("parentSlice(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestStartScopeArgsCountsProperties(t *testing.T) {
	args := startScopeArgs("T8BT", 42, "app.slice")
	want := []string{"--user", "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1",
		"org.freedesktop.systemd1.Manager", "StartTransientUnit", "ssa(sv)a(sa(sv))", "mesh-session-T8BT.scope", "fail", "4",
		"PIDs", "au", "1", "42",
		"Description", "s", "Mesh session T8BT",
		"CollectMode", "s", "inactive-or-failed",
		"Slice", "s", "app.slice",
		"0"}
	if !slices.Equal(args, want) {
		t.Fatalf("startScopeArgs =\n%q\nwant\n%q", args, want)
	}
	if args := startScopeArgs("T8BT", 42, ""); args[9] != "3" || slices.Contains(args, "Slice") {
		t.Fatalf("startScopeArgs without slice = %q", args)
	}
}
