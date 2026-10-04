package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServeRejectsLongSocketPathPromptly(t *testing.T) {
	state := filepath.Join(compactSocketTempDir(t), strings.Repeat("x", 110))
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	listener, port := newTCPListener(t, "127.0.0.1:0")
	done := runRuntime(t, t.Context(), ListenerConfig{
		StateDir: state, TailnetAddrs: []string{"127.0.0.1"}, TailnetPort: port, WebSocketPath: "/mesh",
	}, echoOneFrame, listener)
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "MESH_STATE_DIR") || !strings.Contains(err.Error(), "bytes") {
			t.Fatalf("startup error = %v, want socket path bytes and shorter MESH_STATE_DIR guidance", err)
		}
		t.Log(err)
	case <-time.After(time.Second):
		t.Fatal("overlong socket path did not fail promptly")
	}
	entries, err := os.ReadDir(state)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == daemonSocketName || strings.HasPrefix(entry.Name(), ".d-") {
			t.Fatalf("failed startup left %s", entry.Name())
		}
	}
}

func TestTCPReadinessObservesStartupFailure(t *testing.T) {
	listener, _ := newTCPListener(t, "127.0.0.1:0")
	want := errors.New("fixture socket bind failed")
	done := make(chan error, 1)
	done <- want
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := awaitTCPRuntime(ctx, listener.Addr().String(), done); !errors.Is(err, want) {
		t.Fatalf("pre-bound listener readiness = %v, want startup failure", err)
	}
}

func TestTCPReadinessRequiresHTTPResponse(t *testing.T) {
	listener, _ := newTCPListener(t, "127.0.0.1:0")
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	if err := awaitTCPRuntime(ctx, listener.Addr().String(), make(chan error)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("idle pre-bound listener readiness = %v, want deadline exceeded", err)
	}
}
