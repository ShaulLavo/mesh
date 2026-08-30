package daemon

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/tailnet"
)

func TestMonitorTailnetAddressesIgnoresTransientObservationsAndSignalsChange(t *testing.T) {
	discoveryErr := errors.New("temporary tailscaled failure")
	responses := []struct {
		peer tailnet.Peer
		err  error
	}{
		{err: discoveryErr},
		{peer: tailnet.Peer{}},
		{peer: tailnet.Peer{Addrs: []string{"192.0.2.1"}}},
		{peer: tailnet.Peer{Addrs: []string{"fd7a:115c:a1e0::1", "100.64.0.1", "100.64.0.1"}}},
		{peer: tailnet.Peer{Addrs: []string{"100.64.0.2", "fd7a:115c:a1e0::1"}}},
	}
	var mu sync.Mutex
	next := 0
	discover := func(context.Context) (tailnet.Peer, error) {
		mu.Lock()
		defer mu.Unlock()
		if next >= len(responses) {
			return responses[len(responses)-1].peer, nil
		}
		response := responses[next]
		next++
		return response.peer, response.err
	}
	initial, err := normalizeTailnetAddresses([]string{"100.64.0.1", "fd7a:115c:a1e0::1"})
	if err != nil {
		t.Fatal(err)
	}
	var reported []error
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = monitorTailnetAddresses(ctx, time.Millisecond, time.Second, initial, discover, validateTailscaleServeAddresses, func(err error) {
		reported = append(reported, err)
	})
	if !errors.Is(err, ErrTailnetAddressesChanged) || !strings.Contains(err.Error(), "100.64.0.2") {
		t.Fatalf("monitor error = %v", err)
	}
	if len(reported) != 3 || !errors.Is(reported[0], discoveryErr) || !strings.Contains(reported[1].Error(), "retaining") || !strings.Contains(reported[2].Error(), "outside the Tailscale") {
		t.Fatalf("reported transient observations = %#v", reported)
	}
}

func TestMonitorTailnetAddressesTreatsExternalCancellationAsCleanShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	initial := []string{"100.64.0.1"}
	err := monitorTailnetAddresses(ctx, time.Millisecond, time.Second, initial, func(context.Context) (tailnet.Peer, error) {
		cancel()
		return tailnet.Peer{Addrs: []string{"100.64.0.2"}}, nil
	}, validateTailscaleServeAddresses, func(error) {})
	if err != nil {
		t.Fatalf("monitor returned a restart error after external cancellation: %v", err)
	}
}

func TestMonitorTailnetAddressesTimesOutOneObservationAndRetries(t *testing.T) {
	calls := 0
	var reported []error
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := monitorTailnetAddresses(ctx, time.Millisecond, 5*time.Millisecond, []string{"100.64.0.1"}, func(ctx context.Context) (tailnet.Peer, error) {
		calls++
		if calls == 1 {
			<-ctx.Done()
			return tailnet.Peer{}, ctx.Err()
		}
		return tailnet.Peer{Addrs: []string{"100.64.0.2"}}, nil
	}, validateTailscaleServeAddresses, func(err error) {
		reported = append(reported, err)
	})
	if !errors.Is(err, ErrTailnetAddressesChanged) || calls != 2 {
		t.Fatalf("monitor error = %v, calls = %d", err, calls)
	}
	if len(reported) != 1 || !errors.Is(reported[0], context.DeadlineExceeded) {
		t.Fatalf("timeout reports = %#v", reported)
	}
}
