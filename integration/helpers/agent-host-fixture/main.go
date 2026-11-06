package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shaul/mesh/internal/hostprofile"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/transport"
)

type fixture struct {
	root       string
	host       identity.Host
	auth       *transport.Authentication
	mode       atomic.Value
	mu         sync.Mutex
	operations []string
	server     *httptest.Server
}

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) != 2 {
		return errors.New("pass a task-owned fixture root")
	}
	root := os.Args[1]
	caller, _, err := identity.LoadOrCreate(filepath.Join(root, "state"))
	if err != nil {
		return fmt.Errorf("host fixture: %w", err)
	}
	host, key, err := identity.LoadOrCreate(filepath.Join(root, "remote"))
	if err != nil {
		return fmt.Errorf("host fixture: %w", err)
	}
	f := &fixture{root: root, host: host, auth: &transport.Authentication{Key: key, Authorize: func(id string) bool { return id == caller.ID }}, operations: []string{}}
	f.mode.Store("live")
	if err := f.profile("host notes", 0); err != nil {
		return fmt.Errorf("host fixture: %w", err)
	}
	doc, err := signFleet(hostprofile.Editable{SchemaVersion: 1, Instructions: "fleet notes"}, "fixture", 1, time.Now().UTC(), host.ID, key)
	if err != nil {
		return fmt.Errorf("host fixture: %w", err)
	}
	if err := writeJSON(filepath.Join(root, "remote", "fleet", "profile.json"), doc); err != nil {
		return fmt.Errorf("host fixture: %w", err)
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	defer f.server.Close()
	endpoint := "ws" + strings.TrimPrefix(f.server.URL, "http") + "/control/ws"
	if err := writeJSON(filepath.Join(root, "config", "hosts.json"), map[string]any{"version": 1, "hosts": []map[string]string{{"id": host.ID, "meshIdentity": host.ID, "endpoint": endpoint}}}); err != nil {
		return fmt.Errorf("host fixture: %w", err)
	}
	output := json.NewEncoder(os.Stdout)
	if err := output.Encode(map[string]string{"id": host.ID, "endpoint": endpoint}); err != nil {
		return fmt.Errorf("host fixture: %w", err)
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		if err := f.command(scanner.Text(), output); err != nil {
			return fmt.Errorf("host fixture: %w", err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read fixture command: %w", err)
	}
	return nil
}

func writeJSON(path string, value any) error {
	// #nosec G703 -- Only task-owned fixture paths are passed by the integration runner.
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("host fixture: %w", err)
	}
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("host fixture: %w", err)
	}
	// #nosec G703 -- Only task-owned fixture paths are passed by the integration runner.
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("write fixture file: %w", err)
	}
	return nil
}

func (f *fixture) profile(text string, expected uint64) error {
	_, _, err := hostprofile.OpenHostStore(filepath.Join(f.root, "remote-config"), f.host.ID).Set(context.Background(), expected, hostprofile.Editable{SchemaVersion: 1, Description: "fixture host", Labels: map[string]string{"purpose": "testing"}, Instructions: text})
	if err != nil {
		return fmt.Errorf("set fixture profile: %w", err)
	}
	return nil
}

func (f *fixture) command(command string, output *json.Encoder) error {
	switch command {
	case "operations":
		f.mu.Lock()
		defer f.mu.Unlock()
		return encodeFixture(output, f.operations)
	case "change":
		if err := f.profile("changed host notes", 1); err != nil {
			return fmt.Errorf("host fixture: %w", err)
		}
	case "regress":
		if err := os.Remove(filepath.Join(f.root, "remote-config", "profile.json")); err != nil {
			return fmt.Errorf("host fixture: %w", err)
		}
		if err := f.profile("reset host notes", 0); err != nil {
			return fmt.Errorf("host fixture: %w", err)
		}
	default:
		f.mode.Store(command)
	}
	return encodeFixture(output, map[string]string{"mode": command})
}

func (f *fixture) serve(w http.ResponseWriter, r *http.Request) {
	_ = transport.ServeWithOptions(w, r, transport.ServeOptions{Auth: f.auth}, f.handle)
}

func (f *fixture) handle(ctx context.Context, conn transport.Conn) error {
	for ctx.Err() == nil {
		request, err := protocol.ReadControl(conn.ReadFrame, conn.WriteFrame)
		if err != nil {
			return fmt.Errorf("host fixture: %w", err)
		}
		f.mu.Lock()
		f.operations = append(f.operations, request.Type)
		f.mu.Unlock()
		if f.mode.Load() == "blocked" {
			<-ctx.Done()
			return fmt.Errorf("fixture connection cancelled: %w", ctx.Err())
		}
		response, err := f.response(request)
		if err != nil {
			return fmt.Errorf("host fixture: %w", err)
		}
		response.RequestID = request.RequestID
		payload, err := response.Encode()
		if err != nil {
			return fmt.Errorf("host fixture: %w", err)
		}
		if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload}); err != nil {
			return fmt.Errorf("host fixture: %w", err)
		}
	}
	return fmt.Errorf("fixture connection cancelled: %w", ctx.Err())
}

func (f *fixture) response(request protocol.Control) (protocol.Control, error) {
	if request.Type == protocol.TypeHostInfo {
		return protocol.Control{Type: protocol.TypeHostInfoResult, Host: &protocol.HostInfo{ID: f.host.ID, MeshIdentity: f.host.ID, MachineName: "fixture", NameRevision: 1, Build: &release.Build{Version: "fixture", Platform: release.CurrentPlatform()}}}, nil
	}
	if request.Type != protocol.TypeHostContext {
		return protocol.Control{}, fmt.Errorf("forbidden passive control %s", request.Type)
	}
	if f.mode.Load() == "unsupported" {
		return protocol.Control{Type: protocol.TypeError, ErrorCode: protocol.ErrorCodeUnknownControl, Message: "unknown context"}, nil
	}
	options, err := hostprofile.ParseContextRequest(request.Context)
	if err != nil {
		return protocol.Control{}, fmt.Errorf("fixture context: %w", err)
	}
	reply := hostprofile.ReadContext(filepath.Join(f.root, "remote-config"), filepath.Join(f.root, "remote"), f.host.ID, func(id string) bool { return id == f.host.ID }, options.Full)
	if f.mode.Load() == "corrupt" {
		reply.Fleet.Signature = "bad"
	}
	data, err := json.Marshal(reply)
	if err != nil {
		return protocol.Control{}, fmt.Errorf("fixture context: %w", err)
	}
	return protocol.Control{Type: protocol.TypeHostContextResult, Context: data}, nil
}

func encodeFixture(output *json.Encoder, value any) error {
	if err := output.Encode(value); err != nil {
		return fmt.Errorf("encode fixture output: %w", err)
	}
	return nil
}
func signFleet(e hostprofile.Editable, fleet string, revision uint64, at time.Time, id string, key ed25519.PrivateKey) (hostprofile.FleetDocument, error) {
	e.Labels, e.Warnings = map[string]string{}, []string{}
	doc := hostprofile.FleetDocument{Editable: e, Fleet: fleet, Revision: revision, UpdatedAt: at, Author: id}
	data, err := json.Marshal(doc)
	if err != nil {
		return doc, fmt.Errorf("encode fixture fleet: %w", err)
	}
	doc.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, append([]byte("mesh-fleet-profile-v1\x00"), data...)))
	if err := hostprofile.VerifyFleet(doc); err != nil {
		return doc, fmt.Errorf("verify fixture fleet: %w", err)
	}
	return doc, nil
}
