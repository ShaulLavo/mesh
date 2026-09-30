//go:build linux

package apps

import "testing"

func TestDataSSDMountRequiresExactMountpoint(t *testing.T) {
	mounts := "20 1 0:1 / / rw - ext4 /dev/root rw\n30 20 0:2 / /work rw - ext4 /dev/mapper/work rw\n31 20 0:3 / /data rw - ext4 /dev/sdb1 rw\n"
	if !hasDataMount(mounts, "/work") || !hasDataMount(mounts, "/data") {
		t.Fatal("missed mounted data SSD")
	}
	for _, missing := range []string{"/workload", "/data/child", "/mnt/work"} {
		if hasDataMount(mounts, missing) {
			t.Fatalf("accepted %s", missing)
		}
	}
	if hasDataMount("20 1 0:1 / / rw - ext4 /dev/root rw\n", "/work") {
		t.Fatal("root filesystem treated as /work mount")
	}
	if hasDataMount("30 20 0:2 / /work/child rw - ext4 /dev/mapper/work rw\n", "/work") {
		t.Fatal("child mount substituted for /work")
	}
	if requiredDataMount("/workload/apps") != "" || requiredDataMount("/database/apps") != "" {
		t.Fatal("lookalike path requires data drive")
	}
	if requiredDataMount("/data/apps") != "/data" || requiredDataMount("/work") != "/work" {
		t.Fatal("data drive path not recognized")
	}
}
func TestMountpointEscapesAreDecoded(t *testing.T) {
	if got := unescapeMountPath(`/mount\040name\134folder`); got != "/mount name\\folder" {
		t.Fatalf("mountpoint decode=%q", got)
	}
}
