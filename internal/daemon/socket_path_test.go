package daemon

import (
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
		if entry.Name() != daemonLockName {
			t.Fatalf("failed startup left %s", entry.Name())
		}
	}
}
