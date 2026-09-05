package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/term"

	"github.com/shaul/mesh/internal/terminal"
)

// AttachTerminal supplies one attachment's streams and terminal events.
// CancelInput must unblock Input.Read without closing a containing connection.
// The caller closes any reader resources after AttachWithTerminal returns.
type AttachTerminal struct {
	Input       io.Reader
	Output      io.Writer
	Size        terminal.Size
	Resizes     <-chan terminal.Size
	CancelInput func()
	// ResetInput discards pending input and restarts a canceled reader. It is
	// required when Conn can reconnect; local and SSH streams do not need it.
	ResetInput func() error
}

// AttachWithTerminal uses an explicit terminal without discovering process
// terminal state or registering nesting from the process environment.
func AttachWithTerminal(ctx context.Context, opts AttachOptions, terminal AttachTerminal) (AttachResult, error) {
	keys := newAttachmentKeys(opts, 0, len(opts.ContainingSessions) > 0, false)
	return attachWithTerminal(ctx, opts, terminal, keys, true)
}

func validateAttachTerminal(ctx context.Context, terminal AttachTerminal) error {
	if ctx == nil {
		return errors.New("attach with nil context")
	}
	if terminal.Input == nil || terminal.Output == nil || terminal.CancelInput == nil {
		return errors.New("attach with incomplete terminal streams or input cancellation")
	}
	if terminal.Size.Cols <= 0 || terminal.Size.Rows <= 0 {
		return errors.New("attach with non-positive terminal dimensions")
	}
	return ctx.Err()
}

func localAttachTerminal(input, output *os.File) (AttachTerminal, func(), bool, error) {
	terminal := AttachTerminal{Input: input, Output: output, Size: localTerminalSize(output), CancelInput: func() {}}
	inputIsTerminal := term.IsTerminal(input.Fd())
	restore := func() {}
	if inputIsTerminal {
		var err error
		restore, err = makeRaw(input)
		if err != nil {
			return AttachTerminal{}, nil, false, err
		}
	}
	cancelReader, err := localAttachInput(input, inputIsTerminal)
	if err != nil {
		restore()
		return AttachTerminal{}, nil, false, err
	}
	terminal.Input = cancelReader.reader
	terminal.CancelInput = cancelReader.cancel
	terminal.ResetInput = cancelReader.reset
	resizes, stopResizes := localTerminalResizes(output)
	terminal.Resizes = resizes
	closeTerminal := func() {
		stopResizes()
		_ = cancelReader.close()
		restore()
	}
	return terminal, closeTerminal, inputIsTerminal, nil
}

type attachmentInput struct {
	reader io.Reader
	cancel func()
	close  func() error
	reset  func() error
}

func localAttachInput(input *os.File, inputIsTerminal bool) (attachmentInput, error) {
	info, err := input.Stat()
	if err != nil {
		return attachmentInput{}, fmt.Errorf("inspect terminal input: %w", err)
	}
	if !inputIsTerminal && info.Mode()&(os.ModeNamedPipe|os.ModeSocket) == 0 {
		return attachmentInput{reader: input, cancel: func() {}, close: func() error { return nil }, reset: func() error {
			_, err := input.Seek(0, io.SeekEnd)
			return err
		}}, nil
	}
	reader, err := uv.NewCancelReader(input)
	if err != nil {
		return attachmentInput{}, fmt.Errorf("make terminal input cancelable: %w", err)
	}
	restartable := &restartableAttachmentInput{file: input, current: reader}
	return attachmentInput{reader: restartable, cancel: restartable.cancel, close: restartable.close, reset: restartable.reset}, nil
}

func localTerminalSize(output *os.File) terminal.Size {
	cols, rows, err := term.GetSize(output.Fd())
	if err != nil || cols <= 0 || rows <= 0 {
		return terminal.Size{Cols: 80, Rows: 24}
	}
	return terminal.Size{Cols: cols, Rows: rows}
}

func localTerminalResizes(output *os.File) (<-chan terminal.Size, func()) {
	winch := make(chan os.Signal, 1)
	resizes := make(chan terminal.Size, 1)
	done := make(chan struct{})
	var relays sync.WaitGroup
	signal.Notify(winch, syscall.SIGWINCH)
	relays.Go(func() {
		relayResizes(done, winch, func() { publishLocalResize(done, resizes, output) })
	})
	stop := func() {
		signal.Stop(winch)
		close(done)
		relays.Wait()
	}
	return resizes, stop
}

func publishLocalResize(done <-chan struct{}, resizes chan<- terminal.Size, output *os.File) {
	cols, rows, err := term.GetSize(output.Fd())
	if err != nil || cols <= 0 || rows <= 0 {
		return
	}
	select {
	case resizes <- terminal.Size{Cols: cols, Rows: rows}:
	case <-done:
	}
}
