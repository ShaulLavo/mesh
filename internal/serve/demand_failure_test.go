package serve

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestDemandFailureKeepsDiagnosticsInOwnerLog(t *testing.T) {
	t.Parallel()
	const diagnostic = "route /dev did not start: exit status 7 (session TESTSESSION, command \"SECRET=x ./dev\")\n\nlast output:\nprivate log line"
	registry, err := NewRegistry([]Service{onDemand()})
	if err != nil {
		t.Fatal(err)
	}
	var ownerLog bytes.Buffer
	logger := log.New(&ownerLog, "", 0)
	failure := LogDemandFailure(errString(diagnostic), func(err error) { logger.Print(err) })
	registry.SetDemandGate(&fakeGate{err: failure})

	var previousID string
	for range 2 {
		response := httptest.NewRecorder()
		registry.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/dev/", nil))
		if response.Code != http.StatusBadGateway {
			t.Fatalf("failed start answered %d, want 502", response.Code)
		}
		for _, private := range []string{"/dev", "TESTSESSION", "SECRET=x ./dev", "private log line", "exit status 7"} {
			if strings.Contains(response.Body.String(), private) {
				t.Errorf("HTTP failure disclosed %q", private)
			}
		}
		match := regexp.MustCompile(`^on-demand service unavailable; reference ([A-Z2-7]{26,})\n`).FindStringSubmatch(response.Body.String())
		if match == nil {
			t.Fatalf("failure has no generic message, opaque reference, and owner hint: %q", response.Body.String())
		}
		if previousID != "" && match[1] != previousID {
			t.Errorf("two waiters on one failure received different references")
		}
		previousID = match[1]
		if want := "on-demand service unavailable; reference " + previousID + "\nThe owner can see details with `mesh serve ls` and `mesh logs <session>` on the host.\n"; response.Body.String() != want {
			t.Errorf("failure body = %q, want %q", response.Body.String(), want)
		}
		if !strings.Contains(ownerLog.String(), "daemon: on-demand failure "+match[1]+": "+fmt.Sprintf("%q", diagnostic)) {
			t.Fatalf("owner log lost the correlated diagnostic: %q", ownerLog.String())
		}
		if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("failure headers = %v", response.Header())
		}
	}
	if count := strings.Count(ownerLog.String(), "on-demand failure "); count != 1 {
		t.Errorf("two waiting requests logged %d diagnostics, want 1", count)
	}
}

func TestDemandFailureEscapesDiagnosticLog(t *testing.T) {
	t.Parallel()
	const diagnostic = "private output\n2026/10/01 00:00:00 daemon: forged entry\r\x1b[2J"
	var ownerLog bytes.Buffer
	logger := log.New(&ownerLog, "", 0)
	failure := LogDemandFailure(errString(diagnostic), func(err error) { logger.Print(err) })
	WriteDemandFailure(httptest.NewRecorder(), failure)
	if lines := strings.Count(ownerLog.String(), "\n"); lines != 1 {
		t.Errorf("one failed start wrote %d log lines, want 1", lines)
	}
	if !strings.Contains(ownerLog.String(), fmt.Sprintf("%q", diagnostic)) {
		t.Errorf("owner log did not escape process output: %q", ownerLog.String())
	}
}

func TestDemandFailurePreservesCancellationStatus(t *testing.T) {
	t.Parallel()
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		err := fmt.Errorf("private diagnostic: %w", cause)
		for _, failure := range []error{err, LogDemandFailure(err, func(error) {})} {
			response := httptest.NewRecorder()
			WriteDemandFailure(response, failure)
			if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "private diagnostic") {
				t.Fatalf("cancelled admission answered %d %q", response.Code, response.Body.String())
			}
			if !errors.Is(failure, cause) {
				t.Fatalf("failure lost its cancellation cause: %v", failure)
			}
		}
	}
}

func TestDemandFailureReferencesIdentifyStartAttempts(t *testing.T) {
	t.Parallel()
	first := LogDemandFailure(errString("same diagnostic"), func(error) {})
	second := LogDemandFailure(errString("same diagnostic"), func(error) {})
	responses := make([]string, 0, 2)
	for _, failure := range []error{first, second} {
		response := httptest.NewRecorder()
		WriteDemandFailure(response, fmt.Errorf("wrapped: %w", failure))
		responses = append(responses, response.Body.String())
	}
	if responses[0] == responses[1] {
		t.Fatal("independent failed starts shared a reference")
	}
}
