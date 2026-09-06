package sshfs_test

import (
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/pkg/sftp"
	"github.com/shaul/mesh/internal/serve"
	gossh "golang.org/x/crypto/ssh"
)

func TestSFTPFstatDistinguishesReplacedPathHandles(t *testing.T) {
	f := newFixture(t)
	original := openHandle(t, f.client, "/files/nested/value.txt")
	local := filepath.Join(f.root, "nested", "value.txt")
	if err := os.Rename(local, local+".old"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, local, "x")
	replacement := openHandle(t, f.client, "/files/nested/value.txt")

	assertHandle(t, original, "download bytes\x00\xff\n")
	assertHandle(t, replacement, "x")
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}
	assertHandle(t, replacement, "x")
}

func TestSFTPFstatSurvivesUnlinkedPath(t *testing.T) {
	f := newFixture(t)
	handle := openHandle(t, f.client, "/files/nested/value.txt")
	if err := os.Remove(filepath.Join(f.root, "nested", "value.txt")); err != nil {
		t.Fatal(err)
	}
	assertHandle(t, handle, "download bytes\x00\xff\n")
}

func TestSFTPFstatReportsCurrentDescriptorMetadata(t *testing.T) {
	f := newFixture(t)
	handle := openHandle(t, f.client, "/files/nested/value.txt")
	writeFile(t, filepath.Join(f.root, "nested", "value.txt"), "updated bytes")
	assertHandle(t, handle, "updated bytes")
}

func TestSFTPFstatSurvivesServiceReplacement(t *testing.T) {
	f := newFixture(t)
	handle := openHandle(t, f.client, "/files/nested/value.txt")
	if err := f.registry.Replace(nil); err != nil {
		t.Fatal(err)
	}
	assertHandle(t, handle, "download bytes\x00\xff\n")

	replacementRoot := t.TempDir()
	writeFile(t, filepath.Join(replacementRoot, "value.txt"), "new root")
	if err := f.registry.Replace([]serve.Service{{Name: "files/nested", Kind: serve.Files, Target: replacementRoot}}); err != nil {
		t.Fatal(err)
	}
	replacement := openHandle(t, f.client, "/files/nested/value.txt")
	assertHandle(t, handle, "download bytes\x00\xff\n")
	assertHandle(t, replacement, "new root")
}

func TestSFTPDirectoryFstatSurvivesRenameAndCloses(t *testing.T) {
	f := newFixture(t)
	client := newRawHandleClient(t, f.registry)
	response := client.request(t, 11, "/files/nested")
	var opened struct {
		Type   byte
		ID     uint32
		Handle string
	}
	if err := gossh.Unmarshal(response, &opened); err != nil || opened.Type != 102 {
		t.Fatalf("OPENDIR response = %x, error %v", response, err)
	}
	local := filepath.Join(f.root, "nested")
	if err := os.Rename(local, local+".old"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, local, "replacement regular file")
	if err := os.Chmod(local+".old", 0o750); err != nil { //nolint:gosec // verify FSTAT masks write bits while preserving group access
		t.Fatal(err)
	}
	response = client.request(t, 8, opened.Handle)
	var attrs struct {
		Type  byte
		ID    uint32
		Flags uint32
		Size  uint64
		Mode  uint32
		Atime uint32
		Mtime uint32
	}
	if err := gossh.Unmarshal(response, &attrs); err != nil || attrs.Type != 105 {
		t.Fatalf("FSTAT response = %x, error %v", response, err)
	}
	if attrs.Flags != 0x0d || attrs.Mode != 0o40550 {
		t.Fatalf("FSTAT flags = %#x, mode = %#o; want directory mode 040550 without host ownership", attrs.Flags, attrs.Mode)
	}
	response = client.request(t, 4, opened.Handle)
	if len(response) < 9 || response[0] != 101 || binary.BigEndian.Uint32(response[5:9]) != 0 {
		t.Fatalf("CLOSE response = %x; want successful status", response)
	}
	response = client.request(t, 8, opened.Handle)
	if len(response) < 9 || response[0] != 101 || binary.BigEndian.Uint32(response[5:9]) == 0 {
		t.Fatalf("FSTAT after CLOSE response = %x; want invalid handle status", response)
	}
}

