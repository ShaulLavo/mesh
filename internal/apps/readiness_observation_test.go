package apps

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"syscall"
	"testing"
	"time"
)

func TestReadinessReobservesIncompleteSnapshot(t *testing.T) {
	for _, unsafe := range []bool{false, true} {
		t.Run(fmt.Sprintf("unsafe=%t", unsafe), func(t *testing.T) { testReadinessReobservation(t, unsafe) })
	}
}

func testReadinessReobservation(t *testing.T, unsafe bool) {
	t.Helper()
	o := &Origin{serving: map[string]serving{}}
	partial := listenerSocket{Address: netip.MustParseAddrPort("127.0.0.1:3000"), Own: true, Inode: 41}
	complete := listenerSocket{Address: netip.MustParseAddrPort("127.0.0.1:3000"), Own: true, Inode: 42}
	calls := 0
	fault := listenerFault("port 3000 is held by a process outside the app")
	inspect := func(ctx context.Context) (listenerSocket, error) {
		calls++
		if o.serving["probe"].upstream.IsValid() {
			t.Fatal("published a partial observation")
		}
		if calls == 1 {
			return partial, fmt.Errorf("descriptor denied: %w", errIncompleteListenerObservation)
		}
		if unsafe {
			return listenerSocket{}, fault
		}
		return complete, nil
	}
	err := o.awaitServer(context.Background(), "probe", "session", inspect)
	if calls != 2 {
		t.Fatalf("observations = %d, error %v; want exactly two", calls, err)
	}
	if unsafe {
		assertReadinessRefused(t, o, err, fault)
		return
	}
	if err != nil || o.serving["probe"].inode != complete.Inode {
		t.Fatalf("complete new snapshot = %+v, %v", o.serving["probe"], err)
	}
}

func TestReadinessPermanentIncompleteKeepsDiagnostic(t *testing.T) {
	o := &Origin{serving: map[string]serving{}}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	cause := fmt.Errorf("process 11 descriptor 3: %w", errIncompleteListenerObservation)
	calls := 0
	inspect := func(context.Context) (listenerSocket, error) {
		calls++
		return listenerSocket{}, cause
	}
	err := o.awaitServer(ctx, "probe", "session", inspect)
	assertReadinessRefused(t, o, err, cause)
	if !errors.Is(err, context.DeadlineExceeded) || calls != 1 {
		t.Fatalf("permanent incomplete observation = %v, calls %d", err, calls)
	}
}

func TestReadinessCancellationAndIdentityFailure(t *testing.T) {
	for _, result := range []string{"cancel-incomplete", "cancel-complete", "identity-change"} {
		t.Run(result, func(t *testing.T) { testReadinessFailure(t, result) })
	}
}

func testReadinessFailure(t *testing.T, result string) {
	t.Helper()
	o := &Origin{serving: map[string]serving{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	identity := errors.New("process identity changed")
	calls := 0
	inspect := func(context.Context) (listenerSocket, error) {
		calls++
		if result == "cancel-complete" {
			cancel()
			return listenerSocket{Address: netip.MustParseAddrPort("127.0.0.1:3000"), Own: true}, nil
		}
		if result == "cancel-incomplete" {
			cancel()
		}
		if calls == 1 {
			return listenerSocket{}, errIncompleteListenerObservation
		}
		return listenerSocket{}, identity
	}
	err := o.awaitServer(ctx, "probe", "session", inspect)
	if result == "identity-change" {
		assertReadinessRefused(t, o, err, identity)
		if calls != 2 {
			t.Fatalf("identity failure observations = %d; want two", calls)
		}
		return
	}
	assertReadinessRefused(t, o, err, context.Canceled)
	if calls != 1 {
		t.Fatalf("canceled observation retried %d times", calls)
	}
}

func TestReadinessCompletePositiveControl(t *testing.T) {
	o := &Origin{serving: map[string]serving{}}
	calls := 0
	inspect := func(ctx context.Context) (listenerSocket, error) {
		calls++
		deadline, ok := ctx.Deadline()
		if remaining := time.Until(deadline); !ok || remaining < 29*time.Second || remaining > 30*time.Second {
			t.Fatalf("readiness budget changed: %s, present %t", remaining, ok)
		}
		return listenerSocket{Address: netip.MustParseAddrPort("127.0.0.1:3000"), Own: true, Inode: 42}, nil
	}
	if err := o.awaitServer(context.Background(), "probe", "session", inspect); err != nil || calls != 1 || o.serving["probe"].inode != 42 {
		t.Fatalf("complete positive control = %+v, %v, calls %d", o.serving["probe"], err, calls)
	}
}

func assertReadinessRefused(t *testing.T, o *Origin, err, cause error) {
	t.Helper()
	if !errors.Is(err, cause) || o.serving["probe"].upstream.IsValid() {
		t.Fatalf("readiness = %v, serving %+v; want refusal with %v", err, o.serving["probe"], cause)
	}
}

func TestReadinessCanceledBeforeObservation(t *testing.T) {
	o := &Origin{serving: map[string]serving{}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	inspect := func(context.Context) (listenerSocket, error) {
		t.Fatal("inspected after cancellation")
		return listenerSocket{}, nil
	}
	err := o.awaitServer(ctx, "probe", "session", inspect)
	assertReadinessRefused(t, o, err, context.Canceled)
}

func TestReadinessHardRefusalIsNotRetried(t *testing.T) {
	for _, cause := range []error{errors.New("cannot verify process identity"), syscall.EACCES, syscall.EPERM, syscall.EIO} {
		t.Run(cause.Error(), func(t *testing.T) { testReadinessHardRefusal(t, cause) })
	}
}

func testReadinessHardRefusal(t *testing.T, cause error) {
	t.Helper()
	o := &Origin{serving: map[string]serving{}}
	calls := 0
	inspect := func(context.Context) (listenerSocket, error) {
		calls++
		return listenerSocket{}, cause
	}
	err := o.awaitServer(context.Background(), "probe", "session", inspect)
	assertReadinessRefused(t, o, err, cause)
	if calls != 1 {
		t.Fatalf("hard refusal observations = %d; want one", calls)
	}
}

func TestReadinessKeepsCancellationCauseAndObservation(t *testing.T) {
	o := &Origin{serving: map[string]serving{}}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	identity := errors.New("app operation identity changed")
	inspect := func(context.Context) (listenerSocket, error) {
		cancel(identity)
		return listenerSocket{}, errIncompleteListenerObservation
	}
	err := o.awaitServer(ctx, "probe", "session", inspect)
	for _, cause := range []error{context.Canceled, identity, errIncompleteListenerObservation} {
		assertReadinessRefused(t, o, err, cause)
	}
}
