package cli

import "context"

// One watcher owns one writer and one pending immutable snapshot. Persistence
// never blocks its live publication, and cancellation joins the writer.
func startPickerCacheWriter(ctx context.Context, save func(context.Context, StateView)) (func(StateView), func()) {
	run, cancel := context.WithCancel(ctx)
	pending := make(chan StateView, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runPickerCacheWriter(run, pending, save)
	}()
	return func(view StateView) { queuePickerCacheView(run, pending, view) }, func() { cancel(); <-done }
}

func runPickerCacheWriter(ctx context.Context, pending <-chan StateView, save func(context.Context, StateView)) {
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return
		case view := <-pending:
			save(ctx, view)
		}
	}
}

func queuePickerCacheView(ctx context.Context, pending chan StateView, view StateView) {
	select {
	case pending <- view:
		return
	default:
	}
	select {
	case <-pending:
	default:
	}
	select {
	case pending <- view:
	case <-ctx.Done():
	}
}
