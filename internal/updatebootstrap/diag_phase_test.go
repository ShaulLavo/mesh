package updatebootstrap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
)

// Diagnostic branch only, never merged. Everything here is supplemental
// evidence about where one observation spends its time. The unchanged
// observer stays the only gate.

type diagUsage struct {
	wall, cpu time.Duration
}

func (u diagUsage) String() string {
	return fmt.Sprintf("wall=%.1fms cpu=%.1fms", float64(u.wall)/1e6, float64(u.cpu)/1e6)
}

func (u *diagUsage) add(other diagUsage) {
	u.wall += other.wall
	u.cpu += other.cpu
}

type diagMark struct {
	at             time.Time
	self, children syscall.Rusage
}

func diagStart() diagMark {
	var mark diagMark
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &mark.self)
	_ = syscall.Getrusage(syscall.RUSAGE_CHILDREN, &mark.children)
	mark.at = time.Now()
	return mark
}

func (m diagMark) since() diagUsage {
	end := diagStart()
	cpu := diagCPU(end.self) - diagCPU(m.self) + diagCPU(end.children) - diagCPU(m.children)
	return diagUsage{wall: end.at.Sub(m.at), cpu: cpu}
}

func diagCPU(usage syscall.Rusage) time.Duration {
	user := time.Duration(usage.Utime.Sec)*time.Second + time.Duration(usage.Utime.Usec)*time.Microsecond
	system := time.Duration(usage.Stime.Sec)*time.Second + time.Duration(usage.Stime.Usec)*time.Microsecond
	return user + system
}

func diagMillis(d time.Duration) string {
	return strconv.FormatFloat(float64(d)/1e6, 'f', 1, 64)
}

// ---- positive controls ----

func TestDiagControls(t *testing.T) {
	if os.Getenv("MESH_DIAG_PHASES") == "" {
		t.Skip("supplemental hosted phase attribution; not native acceptance")
	}
	diagControls(t)
}

// Every instrument used by the capture must first detect a known cause in
// this same process. A failed control fails the test before any capture.
func diagControls(t *testing.T) {
	t.Helper()
	diagControlClock(t)
	diagControlLatency(t)
	diagControlSampler(t)
	diagControlSplit(t)
	diagControlCache(t)
}

func diagControlClock(t *testing.T) {
	mark := diagStart()
	time.Sleep(200 * time.Millisecond)
	slept := mark.since()
	if slept.wall < 195*time.Millisecond || slept.wall > 400*time.Millisecond || slept.cpu > 60*time.Millisecond {
		t.Errorf("DIAG CONTROL FAIL sleep-200ms %s", slept)
	}
	// Touch the pages first: under memory pressure a fresh allocation waits for
	// reclaim, which the first capture read as a failed control at 97 workers.
	buffer := make([]byte, 24<<20)
	for i := 0; i < len(buffer); i += 4096 {
		buffer[i] = 1
	}
	mark = diagStart()
	_ = sha256.Sum256(buffer)
	hashed := mark.since()
	if hashed.cpu < 3*time.Millisecond || hashed.cpu < hashed.wall/3 {
		t.Errorf("DIAG CONTROL FAIL hash-24MiB-memory %s", hashed)
	}
	t.Logf("DIAG CONTROL clock sleep-200ms[%s] hash-24MiB-memory[%s]", slept, hashed)
}

// The per-request latency instrument must see a responder that waits 150 ms
// and must not invent a wait for one that answers at once.
func diagControlLatency(t *testing.T) {
	slow := diagControlRequest(t, 150*time.Millisecond)
	fast := diagControlRequest(t, 0)
	if slow < 150*time.Millisecond || slow > 600*time.Millisecond || fast > 100*time.Millisecond {
		t.Errorf("DIAG CONTROL FAIL latency slow=%s fast=%s", slow, fast)
	}
	t.Logf("DIAG CONTROL latency delayed-150ms=%sms immediate=%sms", diagMillis(slow), diagMillis(fast))
}

