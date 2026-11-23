package worker

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testStandby is the daemon's end of a handoff whose standby side the test
// plays itself.
func testStandby(t *testing.T) (*Standby, *os.File) {
	t.Helper()
	ours, theirs, err := handoffPair()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ours.Close(); _ = theirs.Close() })
	return &Standby{ID: "7K3D", Dir: "/state/s/7K3D", handoff: ours, exited: make(chan struct{})}, theirs
}

func testLaunch(dir string) StandbyLaunch {
	return StandbyLaunch{Args: []string{"session-worker", "--id", "7K3D", "--dir", "/state/s/7K3D", "--", "/bin/true"}, Env: []string{"PATH=/bin"}, Cwd: dir}
}

func TestStandbyHandoffIsAcceptedOnlyByAcknowledgement(t *testing.T) {
	s, theirs := testStandby(t)
	done := make(chan error, 1)
	go func() {
		launch, err := AwaitStandbyLaunch(theirs, "7K3D", "/state/s/7K3D")
		if err == nil {
			err = launch.Accept()
		}
		done <- err
	}()
	accepted, err := s.give(testLaunch(t.TempDir()))
	if !accepted || err != nil {
		t.Fatalf("acknowledged handoff = %v, %v", accepted, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A standby that reads the launch and closes without acknowledging started
// nothing, so the launch may go to a fresh worker.
func TestStandbyClosingWithoutAcknowledgementStartsNothing(t *testing.T) {
	s, theirs := testStandby(t)
	go func() {
		_, _ = AwaitStandbyLaunch(theirs, "7K3D", "/state/s/7K3D")
		_ = theirs.Close()
	}()
	if accepted, err := s.give(testLaunch(t.TempDir())); accepted || err != nil {
		t.Fatalf("unacknowledged handoff = %v, %v; want not accepted and no error", accepted, err)
	}
}

// A standby that neither acknowledges nor closes may still be running the
// launch, so the outcome is unknown rather than not started.
func TestStandbySilentAfterReadingIsAnUnknownOutcome(t *testing.T) {
	previous := standbyAcceptTimeout
	standbyAcceptTimeout = 50 * time.Millisecond
	t.Cleanup(func() { standbyAcceptTimeout = previous })
	s, theirs := testStandby(t)
	read := make(chan struct{})
	go func() {
		_, _ = readStandbyLaunch(theirs, "7K3D", "/state/s/7K3D")
		close(read)
	}()
	accepted, err := s.give(testLaunch(t.TempDir()))
	<-read
	if !accepted || err == nil {
		t.Fatalf("silent standby = %v, %v; want accepted with an error", accepted, err)
	}
}

// Anything but an orderly close or the acknowledgement leaves the outcome
// unknown, so the launch is never started a second time.
func TestStandbyWrongReplyIsAnUnknownOutcome(t *testing.T) {
	s, theirs := testStandby(t)
	go func() {
		_, _ = readStandbyLaunch(theirs, "7K3D", "/state/s/7K3D")
		_, _ = theirs.Write([]byte{'?'})
	}()
	if accepted, err := s.give(testLaunch(t.TempDir())); !accepted || err == nil {
		t.Fatalf("wrong reply = %v, %v; want accepted with an error", accepted, err)
	}
}

// A daemon that goes away before reading the acknowledgement makes it fail,
// so the standby runs nothing.
func TestStandbyRunsNothingWhenItsDaemonLeavesBeforeTheAcknowledgement(t *testing.T) {
	s, theirs := testStandby(t)
	launch := testLaunch(t.TempDir())
	if _, err := s.handoff.Write(standbyFrame(t, launch)); err != nil {
		t.Fatal(err)
	}
	_ = s.handoff.Close()
	got, err := AwaitStandbyLaunch(theirs, "7K3D", "/state/s/7K3D")
	if err != nil {
		t.Fatal(err)
	}
	if got.Accept() == nil {
		t.Fatal("acknowledged a launch to a daemon that had gone")
	}
}

func standbyFrame(t *testing.T, launch StandbyLaunch) []byte {
	t.Helper()
	payload, err := json.Marshal(launch)
	if err != nil {
		t.Fatal(err)
	}
	return append(binary.BigEndian.AppendUint32(nil, uint32(len(payload))), payload...) //nolint:gosec // a small fixture
}

func TestStandbyRefusesALaunchForAnotherSession(t *testing.T) {
	for name, args := range map[string][]string{
		"other id":  {"session-worker", "--id", "9ZZZ", "--dir", "/state/s/9ZZZ", "--", "/bin/true"},
		"other dir": {"session-worker", "--id", "7K3D", "--dir", "/elsewhere/7K3D", "--", "/bin/true"},
		"nested":    {"session-worker", StandbyFlag, "--id", "7K3D", "--dir", "/state/s/7K3D", "--", "/bin/true"},
	} {
		s, theirs := testStandby(t)
		launch := testLaunch(t.TempDir())
		launch.Args = args
		go func() { _, _ = s.give(launch) }()
		if _, err := AwaitStandbyLaunch(theirs, "7K3D", "/state/s/7K3D"); err == nil || errors.Is(err, io.EOF) {
			t.Fatalf("%s: accepted %v", name, args)
		}
	}
}

func TestStandbyRunsOnlyWhatAFreshWorkerWould(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "mesh")
	if err := os.WriteFile(exe, []byte("binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	image, err := stampImage(exe)
	if err != nil {
		t.Fatal(err)
	}
	s := &Standby{executable: exe, image: image, runtimeEnv: runtimeVariables([]string{"GOGC=50", "PATH=/bin"}), exited: make(chan struct{})}
	launch := testLaunch(t.TempDir())
	launch.Env = []string{"GOGC=50", "PATH=/usr/bin", "MESH_SESSION=7K3D"}
	if !s.usableFor(exe, launch) {
		t.Fatal("a launch with the same runtime settings was refused")
	}
	for name, change := range map[string]func(*StandbyLaunch){
		"runtime variable": func(l *StandbyLaunch) { l.Env = []string{"GOGC=off", "PATH=/bin"} },
		"loader variable":  func(l *StandbyLaunch) { l.Env = append(l.Env, "LD_PRELOAD=/x.so") },
		"missing cwd":      func(l *StandbyLaunch) { l.Cwd = filepath.Join(l.Cwd, "gone") },
		"bare entry":       func(l *StandbyLaunch) { l.Env = append(l.Env, "NOEQUALS") },
		"non-UTF-8 env":    func(l *StandbyLaunch) { l.Env = append(l.Env, "RAW=\xff") },
		"non-UTF-8 arg":    func(l *StandbyLaunch) { l.Args = append(l.Args, "\xff") },
	} {
		changed := launch
		change(&changed)
		if s.usableFor(exe, changed) {
			t.Errorf("%s: standby accepted a launch a fresh worker would start differently", name)
		}
	}
	// A same-size rewrite that restores the modification time still differs.
	before, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := os.WriteFile(exe, []byte("BINARY"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(exe, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	if s.usableFor(exe, launch) {
		t.Error("standby of an executable rewritten with its old mtime was used")
	}
	close(s.exited)
	if s.usableFor(exe, launch) {
		t.Error("an exited standby was used")
	}
}
