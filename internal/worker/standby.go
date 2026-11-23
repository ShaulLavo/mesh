package worker

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/session"
)

// A standby is a session worker started before any job needs it. Starting a
// 60 MB Go binary and running its initialization took 30 to 90 ms of every
// keyed launch; a standby has already paid that and waits on a socket for
// exactly one launch: the argument list a fresh worker would have been
// started with, its environment and its working directory. It then runs the
// same worker command a fresh process runs, so ownership is unchanged: it was
// started in its own session, owns its child, and outlives the daemon.
//
// The handoff is acknowledged. The standby takes on the launch's directory,
// environment and log, then acknowledges, and only then runs anything. So a
// standby that closes its end without acknowledging never started anything,
// and the daemon starts a fresh worker instead; only a launch whose
// acknowledgement neither arrives nor is ruled out has an unknown outcome.
//
// The session ID in its arguments is only a name chosen in advance, so that
// ancestor lookups see the usual `--id` and `--dir` once it serves a session.
// Nothing is allocated until a job reserves that ID. A standby whose socket
// closes before a launch arrives exits; it owns nothing.

// standbyHandoffLimit bounds one launch description.
const standbyHandoffLimit = 1 << 20

// standbyAcceptTimeout bounds the wait for a standby's acknowledgement. A
// waiting standby answers in microseconds; this only catches one that hangs.
var standbyAcceptTimeout = 5 * time.Second

// standbyAccepted is the byte a standby sends once it takes a launch, and
// standbyPublished the one it sends when its worker is ready.
const (
	standbyAccepted  = 'A'
	standbyPublished = 'R'
)

// StandbyFlag marks a worker started ahead of its launch.
const StandbyFlag = "--standby"

// Standby is the daemon's handle on one waiting worker.
type Standby struct {
	ID, Dir    string
	process    *os.Process
	handoff    *net.UnixConn
	executable string
	image      imageStamp
	runtimeEnv map[string]string
	exited     chan struct{}
}

// StandbyLaunch is the launch a standby receives.
type StandbyLaunch struct {
	Args []string `json:"args"`
	Env  []string `json:"env"`
	Cwd  string   `json:"cwd"`

	handoff io.WriteCloser
}

// StartStandby starts one waiting worker from executable with env, the
// environment fresh workers inherit their runtime settings from.
func StartStandby(sessionsDir, executable string, env []string) (*Standby, error) {
	id, err := session.NewID()
	if err != nil {
		return nil, err //nolint:wrapcheck // session names the failure
	}
	image, err := stampImage(executable)
	if err != nil {
		return nil, err
	}
	ours, theirs, err := handoffPair()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(sessionsDir, id)
	cmd := exec.Command(executable, "session-worker", StandbyFlag, "--id", id, "--dir", dir) //nolint:gosec // the running Mesh binary
	cmd.Env = append([]string(nil), env...)
	cmd.Dir = "/"
	cmd.ExtraFiles = []*os.File{theirs}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = cmd.Start()
	_ = theirs.Close()
	if err != nil {
		_ = ours.Close()
		return nil, fmt.Errorf("standby worker: start: %w", err)
	}
	s := &Standby{ID: id, Dir: dir, process: cmd.Process, handoff: ours, executable: executable, image: image, runtimeEnv: runtimeVariables(env), exited: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(s.exited) }()
	return s, nil
}

// handoffPair makes the connected sockets of one handoff, close-on-exec so
// no other child the daemon starts meanwhile inherits either end.
func handoffPair() (*net.UnixConn, *os.File, error) {
	syscall.ForkLock.RLock()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err == nil {
		unix.CloseOnExec(fds[0])
		unix.CloseOnExec(fds[1])
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return nil, nil, fmt.Errorf("standby worker: handoff sockets: %w", err)
	}
	ours := os.NewFile(uintptr(fds[0]), "standby-handoff")
	conn, err := net.FileConn(ours)
	_ = ours.Close()
	theirs := os.NewFile(uintptr(fds[1]), "standby-handoff")
	if err != nil {
		_ = theirs.Close()
		return nil, nil, fmt.Errorf("standby worker: handoff socket: %w", err)
	}
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		_ = conn.Close()
		_ = theirs.Close()
		return nil, nil, errors.New("standby worker: handoff is not a Unix socket")
	}
	return unixConn, theirs, nil
}

// Discard tells an unused standby to exit. Closing its socket is enough: it
// exits as soon as it sees the end without a launch.
func (s *Standby) Discard() { _ = s.handoff.Close() }

