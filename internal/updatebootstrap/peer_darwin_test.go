package updatebootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/shaul/mesh/internal/updateinstall"
)

func TestDarwinMappedExecutableChild(t *testing.T) {
	socket := os.Getenv("MESH_MAPPED_IMAGE_SOCKET")
	if socket == "" {
		t.Skip("child process for native executable mapping proof")
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	conn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = io.Copy(io.Discard, conn)
}

func TestDarwinMappedExecutableSurvivesAtomicReplacement(t *testing.T) {
	t.Run("process attribution", TestMappedProcessSets)
	t.Run("command results and file resolution", TestDarwinMappedExecutableCommandResults)
	t.Run("strict singleton callers", TestDarwinMappedExecutableSingletonCallers)
	t.Run("24 connected children", testDarwinMappedExecutableGroup)
	root, err := os.MkdirTemp("/tmp", "mesh-map-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(root) }()
	data, err := os.ReadFile(os.Args[0]) //nolint:gosec // copy this Go test executable into an isolated native mapping fixture
	if err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(root, "mesh")
	if err = os.WriteFile(original, data, 0700); err != nil { //nolint:gosec // executable child fixture
		t.Fatal(err)
	}
	socket := filepath.Join(root, "sock")
	child := exec.Command(original, "-test.run=^TestDarwinMappedExecutableChild$") //nolint:gosec // this exact test binary runs only the Unix socket child fixture
	child.Env = append(os.Environ(), "MESH_MAPPED_IMAGE_SOCKET="+socket)
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn := connectMappedChild(t, ctx, socket)
	defer func() { _ = conn.Close() }()
	before, err := peerImage(ctx, conn)
	if err != nil || before.PID != child.Process.Pid {
		t.Fatalf("initial loaded image: %+v %v", before, err)
	}
	var stat unix.Stat_t
	if err = unix.Stat(original, &stat); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(root, ".mesh-update-proof.previous")
	if err = os.Link(original, backup); err != nil {
		t.Fatal(err)
	}
	writeMappedImageJournal(t, root, original)
	if active := activeInstallationPath(root, executableImage{Path: backup, Installed: backup, PID: child.Process.Pid}); active != original {
		t.Fatalf("hardlink name became installation destination: %s", active)
	}
	candidate := filepath.Join(root, "candidate")
	if err = os.WriteFile(candidate, []byte("different replacement image"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(candidate, original); err != nil {
		t.Fatal(err)
	}
	after, err := peerImage(ctx, conn)
	if err != nil || after.PID != before.PID {
		t.Fatalf("retained mapping lost after atomic replacement: %+v %v", after, err)
	}
	retained, err := os.ReadFile(after.Path) //nolint:gosec // OS-identified image path produced by the real native peer probe
	if err != nil || hashBytes(retained) != hashBytes(data) {
		t.Fatalf("probe identified replacement bytes as retained image: %v", err)
	}
	device, inode := darwinDevice(stat.Dev), stat.Ino
	fallback, ok := mappedMeshImage(original, device, inode, child.Process.Pid)
	if !ok || fallback.Path != backup {
		t.Fatalf("stale lsof pathname could not resolve exact retained inode: %+v", fallback)
	}
	copyPath := filepath.Join(root, "copy")
	if err = os.WriteFile(copyPath, data, 0600); err != nil { //nolint:gosec // fixed same-byte control file inside this test's private temporary directory
		t.Fatal(err)
	}
	if _, ok = loadedMeshImage(copyPath, device, inode, child.Process.Pid); ok {
		t.Fatal("same bytes with a different inode were accepted as mapped image")
	}
	if _, ok = loadedMeshImage(backup, device+1, inode, child.Process.Pid); ok {
		t.Fatal("same inode on a different device was accepted")
	}
	t.Logf("native replacement proof: pid=%d device=%#x inode=%d retained=%s installed=%s", child.Process.Pid, device, inode, after.Path, original)
}

func connectMappedChild(t *testing.T, ctx context.Context, socket string) net.Conn {
	t.Helper()
	for ctx.Err() == nil {
		conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
		if err == nil {
			return conn
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("native mapped-image child did not listen")
	return nil
}

func writeMappedImageJournal(t *testing.T, stateDir, executable string) {
	t.Helper()
	dir := filepath.Join(stateDir, "update")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(updateinstall.Status{Schema: 1, Settings: updateinstall.Settings{Executable: executable}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "installation.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func testDarwinMappedExecutableGroup(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "mesh-map-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var stat unix.Stat_t
	if err = unix.Stat(executable, &stat); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pids := make([]int, 24)
	children := make([]*exec.Cmd, 24)
	for i := range pids {
		children[i], pids[i] = startMappedChild(t, ctx, executable, filepath.Join(root, strconv.Itoa(i)))
	}
	distinct := make(map[int]bool, len(pids))
	for _, pid := range pids {
		distinct[pid] = true
	}
	if len(distinct) != 24 {
		t.Fatalf("distinct peers = %d", len(distinct))
	}
	started := time.Now()
	if _, err = processImage(ctx, pids[0]); err != nil {
		t.Fatal(err)
	}
	singleton := time.Since(started)
	original := mappedImageCommand
	t.Cleanup(func() { mappedImageCommand = original })
	calls := 0
	mappedImageCommand = func(ctx context.Context, args ...string) ([]byte, error) {
		calls++
		names := make([]string, len(pids))
		for i, pid := range pids {
			names[i] = strconv.Itoa(pid)
		}
		want := []string{"-a", "-p", strings.Join(names, ","), "-d", "txt", "-F", "pfinD"}
		if !reflect.DeepEqual(args, want) {
			t.Fatalf("native command = %v, want %v", args, want)
		}
		return original(ctx, args...)
	}
	started = time.Now()
	results := processImages(ctx, pids)
	elapsed := time.Since(started)
	if calls != 1 {
		t.Fatalf("native calls = %d, want 1 for 24 distinct PIDs", calls)
	}
	assertMappedGroup(t, results, pids, stat, -1)
	t.Logf("native lookup timing: singleton=%s; 24 distinct connected PIDs=%s; native calls=%d", singleton, elapsed, calls)
	if err = unix.Kill(pids[7], unix.SIGKILL); err != nil {
		t.Fatal(err)
	}
	// Reap before the next lsof, so the missing process is deterministic.
	_ = children[7].Wait()
	results = processImages(ctx, pids)
	if calls != 2 {
		t.Fatalf("second group native calls = %d, want 2 total", calls)
	}
	assertMappedGroup(t, results, pids, stat, 7)
	if got := processImages(ctx, nil); len(got) != 0 || calls != 2 {
		t.Fatalf("empty native group = %v, calls=%d", got, calls)
	}
}

func startMappedChild(t *testing.T, ctx context.Context, executable, socket string) (*exec.Cmd, int) {
	t.Helper()
	child := exec.Command(executable, "-test.run=^TestDarwinMappedExecutableChild$") //nolint:gosec // this test executable runs only the connected mapping child
	child.Env = append(os.Environ(), "MESH_MAPPED_IMAGE_SOCKET="+socket)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	conn := connectMappedChild(t, ctx, socket)
	t.Cleanup(func() { _ = conn.Close() })
	pid, err := peerPID(conn)
	if err != nil || pid != child.Process.Pid {
		t.Fatalf("child peer PID=%d, expected=%d: %v", pid, child.Process.Pid, err)
	}
	return child, pid
}

func assertMappedGroup(t *testing.T, results []processImageResult, pids []int, expected unix.Stat_t, missing int) {
	t.Helper()
	if len(results) != len(pids) {
		t.Fatalf("results = %d, requested=%d", len(results), len(pids))
	}
	for i, result := range results {
		assertMappedResult(t, result, pids[i], expected, i == missing)
	}
}

func assertMappedResult(t *testing.T, result processImageResult, pid int, expected unix.Stat_t, missing bool) {
	t.Helper()
	if missing {
		if result.err == nil || result.image.Path != "" {
			t.Fatalf("dead child resolved: %+v", result)
		}
		return
	}
	if result.err != nil || result.image.PID != pid {
		t.Fatalf("mapped child %d: %+v", pid, result)
	}
	var actual unix.Stat_t
	if err := unix.Stat(result.image.Path, &actual); err != nil {
		t.Fatal(err)
	}
	if actual.Ino != expected.Ino || darwinDevice(actual.Dev) != darwinDevice(expected.Dev) {
		t.Fatalf("child %d mapped another device/inode", pid)
	}
}

func TestDarwinMappedExecutableCommandResults(t *testing.T) {
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var stat unix.Stat_t
	if err = unix.Stat(path, &stat); err != nil {
		t.Fatal(err)
	}
	records := fmt.Sprintf("ftxt\ni%d\nD%#x\nn%s\n", stat.Ino, darwinDevice(stat.Dev), path)
	// Incomplete records cannot borrow fields from the next file. A real Mesh
	// mapping must still resolve after unrelated and incomplete txt records.
	realistic := "ftxt\ni1\nD0x1\nn/not-mesh\nftxt\ni" + strconv.FormatUint(stat.Ino, 10) + "\nftxt\nD" + fmt.Sprintf("%#x", darwinDevice(stat.Dev)) + "\nn" + path + "\n" + records
	one := mappedExitError(t, "exit 1")
	two := mappedExitError(t, "exit 2")
	signal := mappedExitError(t, "kill -TERM $$")
	unavailable := &exec.Error{Name: "lsof", Err: exec.ErrNotFound}
	tests := []struct {
		name, output string
		commandErr   error
		cancel       bool
		proved       []bool
	}{
		{name: "success reordered", output: "p22\n" + realistic + "p11\n" + records, proved: []bool{true, true}},
		{name: "exit one missing", output: "p22\n" + realistic, commandErr: one, proved: []bool{false, true}},
		{name: "exit one invalid evidence", output: "p11\nftxt\ni1\nD0x1\nn/not-mesh\np22\n" + records, commandErr: one, proved: []bool{false, true}},
		{name: "exit one incomplete evidence", output: "p11\nftxt\ni" + strconv.FormatUint(stat.Ino, 10) + "\nftxt\nD" + fmt.Sprintf("%#x", darwinDevice(stat.Dev)) + "\nn" + path + "\np22\n" + records, commandErr: one, proved: []bool{false, true}},
		{name: "empty output", proved: []bool{false, false}},
		{name: "cancellation partial output", output: "p11\n" + records, commandErr: one, cancel: true, proved: []bool{false, false}},
		{name: "other exit", output: "p11\n" + records + "p22\n" + records, commandErr: two, proved: []bool{false, false}},
		{name: "signal", output: "p11\n" + records, commandErr: signal, proved: []bool{false, false}},
		{name: "unavailable", commandErr: unavailable, proved: []bool{false, false}},
		{name: "unattributable trailing header", output: "p11\n" + records + "pbad\n", commandErr: one, proved: []bool{false, false}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			checkMappedCommandResult(t, test.output, test.commandErr, test.cancel, test.proved)
		})
	}
}

func mappedExitError(t *testing.T, script string) error {
	t.Helper()
	err := exec.Command("/bin/sh", "-c", script).Run() //nolint:gosec // fixed test scripts produce real exit and signal statuses
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("expected command exit: %v", err)
	}
	return err
}

func checkMappedCommandResult(t *testing.T, output string, commandErr error, canceled bool, proved []bool) {
	t.Helper()
	original := mappedImageCommand
	t.Cleanup(func() { mappedImageCommand = original })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	mappedImageCommand = func(_ context.Context, args ...string) ([]byte, error) {
		calls++
		if !reflect.DeepEqual(args, []string{"-a", "-p", "11,22", "-d", "txt", "-F", "pfinD"}) {
			t.Fatalf("native invocation = %v", args)
		}
		if canceled {
			cancel()
		}
		return []byte(output), commandErr
	}
	results := processImages(ctx, []int{11, 22})
	if calls != 1 {
		t.Fatalf("native invocations=%d, want one", calls)
	}
	for i, result := range results {
		checkMappedCommandProof(t, result, []int{11, 22}[i], proved[i], commandErr, canceled)
	}
}

func checkMappedCommandProof(t *testing.T, result processImageResult, pid int, proved bool, commandErr error, canceled bool) {
	t.Helper()
	if proved {
		if result.err != nil || result.image.PID != pid {
			t.Fatalf("proved PID %d: %+v", pid, result)
		}
		return
	}
	if result.err == nil {
		t.Fatalf("unproved PID %d accepted: %+v", pid, result)
	}
	if commandErr != nil && !errors.Is(result.err, commandErr) {
		t.Fatalf("command error lost: %v", result.err)
	}
	if canceled && !errors.Is(result.err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", result.err)
	}
}

func TestDarwinMappedExecutableSingletonCallers(t *testing.T) {
	root := t.TempDir()
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var stat unix.Stat_t
	if err = unix.Stat(path, &stat); err != nil {
		t.Fatal(err)
	}
	records := fmt.Sprintf("ftxt\ni%d\nD%#x\nn%s\n", stat.Ino, darwinDevice(stat.Dev), path)
	one := mappedExitError(t, "exit 1")
	original := mappedImageCommand
	t.Cleanup(func() { mappedImageCommand = original })
	mappedImageCommand = func(_ context.Context, args ...string) ([]byte, error) {
		if !reflect.DeepEqual(args, []string{"-a", "-p", strconv.Itoa(os.Getpid()), "-d", "txt", "-F", "finD"}) {
			t.Fatalf("singleton native args=%v", args)
		}
		return []byte(records), one
	}
	if _, err = processImage(t.Context(), os.Getpid()); !errors.Is(err, one) || !strings.Contains(err.Error(), "inspect mapped process image") {
		t.Fatalf("singleton error=%v", err)
	}
	output := fmt.Sprintf("gui/501/dev.shaulavo.mesh-update-helper = {\n\tstate = running\n\tpid = %d\n}\n", os.Getpid())
	if err = writeProcessFixture(filepath.Join(root, "launchctl"), []byte("#!/bin/sh\nprintf '%s' '"+output+"'\n")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	probe := HelperProcessProbe(updateinstall.HelperConfig{Kind: "launchd", Domain: "gui/501"})
	if _, err = probe(t.Context(), updateinstall.HelperInstallation{Executable: path}); !errors.Is(err, one) || !strings.Contains(err.Error(), "inspect running helper image") {
		t.Fatalf("helper singleton error=%v", err)
	}
	plan := ManagedMigration{Definition: filepath.Join(root, "absent.plist"), Before: "absent"}
	plan.Settings.Service = updateinstall.ServiceSpec{Kind: "launchd", Domain: "gui/501", Name: "test"}
	running, err := VerifyManagedMigrationSource(t.Context(), plan, "")
	if !running || !errors.Is(err, one) {
		t.Fatalf("migration singleton: running=%v error=%v", running, err)
	}
}
