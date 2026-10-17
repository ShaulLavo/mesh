// Package transfer implements Mesh's short lived, per-connection file copy
// state. Transfers are streamed and disappear when their connection closes.
package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
)

// Request is the JSON payload carried by a copy.request control message.
type Request struct {
	Action    string `json:"action"`
	Path      string `json:"path,omitempty"`
	Recursive bool   `json:"recursive,omitempty"`
	Info      Info   `json:"info,omitempty"`
	Size      int64  `json:"size,omitempty"`
	Offset    int64  `json:"offset,omitempty"`
	Data      []byte `json:"data,omitempty"`
}

// Result is the JSON payload returned by a copy.result control message.
type Result struct {
	Info Info   `json:"info,omitempty"`
	Size int64  `json:"size,omitempty"`
	Data []byte `json:"data,omitempty"`
	Done bool   `json:"done,omitempty"`
}

type operation struct {
	action string
	file   *os.File
	source *Source
	path   string
	info   Info
	offset int64
}

// Store owns one upload or download at a time for one client connection.
type Store struct {
	mu     sync.Mutex
	active *operation
	closed bool
}

// NewStore creates an empty per-connection store. It has no persistent spool;
// uploads use an unlinked destination-adjacent temporary file and downloads
// stream directly from a bounded pipe.
func NewStore() *Store { return &Store{} }