// Exited closes when the standby process has been reaped.
func (s *Standby) Exited() <-chan struct{} { return s.exited }

// usableFor reports whether this standby can run launch exactly as a freshly
// started worker would: the same, unchanged executable, an environment it can
// reproduce entry for entry, and an existing directory to enter. Go and the
// dynamic loader read some variables only at process start, so a job that
// changes them gets a fresh process.
func (s *Standby) usableFor(executable string, launch StandbyLaunch) bool {
	select {
	case <-s.exited:
		return false
	default:
	}
	if executable != s.executable {
		return false
	}
	if current, err := stampImage(executable); err != nil || current != s.image {
		return false
	}
	if info, err := os.Stat(launch.Cwd); err != nil || !info.IsDir() {
		return false
	}
	// The handoff is JSON, which would replace bytes that are not UTF-8.
	if !utf8.ValidString(launch.Cwd) || !allUTF8(launch.Args) || !allUTF8(launch.Env) {
		return false
	}
	for _, entry := range launch.Env {
		if name, _, ok := strings.Cut(entry, "="); !ok || name == "" {
			return false
		}
	}
	return sameVariables(runtimeVariables(launch.Env), s.runtimeEnv)
}

func allUTF8(values []string) bool {
	for _, value := range values {
		if !utf8.ValidString(value) {
			return false
		}
	}
	return true
}

// imageStamp identifies an executable's file and contents cheaply. The change
// time moves on any rewrite and cannot be set by a process, so a same-size
// rewrite that restores the modification time still changes the stamp.
type imageStamp struct {
	dev, ino     uint64
	size         int64
	mtime, ctime unix.Timespec
}

func stampImage(path string) (imageStamp, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return imageStamp{}, fmt.Errorf("standby worker: inspect executable: %w", err)
	}
	return imageStamp{dev: uint64(st.Dev), ino: st.Ino, size: st.Size, mtime: st.Mtim, ctime: st.Ctim}, nil //nolint:unconvert,gosec // Dev's type differs by platform
}

// give hands launch to the standby. accepted reports that the standby
// acknowledged it and is running it. A standby that closed its end without
// acknowledging never started anything, which reports accepted false and no
// error, so the caller may start a fresh worker. An acknowledgement that
// neither arrives nor is ruled out is an error with accepted true: the
// standby may be running the launch.
func (s *Standby) give(launch StandbyLaunch) (accepted bool, err error) {
	payload, err := json.Marshal(launch)
	if err != nil {
		return false, fmt.Errorf("standby worker: encode launch: %w", err)
	}
	if len(payload) > standbyHandoffLimit {
		return false, fmt.Errorf("standby worker: launch of %d bytes exceeds the handoff limit", len(payload))
	}
	frame := binary.BigEndian.AppendUint32(make([]byte, 0, 4+len(payload)), uint32(len(payload))) //nolint:gosec // bounded above
	frame = append(frame, payload...)
	_ = s.handoff.SetDeadline(time.Now().Add(standbyAcceptTimeout))
	if n, err := s.handoff.Write(frame); n < len(frame) {
		// Without the whole launch it cannot acknowledge, so it starts nothing.
		return false, nil
	} else if err != nil {
		return true, fmt.Errorf("standby worker: hand off launch: %w", err)
	}
	var ack [1]byte
	n, err := s.handoff.Read(ack[:])
	switch {
	case n == 1 && ack[0] == standbyAccepted:
		return true, nil
	case n == 0 && errors.Is(err, io.EOF):
		// An orderly close before acknowledging: it started nothing.
		return false, nil
	default:
		// A timeout, a reset or a wrong byte proves nothing either way.
		return true, fmt.Errorf("standby worker: acknowledgement: %w", errors.Join(err, fmt.Errorf("reply %q", ack[:n])))
	}
}

// standbyHandoffFD is where StartStandby passes the standby's socket.
const standbyHandoffFD = 3

// StandbyHandoff takes the handoff socket this standby was started with and
// marks it close-on-exec, so no process the standby starts, the job
// included, inherits it. Call it before anything that can start a process.
func StandbyHandoff() *os.File {
	unix.CloseOnExec(standbyHandoffFD)
	return os.NewFile(standbyHandoffFD, "standby-handoff")
}

// WarmStandby does start-up work a fresh worker would do on its critical
// path. Only Linux hands workers the daemon's digest of their executable;
// elsewhere every worker hashes its own pinned image, so a standby does that
// while it waits.
func WarmStandby() {
	if runtime.GOOS != "linux" {
		go release.Current()
	}
}