func TestSFTPIntermediateDirectoryFstatMatchesVirtualMetadata(t *testing.T) {
	f := newFixture(t)
	if err := f.registry.Replace([]serve.Service{
		{Name: "files", Kind: serve.Files, Target: f.root},
		{Name: "files/nested/mounted", Kind: serve.Files, Target: t.TempDir()},
	}); err != nil {
		t.Fatal(err)
	}
	client := newRawHandleClient(t, f.registry)
	response := client.request(t, 11, "/files/nested")
	var opened struct {
		Type   byte
		ID     uint32
		Handle string
	}
	if err := gossh.Unmarshal(response, &opened); err != nil || opened.Type != 102 {
		t.Fatalf("OPENDIR response = %x, error %v", response, err)
	}
	response = client.request(t, 8, opened.Handle)
	if len(response) != 29 || response[0] != 105 || binary.BigEndian.Uint32(response[5:9]) != 0x0d {
		t.Fatalf("FSTAT response = %x; want attributes without host ownership", response)
	}
	size := binary.BigEndian.Uint64(response[9:17])
	mode := binary.BigEndian.Uint32(response[17:21])
	if size != 0 || mode != 0o40555 {
		t.Fatalf("FSTAT size = %d, mode = %#o; want virtual directory size 0 and mode 040555", size, mode)
	}
	info, err := f.client.Stat("/files/nested")
	if err != nil || info.Size() != 0 || info.Mode().Perm() != 0o555 {
		t.Fatalf("STAT = %v, error %v; want matching virtual directory metadata", info, err)
	}
}

type rawHandleClient struct {
	io.Reader
	io.Writer
	nextID uint32
}

func newRawHandleClient(t *testing.T, registry *serve.Registry) *rawHandleClient {
	t.Helper()
	session, err := newSSHClient(t, registry).NewSession()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	input, err := session.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := session.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := session.RequestSubsystem("sftp"); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Write([]byte{0, 0, 0, 5, 1, 0, 0, 0, 3}); err != nil {
		t.Fatal(err)
	}
	client := &rawHandleClient{Reader: output, Writer: input}
	if response := client.response(t); len(response) < 5 || response[0] != 2 {
		t.Fatalf("INIT response = %x; want protocol version", response)
	}
	return client
}

func (client *rawHandleClient) request(t *testing.T, operation byte, value string) []byte {
	t.Helper()
	client.nextID++
	payload := gossh.Marshal(struct {
		Type  byte
		ID    uint32
		Value string
	}{operation, client.nextID, value})
	packet := binary.BigEndian.AppendUint32(nil, uint32(len(payload))) //nolint:gosec // test packets contain bounded fixture paths and handles
	if _, err := client.Write(append(packet, payload...)); err != nil {
		t.Fatal(err)
	}
	response := client.response(t)
	if len(response) < 5 || binary.BigEndian.Uint32(response[1:5]) != client.nextID {
		t.Fatalf("response = %x; want request ID %d", response, client.nextID)
	}
	return response
}

func (client *rawHandleClient) response(t *testing.T) []byte {
	t.Helper()
	var length uint32
	if err := binary.Read(client, binary.BigEndian, &length); err != nil {
		t.Fatal(err)
	}
	if length > 256<<10 {
		t.Fatalf("oversized response: %d bytes", length)
	}
	response := make([]byte, length)
	if _, err := io.ReadFull(client, response); err != nil {
		t.Fatal(err)
	}
	return response
}

func openHandle(t *testing.T, client *sftp.Client, name string) *sftp.File {
	t.Helper()
	handle, err := client.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handle.Close() })
	return handle
}

func assertHandle(t *testing.T, handle *sftp.File, content string) {
	t.Helper()
	info, err := handle.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != int64(len(content)) {
		t.Errorf("FSTAT size = %d; want %d", info.Size(), len(content))
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o222 != 0 {
		t.Errorf("FSTAT mode = %v; want read-only regular file", info.Mode())
	}
	buffer := make([]byte, len(content)+1)
	n, err := handle.ReadAt(buffer, 0)
	if err != io.EOF {
		t.Fatalf("ReadAt error = %v; want EOF", err)
	}
	if string(buffer[:n]) != content {
		t.Errorf("ReadAt = %q; want %q", buffer[:n], content)
	}
}
