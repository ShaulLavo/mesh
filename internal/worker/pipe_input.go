package worker

import (
	"fmt"
	"os"
	"sync"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/session/execution"
)

const pipeInputReceipts = 4096
const pipeInputFrames = 1024

type pipeInput struct {
	mu           sync.Mutex
	file         *os.File
	attempt      *execution.Attempt
	queue        chan []byte
	done         chan struct{}
	stopped      chan struct{}
	stopOnce     sync.Once
	offset       uint64
	buffered     int
	capacity     int
	lineMode     bool
	receiptLimit int
	closed       bool
	uncertain    bool
	receipts     map[string]inputAdmission
}

type inputAdmission struct {
	receipt session.InputReceipt
	digest  string
}

func newPipeInput(file *os.File, attempt *execution.Attempt, capacity int, lineMode bool) *pipeInput {
	input := &pipeInput{file: file, attempt: attempt, capacity: capacity, lineMode: lineMode, receiptLimit: pipeInputReceipts, queue: make(chan []byte, pipeInputFrames), done: make(chan struct{}), stopped: make(chan struct{}), receipts: make(map[string]inputAdmission)}
	go input.write()
	return input
}

func (p *pipeInput) admit(receipt session.InputReceipt, data []byte) (*session.InputReceipt, error) {
	if err := p.validateLineOffset(receipt); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if previous, ok := p.receipts[receipt.WriteKey]; ok {
		if previous.receipt != receipt || previous.digest != execution.InputDigest(data) {
			return nil, execution.ErrInputConflict
		}
		return &receipt, nil
	}
	if err := p.validateNewWrite(receipt, len(data)); err != nil {
		return nil, err
	}
	return p.record(receipt, data)
}

func (p *pipeInput) validateNewWrite(receipt session.InputReceipt, size int) error {
	if p.uncertain {
		return execution.ErrInputUnknown
	}
	if p.closed {
		return execution.ErrInputClosed
	}
	select {
	case <-p.stopped:
		return execution.ErrInputClosed
	default:
	}
	if receipt.Offset != p.offset {
		if p.lineMode && receipt.Offset < p.offset {
			return execution.ErrInputUnknown
		}
		return execution.ErrInputOffset
	}
	if len(p.receipts) >= p.receiptLimit && (!receipt.Closed || size != 0) {
		return execution.ErrInputLimit
	}
	if size > p.capacity-p.buffered || (size != 0 && len(p.queue) == cap(p.queue)) {
		return execution.ErrInputFull
	}
	return nil
}

func (p *pipeInput) validateLineOffset(receipt session.InputReceipt) error {
	if !p.lineMode {
		return nil
	}
	offset, err := protocol.LineWriteOffset(receipt.WriteKey)
	if err != nil {
		return workerExecutionError("read framed input key", err)
	}
	if offset != receipt.Offset {
		return execution.ErrInputConflict
	}
	return nil
}

func (p *pipeInput) retireLine(key string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	offset, err := protocol.LineWriteOffset(key)
	if err != nil {
		return workerExecutionError("read framed receipt key", err)
	}
	if offset >= p.offset {
		return execution.ErrInputUnknown
	}
	if _, ok := p.receipts[key]; !ok {
		return nil
	}
	if err := p.attempt.RetireLineInput(key); err != nil {
		return workerExecutionError("retire framed receipt", err)
	}
	delete(p.receipts, key)
	return nil
}

func (p *pipeInput) record(receipt session.InputReceipt, data []byte) (*session.InputReceipt, error) {
	digest := execution.InputDigest(data)
	stage, err := p.attempt.RecordInput(receipt, digest)
	if err != nil {
		p.uncertain = true
		// A visible receipt can be confirmed by a recovering reader. Keep
		// that admission even when the original durability check failed.
		if stage == execution.Unconfirmed {
			p.enqueue(receipt, data)
		}
		return nil, fmt.Errorf("record pipe input: %w", err)
	}
	p.receipts[receipt.WriteKey] = inputAdmission{receipt, digest}
	p.enqueue(receipt, data)
	return &receipt, nil
}

func (p *pipeInput) enqueue(receipt session.InputReceipt, data []byte) {
	p.offset += uint64(len(data))
	p.buffered += len(data)
	p.closed = receipt.Closed
	if len(data) > 0 {
		p.queue <- data
	}
	if receipt.Closed {
		close(p.queue)
	}
}

func (p *pipeInput) nextOffset() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.offset
}

func (p *pipeInput) admissionUncertain() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.uncertain
}

func (p *pipeInput) lookup(key string) (*session.InputReceipt, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if admission, ok := p.receipts[key]; ok {
		return &admission.receipt, nil
	}
	return nil, execution.ErrInputUnknown
}

func (p *pipeInput) write() {
	defer close(p.done)
	defer func() {
		_ = p.file.Close()
		p.mu.Lock()
		p.closed = true
		p.mu.Unlock()
	}()
	for {
		select {
		case <-p.stopped:
			return
		case data, ok := <-p.queue:
			if !ok || p.writeChunk(data) != nil {
				return
			}
		}
	}
}

func (p *pipeInput) writeChunk(data []byte) error {
	_, err := p.file.Write(data)
	p.mu.Lock()
	p.buffered -= len(data)
	p.mu.Unlock()
	return workerExecutionError("write child stdin", err)
}

func (p *pipeInput) stopWriting() {
	p.stopOnce.Do(func() {
		close(p.stopped)
		_ = p.file.Close()
	})
}

func (p *pipeInput) stop() {
	p.stopWriting()
	<-p.done
}
