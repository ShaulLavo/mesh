package update

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/transport"
)

func TestSignedUpdaterDoesNotFallBackToRawControls(t *testing.T) {
	actor, key, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target, _, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var dispatched atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = transport.Serve(w, r, func(_ context.Context, conn transport.Conn) error {
			if _, err := conn.ReadFrame(); err != nil {
				return fmt.Errorf("read legacy fixture: %w", err)
			}
			dispatched.Add(1)
			return nil
		})
	}))
	defer server.Close()
	client := Client{ID: actor.ID, Key: key}
	err = client.Call(context.Background(), Host{ID: target.ID, MachineName: "old", Endpoint: strings.Replace(server.URL, "http://", "ws://", 1)}, "info", nil, nil)
	if !errors.Is(err, transport.ErrAuthenticationRequired) {
		t.Fatalf("legacy endpoint returned %v; want upgrade required", err)
	}
	if dispatched.Load() != 0 {
		t.Fatalf("raw update requests dispatched=%d; want 0", dispatched.Load())
	}
}