// Handle executes a single transfer request. Calls are serialized by Store so
// callers may safely use one store from a connection's request loop.
func (s *Store) Handle(ctx context.Context, request Request) (result Result, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() {
		if err != nil {
			s.clearLocked()
		}
	}()
	if ctx == nil {
		return Result{}, errors.New("transfer: nil context")
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if s.closed {
		return Result{}, errors.New("transfer: store closed")
	}
	switch request.Action {
	case "upload.begin":
		return s.uploadBeginLocked(request)
	case "upload.chunk":
		return s.uploadChunkLocked(request)
	case "upload.commit":
		return s.uploadCommitLocked(ctx, request)
	case "download.begin":
		return s.downloadBeginLocked(ctx, request)
	case "download.chunk":
		return s.downloadChunkLocked(request)
	case "close", "release":
		s.clearLocked()
		return Result{Done: true}, nil
	default:
		return Result{}, fmt.Errorf("transfer: unknown action %q", request.Action)
	}
}

func (s *Store) uploadBeginLocked(request Request) (Result, error) {
	if s.active != nil {
		return Result{}, errors.New("transfer: another operation is active")
	}
	if err := validateInfo(request.Info, false); err != nil {
		return Result{}, err
	}
	dest, err := absolutePath(request.Path)
	if err != nil {
		return Result{}, err
	}
	tmp, err := Temporary(dest)
	if err != nil {
		return Result{}, fmt.Errorf("transfer: create upload staging file: %w", err)
	}
	s.active = &operation{action: "upload", file: tmp, path: dest, info: request.Info}
	return Result{Info: request.Info}, nil
}

func (s *Store) uploadChunkLocked(request Request) (Result, error) {
	op, err := s.require("upload")
	if err != nil {
		return Result{}, err
	}
	if request.Offset != op.offset {
		return Result{}, fmt.Errorf("transfer: upload offset %d, expected %d", request.Offset, op.offset)
	}
	if len(request.Data) == 0 || len(request.Data) > ChunkSize {
		return Result{}, fmt.Errorf("transfer: upload chunk must contain 1 to %d bytes", ChunkSize)
	}
	if int64(len(request.Data)) > math.MaxInt64-op.offset {
		return Result{}, errors.New("transfer: upload size overflows")
	}
	n, err := op.file.Write(request.Data)
	if err != nil {
		return Result{}, fmt.Errorf("transfer: stage upload chunk: %w", err)
	}
	if n != len(request.Data) {
		return Result{}, io.ErrShortWrite
	}
	op.offset += int64(n)
	return Result{Size: op.offset, Done: false}, nil
}

func (s *Store) uploadCommitLocked(ctx context.Context, request Request) (Result, error) {
	op, err := s.require("upload")
	if err != nil {
		return Result{}, err
	}
	if request.Size < 0 || request.Size != op.offset {
		return Result{}, fmt.Errorf("transfer: upload size %d, received %d", request.Size, op.offset)
	}
	if err := validateInfo(request.Info, true); err != nil {
		return Result{}, err
	}
	if request.Info.Name != op.info.Name || request.Info.Directory != op.info.Directory {
		return Result{}, errors.New("transfer: upload metadata changed during commit")
	}
	if err := op.file.Sync(); err != nil {
		return Result{}, fmt.Errorf("transfer: sync upload: %w", err)
	}
	if _, err := op.file.Seek(0, io.SeekStart); err != nil {
		return Result{}, err
	}
	if err := Receive(ctx, op.file, request.Info, op.path); err != nil {
		return Result{}, err
	}
	result := Result{Info: request.Info, Size: request.Size, Done: true}
	s.clearLocked()
	return result, nil
}

func (s *Store) downloadBeginLocked(ctx context.Context, request Request) (Result, error) {
	if s.active != nil {
		return Result{}, errors.New("transfer: another operation is active")
	}
	source, err := absolutePath(request.Path)
	if err != nil {
		return Result{}, err
	}
	stream, err := OpenSource(ctx, source, request.Recursive)
	if err != nil {
		return Result{}, err
	}
	s.active = &operation{action: "download", source: stream, path: source, info: stream.Info}
	return Result{Info: stream.Info}, nil
}

func (s *Store) downloadChunkLocked(request Request) (Result, error) {
	op, err := s.require("download")
	if err != nil {
		return Result{}, err
	}
	if request.Offset != op.offset {
		return Result{}, fmt.Errorf("transfer: download offset %d, expected %d", request.Offset, op.offset)
	}
	if request.Offset < 0 {
		return Result{}, errors.New("transfer: negative download offset")
	}
	data := make([]byte, ChunkSize)
	n, readErr := io.ReadFull(op.source, data)
	if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
		return Result{}, readErr
	}
	data = data[:n]
	if int64(n) > math.MaxInt64-op.offset {
		return Result{}, errors.New("transfer: download size overflows")
	}
	op.offset += int64(n)
	if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
		info, finishErr := op.source.Finish()
		if finishErr != nil {
			return Result{}, finishErr
		}
		op.info = info
		result := Result{Info: info, Size: op.offset, Data: data, Done: true}
		s.clearLocked()
		return result, nil
	}
	return Result{Info: op.info, Size: op.offset, Data: data}, nil
}

func (s *Store) require(action string) (*operation, error) {
	if s.active == nil {
		return nil, errors.New("transfer: no active operation")
	}
	if s.active.action != action {
		return nil, fmt.Errorf("transfer: active operation is %s", s.active.action)
	}
	return s.active, nil
}

func (s *Store) clearLocked() {
	if s.active == nil {
		return
	}
	op := s.active
	s.active = nil
	if op.file != nil {
		_ = op.file.Close()
	}
	if op.source != nil {
		_ = op.source.Close()
	}
}

// Close releases all state and cancels any producer. It is idempotent.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.clearLocked()
	s.closed = true
	return nil
}

func validateInfo(info Info, requireDigest bool) error {
	if len(info.Name) > 255 || !validPeerName(info.Name) {
		return errors.New("transfer: invalid transfer name")
	}
	if !requireDigest {
		return nil
	}
	if !validDigest(info.Digest) {
		return errors.New("transfer: invalid transfer digest")
	}
	return nil
}

func absolutePath(path string) (string, error) {
	if path == "" || !filepath.IsAbs(path) {
		return "", errors.New("transfer: path must be absolute")
	}
	clean := filepath.Clean(path)
	if clean == string(filepath.Separator) {
		return "", errors.New("transfer: refusing filesystem root")
	}
	return clean, nil
}