func diagControlRequest(t *testing.T, delay time.Duration) time.Duration {
	dir, err := os.MkdirTemp("", "dg")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	listener, err := net.Listen("unix", filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	go diagControlResponder(listener, delay)
	conn, err := dial(t.Context(), filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	current := &workerObservation{conn: conn}
	current.meta.ID = "CTRL"
	var latencies diagLatencies
	latencies.timed(diagAskLogs)(current)
	if current.err != nil {
		t.Errorf("DIAG CONTROL FAIL latency request: %v", current.err)
	}
	return latencies.values[0]
}

func diagControlResponder(listener net.Listener, delay time.Duration) {
	conn, err := listener.Accept()
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	frame, err := protocol.NewReader(conn).ReadFrame()
	if err != nil {
		return
	}
	request, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return
	}
	time.Sleep(delay)
	_ = protocol.NewWriter(conn).WriteControlMsg(protocol.Control{Type: protocol.TypeLogged, SessionID: "CTRL", RequestID: request.RequestID})
}

// The child sampler must see a tool started by a watched parent, and must
// see nothing for a parent that starts no tool.
func diagControlSampler(t *testing.T) {
	stop := make(chan struct{})
	result := make(chan diagSample, 1)
	go func() { result <- diagSampleChildren(map[int]bool{os.Getpid(): true}, stop) }()
	child := exec.Command("/bin/sleep", "1")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	close(stop)
	seen := <-result
	_ = child.Process.Kill()
	_ = child.Wait()
	stop = make(chan struct{})
	go func() { result <- diagSampleChildren(map[int]bool{1 << 30: true}, stop) }()
	time.Sleep(150 * time.Millisecond)
	close(stop)
	empty := <-result
	if seen.tools["sleep"] != 1 || len(empty.tools) != 0 {
		t.Errorf("DIAG CONTROL FAIL child-sampler watched=%v unwatched=%v", seen.tools, empty.tools)
	}
	t.Logf("DIAG CONTROL child-sampler watched=%v samples=%d unwatched=%v samples=%d", seen.tools, seen.samples, empty.tools, empty.samples)
}

// The read and hash split must attribute a slow reader to the read side.
func diagControlSplit(t *testing.T) {
	var split diagSplit
	source := diagSlowReader{remaining: 8 << 20, delay: 5 * time.Millisecond}
	digest, size, err := diagCopySplit(t.Context(), &source, make([]byte, 1<<20), &split)
	read, hash := time.Duration(split.read.Load()), time.Duration(split.hash.Load())
	want := sha256.Sum256(make([]byte, 8<<20))
	if err != nil || size != 8<<20 || digest != hex.EncodeToString(want[:]) || read < 40*time.Millisecond || hash > read {
		t.Errorf("DIAG CONTROL FAIL read-hash-split read=%s hash=%s size=%d err=%v", read, hash, size, err)
	}
	t.Logf("DIAG CONTROL read-hash-split eight-5ms-reads read=%sms hash=%sms", diagMillis(read), diagMillis(hash))
}

type diagSlowReader struct {
	remaining int
	delay     time.Duration
}

func (r *diagSlowReader) Read(buffer []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	time.Sleep(r.delay)
	n := min(len(buffer), r.remaining)
	clear(buffer[:n])
	r.remaining -= n
	return n, nil
}

// ---- instruments ----

type diagLatencies struct {
	mu     sync.Mutex
	values []time.Duration
}

func (l *diagLatencies) timed(stage func(*workerObservation)) func(*workerObservation) {
	return func(current *workerObservation) {
		started := time.Now()
		stage(current)
		elapsed := time.Since(started)
		l.mu.Lock()
		l.values = append(l.values, elapsed)
		l.mu.Unlock()
	}
}

func (l *diagLatencies) String() string {
	if len(l.values) == 0 {
		return "n=0"
	}
	slices.Sort(l.values)
	var sum time.Duration
	for _, value := range l.values {
		sum += value
	}
	last := len(l.values) - 1
	return fmt.Sprintf("n=%d sum=%sms p50=%sms p90=%sms max=%sms", len(l.values), diagMillis(sum),
		diagMillis(l.values[last/2]), diagMillis(l.values[last*9/10]), diagMillis(l.values[last]))
}

type diagSample struct {
	tools   map[string]int
	samples int
}

// diagSampleChildren polls the process table until stop closes and counts the
// distinct processes whose parent is one of the watched processes, by name.
func diagSampleChildren(parents map[int]bool, stop <-chan struct{}) diagSample {
	seen := make(map[int]string)
	sample := diagSample{tools: make(map[string]int)}
	for {
		select {
		case <-stop:
			for _, name := range seen {
				sample.tools[name]++
			}
			return sample
		default:
		}
		output, err := exec.Command("ps", "-A", "-o", "pid=,ppid=,comm=").Output()
		if err != nil {
			continue
		}
		sample.samples++
		diagRecordChildren(string(output), parents, seen)
	}
}

func diagRecordChildren(output string, parents map[int]bool, seen map[int]string) {
	for line := range strings.SplitSeq(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		parent, parentErr := strconv.Atoi(fields[1])
		if pidErr != nil || parentErr != nil || !parents[parent] {
			continue
		}
		seen[pid] = filepath.Base(fields[2])
	}
}

type diagSplit struct {
	read, hash atomic.Int64
}

// diagCopySplit is imageDigestContext's loop with the read and the hash timed
// apart and the buffer size chosen by the caller.
func diagCopySplit(ctx context.Context, source io.Reader, buffer []byte, split *diagSplit) (string, int64, error) {
	hash := sha256.New()
	var total int64
	var read, hashed time.Duration
	defer func() {
		split.read.Add(int64(read))
		split.hash.Add(int64(hashed))
	}()
	for {
		if err := ctx.Err(); err != nil {
			return "", total, fmt.Errorf("split digest: %w", err)
		}
		started := time.Now()
		n, err := source.Read(buffer)
		middle := time.Now()
		_, _ = hash.Write(buffer[:n])
		read += middle.Sub(started)
		hashed += time.Since(middle)
		total += int64(n)
		if errors.Is(err, io.EOF) {
			return hex.EncodeToString(hash.Sum(nil)), total, nil
		}
		if err != nil {
			return "", total, fmt.Errorf("split digest read: %w", err)
		}
	}
}

// diagAskLogs is the candidate probe: the same held connection and the same
// checks as ask, with the request every worker answers from memory.
func diagAskLogs(current *workerObservation) {
	defer func() { _ = current.conn.Close() }()
	meta := current.meta
	response, err := exchange(current.conn, protocol.Control{Type: protocol.TypeLogs, RequestID: "bootstrap-worker", Tail: 1})
	if err != nil {
		current.err = fmt.Errorf("verify session %s workerPID=%d: %w", meta.ID, current.image.PID, err)
		return
	}
	if response.Type == protocol.TypeLogged && response.SessionID == meta.ID && response.RequestID == "bootstrap-worker" {
		return
	}
	if meta.Build == nil && response.Type == protocol.TypeError && response.SessionID == meta.ID && response.Message == "expected "+protocol.TypeAttach {
		current.err = errDiagLegacyReply
		return
	}
	current.err = fmt.Errorf("session %s logs reply type=%q session=%q request=%q message=%q", meta.ID, response.Type, response.SessionID, response.RequestID, response.Message)
}

var errDiagLegacyReply = errors.New("legacy attach-only reply")

// ---- capture ----

type diagRoundPlan struct {
	label  string
	ask    string // inspect or logs
	digest string // product, split32k or split1m
	sample bool
}

type diagCapture struct {
	rounds       int
	t            *testing.T
	ctx          context.Context
	state        string
	stateVersion int
	digests      map[string]string
}

func TestDiagCohortPhases(t *testing.T) {
	state := os.Getenv("MESH_COHORT_STATE")
	if state == "" || os.Getenv("MESH_DIAG_PHASES") == "" {
		t.Skip("supplemental hosted phase attribution; not native acceptance")
	}
	diagCohortPhases(t, state)
}

// TestDiagSmoke runs the same capture against a small isolated cohort of one
// executable, to prove the capture itself before a hosted run.
func TestDiagSmoke(t *testing.T) {
	binary, count := os.Getenv("MESH_LEGACY_BINARY"), os.Getenv("MESH_DIAG_SMOKE")
	if binary == "" || count == "" {
		t.Skip("local capture smoke; not native acceptance")
	}
	workers, err := strconv.Atoi(count)
	if err != nil {
		t.Fatal(err)
	}
	state, _ := startLegacyFixture(t, binary)
	for index := range workers {
		diagSmokeSession(t, state, index)
	}
	diagCohortPhases(t, state)
}

func diagSmokeSession(t *testing.T, state string, index int) {
	conn, err := dial(t.Context(), filepath.Join(state, "daemon.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	request := protocol.Control{Type: protocol.TypeCreate, RequestID: "diag-smoke-" + strconv.Itoa(index), Command: []string{"/bin/sh"}, Cols: 80, Rows: 24}
	response, err := exchange(conn, request)
	if err != nil || response.Type != protocol.TypeCreated || response.SessionID == "" {
		t.Fatalf("DIAG smoke create %d: %+v: %v", index, response, err)
	}
}

func diagCohortPhases(t *testing.T, state string) {
	ctx, cancel := context.WithTimeout(t.Context(), 170*time.Second)
	defer cancel()
	// The job proved every control on this runner before the cohort started.
	// Under the cohort's memory pressure the controls are reported and the
	// capture continues, so a loaded runner still yields its measurements.
	t.Run("controls-under-load", diagControls)
	capture := &diagCapture{t: t, ctx: ctx, state: state, digests: make(map[string]string)}
	diagSystem(ctx, t, "before")
	capture.stateVersion = diagDaemon(ctx, t, state)
	// Inspect is the product path of this build. It runs first, as found.
	diagInspect(t, state, "inspect-1")
	diagInspect(t, state, "inspect-2")
	var paths []string
	for _, digest := range []string{"product", "direct4m", "direct4m", "direct4m", "product", "product", "direct1m", "direct1m", "split1m", "split1m", "direct4m", "direct4m", "product"} {
		capture.rounds++
		label := "r" + strconv.Itoa(capture.rounds)
		paths = capture.round(diagRoundPlan{label: label, ask: "logs", digest: digest})
	}
	diagPairedReads(ctx, t, paths)
	diagInspect(t, state, "inspect-3")
	diagInspect(t, state, "inspect-4")
	diagSystem(ctx, t, "after")
}

func diagSystem(ctx context.Context, t *testing.T, label string) {
	t.Logf("DIAG system %s cpus=%d gomaxprocs=%d", label, runtime.NumCPU(), runtime.GOMAXPROCS(0))
	if runtime.GOOS != "darwin" {
		return
	}
	output, err := exec.CommandContext(ctx, "sysctl", "-n", "hw.memsize", "hw.ncpu", "vm.swapusage", "kern.memorystatus_vm_pressure_level").CombinedOutput()
	t.Logf("DIAG system %s sysctl err=%v\n%s", label, err, output)
	output, err = exec.CommandContext(ctx, "vm_stat").CombinedOutput()
	t.Logf("DIAG system %s vm_stat err=%v\n%s", label, err, output)
	output, err = exec.CommandContext(ctx, "ps", "-A", "-o", "pid=").CombinedOutput()
	t.Logf("DIAG system %s processes=%d err=%v", label, strings.Count(string(output), "\n"), err)
}

func diagDaemon(ctx context.Context, t *testing.T, state string) int {
	mark := diagStart()
	conn, err := dial(ctx, filepath.Join(state, "daemon.sock"))
	if err != nil {
		t.Fatalf("DIAG daemon dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	image, err := peerImage(ctx, conn)
	t.Logf("DIAG daemon dial+peer-image %s err=%v", mark.since(), err)
	mark = diagStart()
	stateVersion, err := readStateVersion(ctx, state)
	t.Logf("DIAG daemon state-version %s err=%v", mark.since(), err)
	mark = diagStart()
	_, err = imageDigestContext(ctx, image.Path)
	t.Logf("DIAG daemon digest %s err=%v", mark.since(), err)
	return stateVersion
}

func (c *diagCapture) round(plan diagRoundPlan) []string {
	root := filepath.Join(c.state, "s")
	entries, err := os.ReadDir(root)
	if err != nil {
		c.t.Fatalf("DIAG round %s: %v", plan.label, err)
	}
	var observations []workerObservation
	for _, entry := range entries {
		if entry.IsDir() {
			observations = append(observations, workerObservation{dir: filepath.Join(root, entry.Name())})
		}
	}
	run := &diagRound{capture: c, plan: plan}
	whole := diagStart()
	for start := 0; start < len(observations); start += workerImageBatch {
		run.group(observations[start:min(start+workerImageBatch, len(observations))])
	}
	total := whole.since()
	run.report(observations, total)
	var paths []string
	for _, current := range observations {
		if current.image.Path != "" {
			paths = append(paths, current.image.Path)
		}
	}
	return paths
}

type diagRound struct {
	capture   *diagCapture
	plan      diagRoundPlan
	stages    [4]diagUsage
	latencies diagLatencies
	split     diagSplit
	sampled   diagSample
	mismatch  atomic.Int32
	legacy    atomic.Int32
}

func (r *diagRound) group(group []workerObservation) {
	ctx := r.capture.ctx
	defer closeWorkerConnections(group)
	mark := diagStart()
	runWorkerStage(ctx, group, func(current *workerObservation) { current.connect(ctx) })
	r.stages[0].add(mark.since())
	mark = diagStart()
	identifyWorkers(ctx, group)
	r.stages[1].add(mark.since())
	stop, sampled := r.startSampler(group)
	mark = diagStart()
	runWorkerStage(ctx, group, r.latencies.timed(r.askStage()))
	r.stages[2].add(mark.since())
	r.stopSampler(stop, sampled)
	mark = diagStart()
	runWorkerStage(ctx, group, r.digestStage)
	r.stages[3].add(mark.since())
}

func (r *diagRound) startSampler(group []workerObservation) (chan struct{}, chan diagSample) {
	if !r.plan.sample {
		return nil, nil
	}
	parents := make(map[int]bool)
	for i := range group {
		if group[i].pid != 0 {
			parents[group[i].pid] = true
		}
	}
	stop, sampled := make(chan struct{}), make(chan diagSample, 1)
	go func() { sampled <- diagSampleChildren(parents, stop) }()
	// One table read must finish before the first request is sent.
	time.Sleep(100 * time.Millisecond)
	return stop, sampled
}

func (r *diagRound) stopSampler(stop chan struct{}, sampled chan diagSample) {
	if stop == nil {
		return
	}
	close(stop)
	result := <-sampled
	if r.sampled.tools == nil {
		r.sampled.tools = make(map[string]int)
	}
	r.sampled.samples += result.samples
	for name, count := range result.tools {
		r.sampled.tools[name] += count
	}
}

func (r *diagRound) askStage() func(*workerObservation) {
	if r.plan.ask == "inspect" {
		return func(current *workerObservation) { current.ask() }
	}
	return func(current *workerObservation) {
		diagAskLogs(current)
		if errors.Is(current.err, errDiagLegacyReply) {
			current.err = nil
			r.legacy.Add(1)
		}
	}
}

func (r *diagRound) digestStage(current *workerObservation) {
	ctx := r.capture.ctx
	switch r.plan.digest {
	case "none":
		return
	case "product":
		current.digest(ctx, r.capture.stateVersion)
		if current.worker != nil && current.worker.Build != nil {
			r.capture.remember(current.image.Path, current.worker.Build.Digest)
		}
		return
	}
	digest, err := diagDigestVariant(ctx, current.image.Path, r.plan.digest, &r.split)
	if err != nil {
		current.err = err
		return
	}
	if r.capture.recorded(current.image.Path) != digest {
		r.mismatch.Add(1)
	}
}

// diagDigestVariant hashes every byte of one image with the named reader.
// The direct readers ask the kernel to leave the file cache alone.
func diagDigestVariant(ctx context.Context, path, variant string, split *diagSplit) (string, error) {
	size := 32 << 10
	if strings.HasSuffix(variant, "1m") {
		size = 1 << 20
	}
	if strings.HasSuffix(variant, "4m") {
		size = 4 << 20
	}
	file, err := os.Open(path) //nolint:gosec // OS-reported executable of an observed local worker, or this test's own control file
	if err != nil {
		return "", fmt.Errorf("open image: %w", err)
	}
	defer func() { _ = file.Close() }()
	buffer := make([]byte, size)
	if strings.HasPrefix(variant, "direct") {
		direct, release, err := diagDirectBuffer(file, size)
		if err != nil {
			return "", err
		}
		defer release()
		buffer = direct
	}
	digest, _, err := diagCopySplit(ctx, file, buffer, split)
	return digest, err
}

var diagDigestLock sync.Mutex

func (c *diagCapture) remember(path, digest string) {
	diagDigestLock.Lock()
	defer diagDigestLock.Unlock()
	c.digests[path] = digest
}

func (c *diagCapture) recorded(path string) string {
	diagDigestLock.Lock()
	defer diagDigestLock.Unlock()
	return c.digests[path]
}

func (r *diagRound) report(observations []workerObservation, total diagUsage) {
	t := r.capture.t
	failed, connected, observed := 0, 0, 0
	for _, current := range observations {
		if current.err != nil {
			failed++
			t.Logf("DIAG round %s error %v", r.plan.label, current.err)
		}
		if current.pid != 0 {
			connected++
		}
		if current.worker != nil {
			observed++
		}
	}
	t.Logf("DIAG round %s ask=%s digest=%s entries=%d connected=%d observed=%d failed=%d legacyReplies=%d total[%s] connect[%s] identify[%s] ask[%s] digest[%s] askLatency[%s] digestSplit[read=%sms hash=%sms mismatches=%d] sampledChildren=%v samples=%d",
		r.plan.label, r.plan.ask, r.plan.digest, len(observations), connected, observed, failed, r.legacy.Load(), total,
		r.stages[0], r.stages[1], r.stages[2], r.stages[3], r.latencies.String(),
		diagMillis(time.Duration(r.split.read.Load())), diagMillis(time.Duration(r.split.hash.Load())), r.mismatch.Load(),
		r.sampled.tools, r.sampled.samples)
}

// diagPairedReads reads every image twice in a row, one file at a time. The
// second read is of bytes that were just read, so a first read that is much
// slower than its repeat was waiting for the disk.
func diagPairedReads(ctx context.Context, t *testing.T, paths []string) {
	var first, second []time.Duration
	var firstTotal, secondTotal time.Duration
	var bytes int64
	slow := 0
	buffer := make([]byte, 1<<20)
	for _, path := range paths {
		if ctx.Err() != nil {
			break
		}
		one, size := diagReadOnce(path, buffer)
		two, _ := diagReadOnce(path, buffer)
		bytes += size
		first, second = append(first, one), append(second, two)
		firstTotal, secondTotal = firstTotal+one, secondTotal+two
		if one > 3*two && one > 20*time.Millisecond {
			slow++
		}
	}
	if len(first) == 0 {
		t.Logf("DIAG paired-reads files=0")
		return
	}
	slices.Sort(first)
	slices.Sort(second)
	last := len(first) - 1
	t.Logf("DIAG paired-reads files=%d bytes=%d first[total=%sms p50=%sms max=%sms] repeat[total=%sms p50=%sms max=%sms] firstMuchSlower=%d",
		len(first), bytes, diagMillis(firstTotal), diagMillis(first[last/2]), diagMillis(first[last]),
		diagMillis(secondTotal), diagMillis(second[last/2]), diagMillis(second[last]), slow)
}

func diagReadOnce(path string, buffer []byte) (time.Duration, int64) {
	started := time.Now()
	file, err := os.Open(path) //nolint:gosec // OS-reported executable of an observed local worker, or this test's own control file
	if err != nil {
		return 0, 0
	}
	defer func() { _ = file.Close() }()
	var total int64
	for {
		n, err := file.Read(buffer)
		total += int64(n)
		if err != nil {
			return time.Since(started), total
		}
	}
}

func diagInspect(t *testing.T, state, label string) {
	mark := diagStart()
	observed, err := Inspect(context.Background(), state)
	t.Logf("DIAG %s workers=%d %s err=%v", label, len(observed.Health.Workers), mark.since(), err)
}
