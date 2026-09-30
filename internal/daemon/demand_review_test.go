package daemon

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	meshserve "github.com/shaul/mesh/internal/serve"
)

type demandLogSink struct {
	mu      sync.Mutex
	buffer  bytes.Buffer
	blocked chan struct{}
	resume  <-chan struct{}
	once    sync.Once
}

func (s *demandLogSink) Write(data []byte) (int, error) {
	if s.blocked != nil {
		s.once.Do(func() {
			close(s.blocked)
			<-s.resume
		})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	count, _ := s.buffer.Write(data)
	return count, nil
}

func (s *demandLogSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buffer.String()
}

func TestDemandFailureBypassesFullReporterQueue(t *testing.T) {
	t.Parallel()
	resume := make(chan struct{})
	var release sync.Once
	unblock := func() { release.Do(func() { close(resume) }) }
	sink := &demandLogSink{blocked: make(chan struct{}), resume: resume}
	logger := log.New(sink, "", 0)
	reporter := newErrorReporter(func(err error) { logger.Print(err) })
	t.Cleanup(func() {
		unblock()
		reporter.shutdown()
		<-reporter.stop
	})
	reporter.report(errors.New("blocked best-effort diagnostic"))
	<-sink.blocked
	for range cap(reporter.queue) {
		reporter.report(errors.New("queued best-effort diagnostic"))
	}
	if len(reporter.queue) != cap(reporter.queue) {
		t.Fatal("reporter queue was not full")
	}

	sessions := newFakeDemandSessions()
	code := 7
	sessions.exitNow = &code
	manager := testDemandManager(t, sessions, func() bool { return false })
	manager.report = reporter.report
	manager.Sync([]meshserve.Service{demandService(time.Minute)})
	result := make(chan error, 1)
	go func() {
		_, err := manager.Enter(context.Background(), "dev")
		result <- err
	}()
	waitForDemand(t, manager, protocol.DemandFailed)
	var failure error
	select {
	case failure = <-result:
		t.Error("waiter received a reference before the blocked sink recorded its diagnostic")
	case <-time.After(30 * time.Millisecond):
	}
	unblock()
	if failure == nil {
		select {
		case failure = <-result:
		case <-time.After(2 * time.Second):
			t.Fatal("failed start did not finish after the sink resumed")
		}
	}
	if failure == nil {
		t.Fatal("failed start returned no error")
	}
	response := httptest.NewRecorder()
	meshserve.WriteDemandFailure(response, failure)
	reference, _, _ := strings.Cut(strings.TrimPrefix(response.Body.String(), "on-demand service unavailable; reference "), "\n")
	if !strings.Contains(sink.String(), "on-demand failure "+reference+": ") {
		t.Errorf("full reporter queue lost the referenced diagnostic: %q", sink.String())
	}
}

func TestDemandAdmissionErrorsHaveCorrelatedLog(t *testing.T) {
	t.Parallel()
	for _, removed := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancellation", true: "removed route"}[removed], func(t *testing.T) {
			manager := testDemandManager(t, newFakeDemandSessions(), func() bool { return false })
			sink := &demandLogSink{}
			logger := log.New(sink, "", 0)
			manager.report = func(err error) { logger.Print(err) }
			service := demandService(time.Minute)
			manager.Sync([]meshserve.Service{service})
			registry, err := meshserve.NewRegistry([]meshserve.Service{service})
			if err != nil {
				t.Fatal(err)
			}
			registry.SetDemandGate(manager)
			request := httptest.NewRequest(http.MethodGet, "/dev/", nil)
			wantStatus := http.StatusServiceUnavailable
			if removed {
				manager.Sync(nil)
				wantStatus = http.StatusBadGateway
			} else {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				request = request.WithContext(ctx)
			}
			response := httptest.NewRecorder()
			registry.ServeHTTP(response, request)
			if response.Code != wantStatus {
				t.Fatalf("admission failure answered %d, want %d", response.Code, wantStatus)
			}
			reference, _, _ := strings.Cut(strings.TrimPrefix(response.Body.String(), "on-demand service unavailable; reference "), "\n")
			if !strings.Contains(sink.String(), "on-demand failure "+reference+": ") {
				t.Errorf("admission failure returned an unrecorded reference: %q", sink.String())
			}
			if strings.Contains(response.Body.String(), "/dev") || strings.Contains(response.Body.String(), "context canceled") {
				t.Errorf("HTTP admission failure leaked its diagnostic: %q", response.Body.String())
			}
		})
	}
}
