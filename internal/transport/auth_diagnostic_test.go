package transport

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestControlAuthenticationRequirementHasSpecificDiagnostic(t *testing.T) {
	_, serverKey := authIdentity(t)
	var handled atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = ServeWithOptions(w, r, ServeOptions{Auth: &Authentication{Key: serverKey, Authorize: func(string) bool { return true }}}, func(context.Context, Conn) error {
			handled.Store(true)
			return nil
		})
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	connection, err := DialOnce(ctx, server.URL, DialOptions{})
	if connection != nil {
		_ = connection.Close()
	}
	if !errors.Is(err, ErrControlAuthenticationRequired) || errors.Is(err, ErrAuthenticationRequired) {
		t.Fatalf("server requiring client authentication returned %v; need the destination requirement, separate from a legacy-server upgrade", err)
	}
	if !strings.Contains(err.Error(), "HTTP 426") || !strings.Contains(err.Error(), "destination requires Mesh control authentication") {
		t.Fatalf("diagnostic lost HTTP status or authentication requirement: %v", err)
	}
	if handled.Load() {
		t.Fatal("unauthenticated client reached the control handler")
	}
}

func TestOtherUpgradeFailuresKeepTheirHTTPDiagnostic(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		marker string
		want   string
	}{
		{"missing-auth-marker", http.StatusUpgradeRequired, "", "Upgrade Required"},
		{"unknown-auth-protocol", http.StatusUpgradeRequired, "other-protocol", "Upgrade Required"},
		{"different-http-failure", http.StatusForbidden, AuthProtocol, "Forbidden"},
		{"connection-cap", http.StatusServiceUnavailable, AuthProtocol, "Tailnet control connection cap (2)"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set(ControlAuthenticationHeader, tt.marker)
				w.Header().Set(ControlConnectionLimitHeader, "2")
				http.Error(w, "fixture refusal", tt.status)
			}))
			defer server.Close()
			connection, err := DialOnce(context.Background(), server.URL, DialOptions{})
			if connection != nil {
				_ = connection.Close()
			}
			if err == nil || errors.Is(err, ErrControlAuthenticationRequired) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("unrelated HTTP refusal misclassified: %v", err)
			}
		})
	}
}

func TestControlAuthenticationRequirementEndsReconnectAttempts(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			connection, err := websocket.Accept(w, r, nil)
			if err == nil {
				_ = connection.CloseNow()
			}
			return
		}
		w.Header().Set(ControlAuthenticationHeader, AuthProtocol)
		http.Error(w, "Mesh control authentication required", http.StatusUpgradeRequired)
	}))
	defer server.Close()
	connection, err := Dial(context.Background(), server.URL, DialOptions{Backoff: Backoff{Initial: time.Millisecond, Max: time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	result := make(chan error, 1)
	go func() { _, err := connection.ReadFrame(); result <- err }()
	select {
	case err := <-result:
		if !errors.Is(err, ErrControlAuthenticationRequired) || requests.Load() != 2 {
			t.Fatalf("permanent requirement retried or lost after initial connection: %v, requests %d", err, requests.Load())
		}
	case <-time.After(500 * time.Millisecond):
		_ = connection.Close()
		err := <-result
		t.Fatalf("control authentication refusal kept reconnecting: %v, requests %d", err, requests.Load())
	}
}
