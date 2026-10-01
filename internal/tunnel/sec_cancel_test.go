package tunnel

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

func TestSecurityCallerCancellationDoesNotStopHealthyForward(t *testing.T) {
	for _, accept := range []bool{true, false} {
		name := "late rejection"
		if accept {
			name = "late acceptance"
		}
		t.Run(name, func(t *testing.T) {
			const hostname = "blog.shaulavo.dev"
			const sibling = "other.shaulavo.dev"
			f := newSSHFixture(t, time.Second, hostname, sibling)
			conn, channels := f.client(t, true)
			for _, forward := range []string{hostname, sibling} {
				if !sendForward(t, conn, "tcpip-forward", forward, 80) {
					t.Fatal("forward refused")
				}
			}
			endpoint := f.activator.endpoint(hostname).(*sshEndpoint)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				stream, err := endpoint.Dial(ctx)
				if stream != nil {
					_ = stream.Close()
				}
				result <- err
			}()
			var pending gossh.NewChannel
			select {
			case pending = <-channels:
			case <-time.After(time.Second):
				t.Fatal("channel open did not reach the peer")
			}
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("Dial error = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("cancelled dial did not return")
			}
			if f.activator.endpoint(hostname) != endpoint || f.activator.endpoint(sibling) == nil {
				t.Fatal("one cancelled public request unpublished a healthy reverse tunnel")
			}
			if len(endpoint.slots) != 1 || len(endpoint.global) != 1 {
				t.Fatal("pending channel open lost its reservation")
			}
			if accept {
				channel, requests, err := pending.Accept()
				if err != nil {
					t.Fatalf("accept late channel: %v", err)
				}
				defer func() { _ = channel.Close() }()
				go gossh.DiscardRequests(requests)
				closed := make(chan error, 1)
				go func() { _, err := io.Copy(io.Discard, channel); closed <- err }()
				select {
				case err := <-closed:
					if err != nil {
						t.Fatalf("late channel close: %v", err)
					}
				case <-time.After(time.Second):
					t.Fatal("cancelled dial leaked its late channel")
				}
				_ = channel.Close()
			} else if err := pending.Reject(gossh.ConnectionFailed, "fixture rejection"); err != nil {
				t.Fatal(err)
			}
			waitTunnel(t, func() bool { return len(endpoint.slots) == 0 && len(endpoint.global) == 0 })
			accepted := make(chan gossh.Channel, maximumStreamsPerForward)
			go acceptForwardChannels(channels, accepted)
			for range maximumStreamsPerForward {
				stream, err := endpoint.Dial(context.Background())
				if err != nil {
					t.Fatalf("released reservation not reusable: %v", err)
				}
				t.Cleanup(func() { _ = stream.Close() })
			}
			if _, err := endpoint.Dial(context.Background()); !errors.Is(err, ErrCapacity) {
				t.Fatalf("channel budget changed after cancellation: %v", err)
			}
		})
	}
}
