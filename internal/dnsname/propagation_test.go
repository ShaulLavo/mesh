package dnsname

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWaitForTXTRequiresEveryAuthoritativeServer(t *testing.T) {
	observer := &sequenceObserver{observations: [][]TXTObservation{
		{{Server: "one", Values: []string{"wanted"}}, {Server: "two", Values: []string{"old"}}},
		{{Server: "one", Values: []string{"wanted"}}, {Server: "two", Values: []string{"wanted"}}},
	}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := WaitForTXT(ctx, observer, Zone, "_acme-challenge.mesh.shaulavo.dev", "wanted", time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if observer.calls != 2 {
		t.Fatalf("observer calls = %d, want 2", observer.calls)
	}
}

func TestWaitForTXTStopsAtContextBound(t *testing.T) {
	observer := &sequenceObserver{err: errors.New("DNS unavailable")}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	err := WaitForTXT(ctx, observer, Zone, "_acme-challenge.mesh.shaulavo.dev", "wanted", time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline", err)
	}
}

type sequenceObserver struct {
	observations [][]TXTObservation
	err          error
	calls        int
}

func (o *sequenceObserver) ObserveTXT(context.Context, string, string) ([]TXTObservation, error) {
	o.calls++
	if o.err != nil {
		return nil, o.err
	}
	index := o.calls - 1
	if index >= len(o.observations) {
		index = len(o.observations) - 1
	}
	return o.observations[index], nil
}
