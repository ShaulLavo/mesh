package storage

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/tunnel"
)

func TestTunnelOutboxRetainsAmbiguousAndMismatchedAttempts(t *testing.T) {
	ctx := context.Background()
	store := openTunnelTestStore(t)
	target, _ := storageEdgeIdentity(t)
	_, key := storageEdgeIdentity(t)
	var first tunnel.Mutation
	_, err := store.DeliverTunnelMutation(ctx, target, key, tunnel.Create, "retry.shaulavo.dev", func(_ context.Context, mutation tunnel.Mutation) (tunnel.Ack, error) {
		first = mutation
		return tunnel.Ack{}, errors.New("connection lost after send")
	})
	if err == nil {
		t.Fatal("ambiguous delivery was accepted")
	}
	_, err = store.DeliverTunnelMutation(ctx, target, key, tunnel.Create, first.PublicName, func(_ context.Context, mutation tunnel.Mutation) (tunnel.Ack, error) {
		if !reflect.DeepEqual(first, mutation) {
			t.Fatal("ambiguous retry changed signed mutation")
		}
		ack := ackTestTunnel(t, mutation)
		ack.Sequence++
		return ack, nil
	})
	if err == nil {
		t.Fatal("mismatched acknowledgement was accepted")
	}
	ack, err := store.DeliverTunnelMutation(ctx, target, key, tunnel.Create, first.PublicName, func(_ context.Context, mutation tunnel.Mutation) (tunnel.Ack, error) {
		if !reflect.DeepEqual(first, mutation) {
			t.Fatal("mismatched acknowledgement discarded exact pending attempt")
		}
		return ackTestTunnel(t, mutation), nil
	})
	if err != nil || ack.Sequence != 1 {
		t.Fatalf("retry = %+v, %v", ack, err)
	}
	ack, err = store.DeliverTunnelMutation(ctx, target, key, tunnel.Release, first.PublicName, func(_ context.Context, mutation tunnel.Mutation) (tunnel.Ack, error) {
		return ackTestTunnel(t, mutation), nil
	})
	if err != nil || ack.Sequence != 2 {
		t.Fatalf("successor sequence = %+v, %v", ack, err)
	}
	var pending int
	if err := store.db.QueryRowContext(ctx, "SELECT count(*) FROM tunnel_outbox WHERE canonical IS NOT NULL").Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("acknowledged payloads = %d, %v", pending, err)
	}
}

func TestTunnelOutboxDefinitiveRefusalDoesNotBlockRelease(t *testing.T) {
	ctx := context.Background()
	store := openTunnelTestStore(t)
	target, _ := storageEdgeIdentity(t)
	_, key := storageEdgeIdentity(t)
	_, err := store.DeliverTunnelMutation(ctx, target, key, tunnel.Create, "refused.shaulavo.dev", func(_ context.Context, mutation tunnel.Mutation) (tunnel.Ack, error) {
		return tunnel.Ack{}, io.ErrUnexpectedEOF
	})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal(err)
	}
	var sent []tunnel.Mutation
	ack, err := store.DeliverTunnelMutation(ctx, target, key, tunnel.Release, "owned.shaulavo.dev", func(_ context.Context, mutation tunnel.Mutation) (tunnel.Ack, error) {
		sent = append(sent, mutation)
		ack := ackTestTunnel(t, mutation)
		if mutation.Action == tunnel.Create {
			ack.Error = tunnel.ErrCapacity.Error()
		}
		return ack, nil
	})
	if err != nil || ack.Sequence != 2 || len(sent) != 2 || sent[0].Action != tunnel.Create || sent[1].Action != tunnel.Release {
		t.Fatalf("release following pending refusal = %+v, %v, %+v", ack, err, sent)
	}
	ack, err = store.DeliverTunnelMutation(ctx, target, key, tunnel.Create, "again.shaulavo.dev", func(_ context.Context, mutation tunnel.Mutation) (tunnel.Ack, error) {
		ack := ackTestTunnel(t, mutation)
		ack.Error = tunnel.ErrCapacity.Error()
		return ack, nil
	})
	if err == nil || ack.Sequence != 3 || ack.Error != tunnel.ErrCapacity.Error() {
		t.Fatalf("current refusal = %+v, %v", ack, err)
	}
	ack, err = store.DeliverTunnelMutation(ctx, target, key, tunnel.Release, "owned.shaulavo.dev", func(_ context.Context, mutation tunnel.Mutation) (tunnel.Ack, error) {
		return ackTestTunnel(t, mutation), nil
	})
	if err != nil || ack.Sequence != 4 {
		t.Fatalf("sequence after conclusive refusal = %+v, %v", ack, err)
	}
}

