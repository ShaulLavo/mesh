package daemon

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	meshserve "github.com/shaul/mesh/internal/serve"
)

type demandWaitContext struct {
	context.Context
	waiting chan<- struct{}
}

func (c demandWaitContext) Done() <-chan struct{} {
	c.waiting <- struct{}{}
	return c.Context.Done()
}

func TestDemandFailedStartLogsOnceForWaitingRequests(t *testing.T) {
	t.Parallel()
	sessions := newFakeDemandSessions()
	manager := testDemandManager(t, sessions, func() bool { return false })
	var ownerLog bytes.Buffer
	logger := log.New(&ownerLog, "", 0)
	manager.report = func(err error) { logger.Print(err) }

	allowFailure := make(chan struct{})
	var unblock sync.Once
	releaseFailure := func() { unblock.Do(func() { close(allowFailure) }) }
	t.Cleanup(releaseFailure)
	manager.dial = func(context.Context, string) error {
		<-allowFailure
		sessions.crash("S001", 7)
		return syscall.ECONNREFUSED
	}
	service := demandService(time.Minute)
	manager.Sync([]meshserve.Service{service})
	registry, err := meshserve.NewRegistry([]meshserve.Service{service})
	if err != nil {
		t.Fatal(err)
	}
	registry.SetDemandGate(manager)
	waiting := make(chan struct{}, 2)
	responses := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		go func() {
			request := httptest.NewRequest(http.MethodGet, "/dev/", nil)
			request = request.WithContext(demandWaitContext{Context: context.Background(), waiting: waiting})
			response := httptest.NewRecorder()
			registry.ServeHTTP(response, request)
			responses <- response
		}()
	}
	for range 2 {
		select {
		case <-waiting:
		case <-time.After(2 * time.Second):
			t.Fatal("request did not wait for the shared start")
		}
	}
	releaseFailure()
	var previousBody string
	for range 2 {
		select {
		case response := <-responses:
			body := response.Body.String()
			if response.Code != http.StatusBadGateway || strings.Contains(body, "S001") || strings.Contains(body, "npm ERR") {
				t.Errorf("failed start answered %d %q", response.Code, body)
			}
			if previousBody != "" && body != previousBody {
				t.Error("waiting requests did not share a failure reference")
			}
			previousBody = body
		case <-time.After(2 * time.Second):
			t.Fatal("request did not receive the failed start")
		}
	}
	if started, _ := sessions.counts(); started != 1 {
		t.Errorf("two waiting requests started %d sessions, want 1", started)
	}
	if count := strings.Count(ownerLog.String(), "daemon: on-demand failure "); count != 1 {
		t.Errorf("one failed start logged %d diagnostics, want 1: %q", count, ownerLog.String())
	}
	reference, _, _ := strings.Cut(strings.TrimPrefix(previousBody, "on-demand service unavailable; reference "), "\n")
	if !strings.Contains(ownerLog.String(), "on-demand failure "+reference+": ") || !strings.Contains(ownerLog.String(), "npm ERR! boom") {
		t.Errorf("owner log lost the correlated diagnostic: %q", ownerLog.String())
	}
}

func TestDemandFailedStartLogsWithoutHTTPWaiters(t *testing.T) {
	t.Parallel()
	sessions := newFakeDemandSessions()
	code := 7
	sessions.exitNow = &code
	manager := testDemandManager(t, sessions, func() bool { return false })
	var ownerLog bytes.Buffer
	logger := log.New(&ownerLog, "", 0)
	manager.report = func(err error) { logger.Print(err) }
	manager.Sync([]meshserve.Service{demandService(time.Minute)})
	if err := manager.Start(context.Background(), "dev"); err == nil {
		t.Fatal("failed command started successfully")
	}
	if lines := strings.Count(ownerLog.String(), "\n"); lines != 1 || !strings.Contains(ownerLog.String(), "status 7") {
		t.Errorf("owner-only start did not log one diagnostic: %q", ownerLog.String())
	}
}
