package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Source streams an archive from a producer. Info contains the immutable name
// and kind before the first read; Finish returns its final digest after EOF.
type Source struct {
	Info Info

	reader   *io.PipeReader
	cancel   context.CancelFunc
	done     chan struct{}
	info     Info
	err      error
	once     sync.Once
	closeErr error
}

// OpenSource opens one file or recursive directory as a streamed archive.
// Closing the source cancels its producer and waits for it to finish.
func OpenSource(ctx context.Context, source string, recursive bool) (*Source, error) {
	if ctx == nil {
		return nil, errors.New("transfer: nil source context")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("transfer source context: %w", err)
	}
	absolute, err := filepath.Abs(source)
	if err != nil {
		return nil, fmt.Errorf("transfer source absolute path: %w", err)
	}
	stat, err := os.Lstat(absolute)
	if err != nil {
		return nil, fmt.Errorf("transfer: open source: %w", err)
	}
	if stat.IsDir() && !recursive {
		return nil, errors.New("transfer: directory source requires recursive")
	}
	if !stat.IsDir() && !stat.Mode().IsRegular() {
		return nil, errors.New("transfer: source must be a regular file or directory")
	}
	info := Info{Name: filepath.Base(absolute), Directory: stat.IsDir()}
	if err := validateInfo(info, false); err != nil {
		return nil, err
	}
	reader, writer := io.Pipe()
	lifetime, cancel := context.WithCancel(ctx)
	sourceStream := &Source{Info: info, reader: reader, cancel: cancel, done: make(chan struct{})}
	go sourceStream.produce(lifetime, absolute, writer)
	return sourceStream, nil
}

func (s *Source) produce(ctx context.Context, path string, writer *io.PipeWriter) {
	stop := context.AfterFunc(ctx, func() { _ = writer.CloseWithError(ctx.Err()) })
	info, err := Pack(ctx, path, writer)
	stop()
	s.info, s.err = info, err
	_ = writer.CloseWithError(err)
	close(s.done)
}

// Read reads the archive stream. The caller should call Finish after EOF to
// obtain its digest and Close on every path to join the producer.
func (s *Source) Read(data []byte) (int, error) {
	return s.reader.Read(data) //nolint:wrapcheck // io.ReadFull requires the reader's exact io.EOF sentinel.
}

// Finish waits for the producer and returns its complete archive metadata.
// It is intended to follow reading EOF; calling it while unread data remains
// requires a concurrent reader.
func (s *Source) Finish() (Info, error) {
	<-s.done
	return s.info, s.err
}

// Close cancels the producer, unblocks either end of its pipe, and joins it.
func (s *Source) Close() error {
	s.once.Do(func() {
		s.cancel()
		s.closeErr = s.reader.CloseWithError(context.Canceled)
		<-s.done
	})
	return s.closeErr
}