// AwaitStandbyLaunch reads the one launch given to the standby with this ID
// and directory. A socket that ends before a complete launch returns io.EOF.
// The caller applies the launch and then accepts it; it runs nothing before
// Accept succeeds.
func AwaitStandbyLaunch(handoff io.ReadWriteCloser, id, dir string) (StandbyLaunch, error) {
	launch, err := readStandbyLaunch(handoff, id, dir)
	if err != nil {
		_ = handoff.Close()
		return StandbyLaunch{}, err
	}
	launch.handoff = handoff
	return launch, nil
}

func readStandbyLaunch(handoff io.Reader, id, dir string) (StandbyLaunch, error) {
	var size [4]byte
	if _, err := io.ReadFull(handoff, size[:]); err != nil {
		return StandbyLaunch{}, io.EOF
	}
	length := binary.BigEndian.Uint32(size[:])
	if length > standbyHandoffLimit {
		return StandbyLaunch{}, fmt.Errorf("standby worker: launch of %d bytes exceeds the handoff limit", length)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(handoff, payload); err != nil {
		return StandbyLaunch{}, io.EOF
	}
	var launch StandbyLaunch
	if err := json.Unmarshal(payload, &launch); err != nil {
		return StandbyLaunch{}, fmt.Errorf("standby worker: decode launch: %w", err)
	}
	location, ok := sessionWorkerLocationFromArgs(append([]string{""}, launch.Args...))
	if !ok || location.SessionID != id || location.Dir != dir || launch.Cwd == "" || slices.Contains(launch.Args, StandbyFlag) {
		return StandbyLaunch{}, fmt.Errorf("standby worker: launch is for another session")
	}
	return launch, nil
}

// Accept acknowledges the launch. Only after it succeeds may the standby run
// anything; if it fails the daemon starts a fresh worker instead. Once it is
// accepted, the worker tells the daemon when it has published itself, which
// saves the daemon's readiness polling.
func (l StandbyLaunch) Accept() error {
	if _, err := l.handoff.Write([]byte{standbyAccepted}); err != nil {
		return errors.Join(err, l.handoff.Close())
	}
	var once sync.Once
	onPublished = func() {
		once.Do(func() {
			_, _ = l.handoff.Write([]byte{standbyPublished})
			_ = l.handoff.Close()
		})
	}
	return nil
}

// onPublished runs once a worker's metadata and socket are ready. Only a
// standby sets it: its daemon is waiting on the handoff socket.
var onPublished = func() {}

func notifyPublished() { onPublished() }

// published closes when the accepted standby reports its publication or its
// handoff socket ends; the caller then checks readiness at once instead of at
// its next poll.
func (s *Standby) published() <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.handoff.SetReadDeadline(time.Time{})
		var notice [1]byte
		_, _ = s.handoff.Read(notice[:])
	}()
	return done
}

// Apply gives this process what a fresh worker would have started with: the
// working directory, the environment, and the session log on stdout and
// stderr. It runs before Accept, so a failure here leaves the launch to a
// fresh worker.
func (l StandbyLaunch) Apply(dir string) error {
	if err := os.Chdir(l.Cwd); err != nil {
		return fmt.Errorf("standby worker: enter %s: %w", l.Cwd, err)
	}
	os.Clearenv()
	for _, entry := range l.Env {
		name, value, _ := strings.Cut(entry, "=")
		if err := os.Setenv(name, value); err != nil {
			return fmt.Errorf("standby worker: set environment: %w", err)
		}
	}
	log, err := os.OpenFile(filepath.Join(dir, "worker.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // the fixed log of this worker's reserved session
	if err != nil {
		return fmt.Errorf("standby worker: open log: %w", err)
	}
	defer func() { _ = log.Close() }()
	return redirectOutput(log)
}

func redirectOutput(log *os.File) error {
	for _, fd := range []int{1, 2} {
		if err := unix.Dup2(int(log.Fd()), fd); err != nil {
			return fmt.Errorf("standby worker: redirect output to log: %w", err)
		}
	}
	return nil
}

// runtimeVariables are the environment variables a process reads only when
// it starts: Go's runtime settings and the dynamic loader's.
func runtimeVariables(env []string) map[string]string {
	out := make(map[string]string)
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "GO") || strings.HasPrefix(name, "LD_") || strings.HasPrefix(name, "DYLD_") {
			out[name] = value
		}
	}
	return out
}

func sameVariables(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for name, value := range a {
		if other, ok := b[name]; !ok || other != value {
			return false
		}
	}
	return true
}
