package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/muesli/cancelreader"

	"github.com/shaul/mesh/internal/protocol"
)

type attachmentInputRelay struct {
	terminal AttachTerminal
	done     <-chan struct{}
	// A reset must also discard bytes already read but not yet sent.
	pipeline  sync.Mutex
	stateMu   sync.Mutex
	resetting chan struct{}
}

func (r *attachmentInputRelay) reset() error {
	if r.terminal.ResetInput == nil {
		return errors.New("terminal input cannot discard buffered data after reconnect")
	}
	resetDone := make(chan struct{})
	r.stateMu.Lock()
	r.resetting = resetDone
	r.stateMu.Unlock()
	r.terminal.CancelInput()
	r.pipeline.Lock()
	defer r.pipeline.Unlock()
	err := r.terminal.ResetInput()
	r.stateMu.Lock()
	r.resetting = nil
	close(resetDone)
	r.stateMu.Unlock()
	return err
}

func (r *attachmentInputRelay) pendingReset() <-chan struct{} {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	return r.resetting
}

func relayInput(r *attachmentInputRelay, keys *attachmentKeys, sid protocol.SessionID, send func(protocol.Frame) error, detach func()) {
	buf := make([]byte, 4096)
	for {
		resetDone, detached, err := r.readAndSend(keys, buf, sid, send)
		if resetDone != nil {
			<-resetDone
			continue
		}
		if detached {
			detach()
			return
		}
		if err != nil {
			return
		}
	}
}

func (r *attachmentInputRelay) readAndSend(keys *attachmentKeys, buf []byte, sid protocol.SessionID, send func(protocol.Frame) error) (<-chan struct{}, bool, error) {
	r.pipeline.Lock()
	defer r.pipeline.Unlock()
	select {
	case <-r.done:
		return nil, false, io.EOF
	default:
	}
	if resetDone := r.pendingReset(); resetDone != nil {
		return resetDone, false, nil
	}
	n, err := r.terminal.Input.Read(buf)
	if resetDone := r.pendingReset(); resetDone != nil {
		return resetDone, false, nil
	}
	return nil, relayInputBytes(keys, buf[:n], sid, send), err
}

type restartableAttachmentInput struct {
	file    *os.File
	mu      sync.Mutex
	current cancelreader.CancelReader
}

func (r *restartableAttachmentInput) Read(buf []byte) (int, error) {
	r.mu.Lock()
	reader := r.current
	r.mu.Unlock()
	if reader == nil {
		return 0, cancelreader.ErrCanceled
	}
	return reader.Read(buf)
}

func (r *restartableAttachmentInput) cancel() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current != nil {
		r.current.Cancel()
	}
}

func (r *restartableAttachmentInput) close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current == nil {
		return nil
	}
	err := r.current.Close()
	r.current = nil
	return err
}

func (r *restartableAttachmentInput) reset() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current == nil {
		return errors.New("terminal input is closed")
	}
	err := r.current.Close()
	r.current = nil
	if err != nil {
		return fmt.Errorf("close canceled terminal reader: %w", err)
	}
	if err := discardPendingInput(r.file); err != nil {
		return fmt.Errorf("discard buffered terminal input: %w", err)
	}
	r.current, err = uv.NewCancelReader(r.file)
	return err
}