func TestTunnelOutboxProcessesSerializeAndRecoverKilledSend(t *testing.T) {
	if os.Getenv("MESH_TEST_TUNNEL_WORKER") != "" {
		runTunnelOutboxWorker(t)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "mesh.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	first, firstRecords := startTunnelWorker(t, ctx, path, "hold", "first.shaulavo.dev")
	original := receiveTunnelWorkerMutation(t, ctx, firstRecords)
	second, secondRecords := startTunnelWorker(t, ctx, path, "finish", "second.shaulavo.dev")
	select {
	case early := <-secondRecords:
		t.Fatalf("second process sent before first released stream lock: %s", early)
	case <-time.After(100 * time.Millisecond):
	}
	if err := first.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = first.Wait()
	retried := receiveTunnelWorkerMutation(t, ctx, secondRecords)
	if !bytes.Equal(original, retried) {
		t.Fatalf("restart changed exact signed attempt:\n%s\n%s", original, retried)
	}
	successorBytes := receiveTunnelWorkerMutation(t, ctx, secondRecords)
	var successor tunnel.Mutation
	if err := json.Unmarshal(successorBytes, &successor); err != nil {
		t.Fatal(err)
	}
	if successor.Sequence != 2 || successor.PublicName != "second.shaulavo.dev" {
		t.Fatalf("successor = %+v", successor)
	}
	if err := second.Wait(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() //nolint:errcheck
	var count int
	if err := store.db.QueryRowContext(ctx, "SELECT count(*) FROM tunnel_claims").Scan(&count); err != nil || count != 2 {
		t.Fatalf("process recovery claims = %d, %v", count, err)
	}
}

func startTunnelWorker(t *testing.T, ctx context.Context, path, mode, name string) (*exec.Cmd, <-chan []byte) {
	t.Helper()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestTunnelOutboxProcessesSerializeAndRecoverKilledSend$") //nolint:gosec // reruns the current test executable
	cmd.Env = append(os.Environ(), "MESH_TEST_TUNNEL_WORKER="+mode, "MESH_TEST_TUNNEL_DB="+path, "MESH_TEST_TUNNEL_NAME="+name)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	records := make(chan []byte, 4)
	go scanTunnelWorker(stdout, records)
	return cmd, records
}

func scanTunnelWorker(stdout io.Reader, records chan<- []byte) {
	defer close(records)
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := bytes.Clone(scanner.Bytes())
		if bytes.HasPrefix(line, []byte("{")) {
			records <- line
		}
	}
}

func receiveTunnelWorkerMutation(t *testing.T, ctx context.Context, records <-chan []byte) []byte {
	t.Helper()
	select {
	case record, ok := <-records:
		if !ok {
			t.Fatal("worker exited before sending its mutation")
		}
		return record
	case <-ctx.Done():
		t.Fatal("timed out waiting for outbox worker")
		return nil
	}
}

func runTunnelOutboxWorker(t *testing.T) {
	store, err := Open(context.Background(), os.Getenv("MESH_TEST_TUNNEL_DB"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() //nolint:errcheck
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{17}, ed25519.SeedSize))
	edgeKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{19}, ed25519.SeedSize))
	target := tunnel.KeyID(edgeKey.Public().(ed25519.PublicKey))
	_, err = store.DeliverTunnelMutation(context.Background(), target, key, tunnel.Create, os.Getenv("MESH_TEST_TUNNEL_NAME"), func(_ context.Context, mutation tunnel.Mutation) (tunnel.Ack, error) {
		return sendTunnelWorkerMutation(t, store, mutation)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func sendTunnelWorkerMutation(t *testing.T, store *Store, mutation tunnel.Mutation) (tunnel.Ack, error) {
	t.Helper()
	if err := applyTestTunnel(store, mutation); err != nil {
		return tunnel.Ack{}, err
	}
	encoded, err := json.Marshal(mutation)
	if err != nil {
		return tunnel.Ack{}, err
	}
	fmt.Println(string(encoded))
	if os.Getenv("MESH_TEST_TUNNEL_WORKER") == "hold" {
		time.Sleep(time.Minute)
	}
	return ackTestTunnel(t, mutation), nil
}

func ackTestTunnel(t *testing.T, mutation tunnel.Mutation) tunnel.Ack {
	t.Helper()
	digest, err := tunnel.Verify(mutation, mutation.TargetID)
	if err != nil {
		t.Fatal(err)
	}
	return tunnel.Ack{Sequence: mutation.Sequence, Digest: digest}
}
