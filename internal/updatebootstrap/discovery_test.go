package updatebootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/transport"
)

type discoveryHost struct {
	Alias             string         `json:"alias"`
	Endpoint          string         `json:"endpoint"`
	ID                string         `json:"id,omitempty"`
	MeshIdentity      string         `json:"meshIdentity,omitempty"`
	Build             *release.Build `json:"build,omitempty"`
	RecoverySupported bool           `json:"recoverySupported"`
	SessionCount      int            `json:"sessionCount"`
	ActiveSessions    int            `json:"activeSessions"`
	Problem           string         `json:"problem,omitempty"`
}

// This optional read-only probe records discovery evidence without adopting a
// host, sending a shell command, or changing a remote installation.
func TestReadOnlyFleetDiscovery(t *testing.T) {
	encoded := os.Getenv("MESH_DISCOVERY_TARGETS")
	if encoded == "" {
		t.Skip("set MESH_DISCOVERY_TARGETS for explicit read-only endpoint discovery")
	}
	var hosts []discoveryHost
	if err := json.Unmarshal([]byte(encoded), &hosts); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for index := range hosts {
		group.Go(func() {
			if err := discoverHost(&hosts[index]); err != nil {
				hosts[index].Problem = err.Error()
			}
		})
	}
	group.Wait()
	data, err := json.MarshalIndent(hosts, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := os.Getenv("MESH_DISCOVERY_OUTPUT")
	if path == "" {
		t.Fatal("MESH_DISCOVERY_OUTPUT is required")
	}
	if err = os.WriteFile(path, append(data, '\n'), 0600); err != nil { //nolint:gosec // explicit operator-selected evidence output in this opt-in read-only test
		t.Fatal(err)
	}
	t.Logf("read-only discovery: %s", data)
}

func discoverHost(host *discoveryHost) error {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	conn, err := transport.DialOnce(ctx, host.Endpoint, transport.DialOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	info, err := remoteExchange(conn, protocol.Control{Type: protocol.TypeHostInfo, RequestID: "read-only-host-info"})
	if err != nil {
		return err
	}
	if info.Type != protocol.TypeHostInfoResult || info.Host == nil {
		return errors.New("endpoint did not return Mesh host information")
	}
	host.ID, host.MeshIdentity, host.Build, host.RecoverySupported = info.Host.ID, info.Host.MeshIdentity, info.Host.Build, info.Host.RecoverySupported
	sessions, err := remoteExchange(conn, protocol.Control{Type: protocol.TypeList, RequestID: "read-only-session-count"})
	if err != nil {
		return err
	}
	if sessions.Type != protocol.TypeListed {
		return fmt.Errorf("session inventory returned %s", sessions.Type)
	}
	host.SessionCount = len(sessions.Sessions)
	for _, session := range sessions.Sessions {
		if session.State == "running" || session.State == "detached" {
			host.ActiveSessions++
		}
	}
	return nil
}

func remoteExchange(conn transport.Conn, request protocol.Control) (protocol.Control, error) {
	data, err := json.Marshal(request)
	if err != nil {
		return protocol.Control{}, err
	}
	if err = conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: data}); err != nil {
		return protocol.Control{}, err
	}
	frame, err := conn.ReadFrame()
	if err != nil {
		return protocol.Control{}, err
	}
	if frame.Kind != protocol.KindControl {
		return protocol.Control{}, errors.New("unexpected remote discovery frame")
	}
	response, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return response, err
	}
	if response.RequestID != request.RequestID {
		return response, errors.New("discovery response ID mismatch")
	}
	return response, nil
}
