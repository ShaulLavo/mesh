package sftp

import (
	"context"
	"io"
	"net"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequestFstatUsesOpenFileMetadata(t *testing.T) {
	for name, flags := range map[string]int{
		"reader":        os.O_RDONLY,
		"writer":        os.O_WRONLY,
		"reader-writer": os.O_RDWR,
	} {
		t.Run(name, func(t *testing.T) { checkRequestFstatFile(t, flags) })
	}
}

func checkRequestFstatFile(t *testing.T, flags int) {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "original")
	require.NoError(t, err)
	t.Cleanup(func() { _ = file.Close() })
	_, err = file.WriteString("open descriptor")
	require.NoError(t, err)
	client := statHandleClient(t, file)
	handle, err := client.OpenFile("/unresolvable", flags)
	require.NoError(t, err)
	info, err := handle.Stat()
	require.NoError(t, err)
	assert.Equal(t, int64(len("open descriptor")), info.Size())
	require.NoError(t, file.Truncate(4))
	info, err = handle.Stat()
	require.NoError(t, err)
	assert.Equal(t, int64(4), info.Size())
	require.NoError(t, handle.Close())
}

func TestRequestFstatUsesOpenDirectoryMetadata(t *testing.T) {
	directory := t.TempDir()
	file, err := os.Open(directory)
	require.NoError(t, err)
	t.Cleanup(func() { _ = file.Close() })
	client := statHandleClient(t, file)
	handle, err := client.opendir(context.Background(), "/unresolvable")
	require.NoError(t, err)
	info, err := client.fstat(handle)
	require.NoError(t, err)
	assert.True(t, info.FileMode().IsDir())
	want, err := os.Stat(directory)
	require.NoError(t, err)
	assert.Equal(t, uint64(want.Size()), info.Size)
	require.NoError(t, client.close(handle))
}

func TestRequestFstatReturnsOpenHandleStatError(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "original")
	require.NoError(t, err)
	t.Cleanup(func() { _ = file.Close() })
	client := statHandleClient(t, file)
	handle, err := client.Open("/unresolvable")
	require.NoError(t, err)
	require.NoError(t, file.Close())
	_, err = handle.Stat()
	var status *StatusError
	require.ErrorAs(t, err, &status)
	assert.Equal(t, uint32(sshFxFailure), status.Code)
}

func statHandleClient(t *testing.T, file *os.File) *Client {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	handler := statHandleHandler{file: file}
	server := NewRequestServer(serverConn, Handlers{
		FileGet: handler, FilePut: handler, FileList: handler,
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.Serve()
	}()
	t.Cleanup(func() {
		_ = server.Close()
		_ = clientConn.Close()
		<-done
	})
	client, err := NewClientPipe(clientConn, clientConn)
	require.NoError(t, err)
	return client
}

type statHandleHandler struct{ file *os.File }

func (h statHandleHandler) Fileread(*Request) (io.ReaderAt, error)      { return h.file, nil }
func (h statHandleHandler) Filewrite(*Request) (io.WriterAt, error)     { return h.file, nil }
func (h statHandleHandler) OpenFile(*Request) (WriterAtReaderAt, error) { return h.file, nil }

func (h statHandleHandler) Filelist(r *Request) (ListerAt, error) {
	if r.Method != "List" {
		return nil, os.ErrNotExist
	}
	return statDirectoryHandle{File: h.file}, nil
}

type statDirectoryHandle struct{ *os.File }

func (statDirectoryHandle) ListAt([]os.FileInfo, int64) (int, error) { return 0, io.EOF }
