package tui

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/muesli/cancelreader"

	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/terminal"
)

func NewTerminalPicker(input *os.File, output io.Writer, term string, size func() terminal.Size, changes <-chan terminal.Size) cli.PickerFunc {
	return func(ctx context.Context, catalog cli.PickerInput) (cli.PickerSelection, error) {
		if input == nil || output == nil || size == nil {
			return cli.PickerSelection{}, fmt.Errorf("terminal picker needs input, output, and window size")
		}
		pickerContext, cancel := context.WithCancel(ctx)
		defer cancel()
		final, err := runTerminalPicker(pickerContext, newPickerModel(pickerContext, catalog, time.Now()), input, output, term, size(), changes)
		if err != nil {
			return cli.PickerSelection{}, fmt.Errorf("terminal picker: %w", err)
		}
		finished, ok := final.(model)
		if !ok {
			return cli.PickerSelection{}, fmt.Errorf("terminal picker returned model %T", final)
		}
		if finished.selection == nil {
			return cli.PickerSelection{}, nil
		}
		return cliSelection(finished.selection), nil
	}
}

func runTerminalPicker(ctx context.Context, initial tea.Model, input *os.File, output io.Writer, term string, size terminal.Size, changes <-chan terminal.Size) (tea.Model, error) {
	reader, err := cancelreader.NewReader(input)
	if err != nil {
		return nil, fmt.Errorf("prepare terminal picker input: %w", err)
	}
	joined := &terminalPickerInput{reader: reader}
	defer joined.stop()
	ready := make(chan struct{})
	markReady := sync.OnceFunc(func() { close(ready) })
	program := tea.NewProgram(initial,
		tea.WithContext(ctx),
		tea.WithInput(joined),
		tea.WithOutput(struct{ io.Writer }{output}),
		tea.WithEnvironment([]string{"TERM=" + term}),
		tea.WithoutSignals(),
		tea.WithoutSignalHandler(),
		tea.WithWindowSize(size.Cols, size.Rows),
		tea.WithFilter(func(_ tea.Model, message tea.Msg) tea.Msg {
			if _, ok := message.(tea.WindowSizeMsg); ok {
				markReady()
			}
			return message
		}),
	)
	resizeContext, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go forwardTerminalPickerSizes(resizeContext, program, ready, changes, done)
	final, err := program.Run()
	cancel()
	<-done
	return final, err
}

func forwardTerminalPickerSizes(ctx context.Context, program *tea.Program, ready <-chan struct{}, changes <-chan terminal.Size, done chan<- struct{}) {
	defer close(done)
	// Tea sends its initial size asynchronously. Apply queued SSH resizes after
	// that message so startup cannot overwrite a newer size with the old one.
	select {
	case <-ctx.Done():
		return
	case <-ready:
	}
	for {
		size, ok := nextTerminalPickerSize(ctx, changes)
		if !ok {
			return
		}
		program.Send(tea.WindowSizeMsg{Width: size.Cols, Height: size.Rows})
	}
}

func nextTerminalPickerSize(ctx context.Context, changes <-chan terminal.Size) (terminal.Size, bool) {
	select {
	case <-ctx.Done():
		return terminal.Size{}, false
	case size, ok := <-changes:
		return size, ok
	}
}

// Bubble Tea skips its reader join on cancellation. This gate stops and joins
// the active read before a following attachment can reuse the channel's pipe.
type terminalPickerInput struct {
	mu      sync.Mutex
	reader  cancelreader.CancelReader
	stopped bool
}

func (r *terminalPickerInput) Read(data []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return 0, cancelreader.ErrCanceled
	}
	return r.reader.Read(data)
}

func (r *terminalPickerInput) stop() {
	r.reader.Cancel()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopped = true
	_ = r.reader.Close()
}
