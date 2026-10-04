package cli

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"sync"
	"testing"

	"github.com/shaul/mesh/internal/machinename"
)

func BenchmarkLoadHostsClaimCache(b *testing.B) {
	for _, count := range []int{1, 10, 100} {
		for _, writers := range []bool{false, true} {
			b.Run(fmt.Sprintf("hosts-%d/writers-%t", count, writers), func(b *testing.B) {
				benchmarkLoadHostsClaimCache(b, count, writers)
			})
		}
	}
}

func benchmarkLoadHostsClaimCache(b *testing.B, count int, writers bool) {
	directory := b.TempDir()
	b.Setenv("MESH_CONFIG_DIR", directory)
	config := hostConfig{Version: hostConfigVersion}
	claims := make([]machinename.Claim, count)
	for i := range count {
		key := make([]byte, 32)
		binary.BigEndian.PutUint64(key, uint64(i+1))
		id := base64.RawURLEncoding.EncodeToString(key)
		claims[i] = machinename.Claim{ID: id, MachineName: fmt.Sprintf("fixture-%d", i), Revision: 1}
		config.Hosts = append(config.Hosts, HostRecord{ID: id, MeshIdentity: id, Endpoint: "ws://127.0.0.1:1/control/ws"})
		if _, err := machinename.RememberClaim(b.Context(), directory, id, claims[i]); err != nil {
			b.Fatal(err)
		}
	}
	if err := writeHostConfig(config); err != nil {
		b.Fatal(err)
	}
	stop, done := make(chan struct{}), make(chan struct{})
	errors := make(chan error, 8)
	var workers sync.WaitGroup
	if writers {
		for i := range min(count, 8) {
			workers.Go(func() { benchmarkCacheWriter(b, directory, claims[i], stop, errors) })
		}
	}
	go func() { workers.Wait(); close(done) }()
	b.ResetTimer()
	for range b.N {
		hosts, err := LoadHosts()
		if err != nil || len(hosts) != count {
			b.Fatalf("load hosts: count=%d error=%v", len(hosts), err)
		}
	}
	b.StopTimer()
	close(stop)
	<-done
	close(errors)
	for err := range errors {
		b.Fatal(err)
	}
}

func benchmarkCacheWriter(b *testing.B, directory string, claim machinename.Claim, stop <-chan struct{}, errors chan<- error) {
	for {
		select {
		case <-stop:
			return
		default:
		}
		if _, err := machinename.RememberClaim(b.Context(), directory, claim.ID, claim); err != nil {
			errors <- err
			return
		}
	}
}
