package transport

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
)

func benchmarkAuthentication(b *testing.B, mode string, checks *atomic.Int64) (*Authentication, *Authentication) {
	b.Helper()
	if mode == "raw" {
		return nil, nil
	}
	serverID, serverKey := authIdentity(b)
	clientID, clientKey := authIdentity(b)
	authorize := func(id string) bool { checks.Add(1); return id == clientID }
	state := ""
	if mode == "tls-grants" {
		state = b.TempDir()
		if err := identity.ApproveDevice(state, clientID); err != nil {
			b.Fatal(err)
		}
		authorize = func(id string) bool { checks.Add(1); return identity.GrantedIdentity(state, id) }
	}
	serverAuth := &Authentication{Key: serverKey, Authorize: authorize}
	if mode == "tls-grants" {
		serverAuth.Bind = func(id string) Authorization {
			current, ok := identity.BindIdentity(state, id)
			return Authorization{Full: ok, Current: func() bool { checks.Add(1); return ok && current() }}
		}
	}
	return serverAuth, &Authentication{Key: clientKey, ExpectedIdentity: serverID}
}

func benchmarkControlServer(b *testing.B, auth *Authentication) *httptest.Server {
	b.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = ServeWithOptions(w, r, ServeOptions{Auth: auth}, func(_ context.Context, conn Conn) error {
			for {
				frame, err := conn.ReadFrame()
				if err != nil {
					return fmt.Errorf("benchmark read: %w", err)
				}
				if err := conn.WriteFrame(frame); err != nil {
					return fmt.Errorf("benchmark echo: %w", err)
				}
			}
		})
	}))
	b.Cleanup(server.Close)
	return server
}

func BenchmarkControlRoundTrip(b *testing.B) {
	for _, size := range []int{1, 64 << 10} {
		for _, mode := range []string{"raw", "tls", "tls-grants"} {
			b.Run(fmt.Sprintf("bytes=%d/mode=%s", size, mode), func(b *testing.B) {
				var checks atomic.Int64
				serverAuth, clientAuth := benchmarkAuthentication(b, mode, &checks)
				server := benchmarkControlServer(b, serverAuth)
				conn, err := DialOnce(context.Background(), server.URL, DialOptions{Auth: clientAuth})
				if err != nil {
					b.Fatal(err)
				}
				defer conn.Close() //nolint:errcheck // benchmark cleanup
				id, err := protocol.NewSessionID("PERF")
				if err != nil {
					b.Fatal(err)
				}
				frame := protocol.Frame{Kind: protocol.KindInput, Session: id, Payload: make([]byte, size)}
				b.SetBytes(int64(size))
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					if err := conn.WriteFrame(frame); err != nil {
						b.Fatal(err)
					}
					response, err := conn.ReadFrame()
					if err != nil {
						b.Fatal(err)
					}
					if len(response.Payload) != size {
						b.Fatal("terminal payload truncated")
					}
				}
			})
		}
	}
}

func BenchmarkIdleControlConnections(b *testing.B) {
	const count = 100
	for _, mode := range []string{"raw", "tls", "tls-grants"} {
		b.Run(mode, func(b *testing.B) {
			var checks atomic.Int64
			serverAuth, clientAuth := benchmarkAuthentication(b, mode, &checks)
			server := benchmarkControlServer(b, serverAuth)
			for range count {
				conn, err := DialOnce(context.Background(), server.URL, DialOptions{Auth: clientAuth})
				if err != nil {
					b.Fatal(err)
				}
				b.Cleanup(func() { _ = conn.Close() })
			}
			runtime.GC()
			var before, after syscall.Rusage
			if err := syscall.Getrusage(syscall.RUSAGE_SELF, &before); err != nil {
				b.Fatal(err)
			}
			initial := checks.Load()
			b.ResetTimer()
			started := time.Now()
			for range b.N {
				time.Sleep(time.Second)
			}
			b.StopTimer()
			elapsed := time.Since(started).Seconds()
			if err := syscall.Getrusage(syscall.RUSAGE_SELF, &after); err != nil {
				b.Fatal(err)
			}
			cpu := after.Utime.Nano() + after.Stime.Nano() - before.Utime.Nano() - before.Stime.Nano()
			b.ReportMetric(float64(cpu)/count/elapsed, "cpu-ns/conn-s")
			b.ReportMetric(float64(checks.Load()-initial)/count/elapsed, "checks/conn-s")
		})
	}
}
