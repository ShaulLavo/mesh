//go:build mesh_installed_fixture

package update

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"
	"time"
)

func installedServingRoot(t testing.TB) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}

func installedServingPut(t *testing.T, root *os.Root, proxy *adoptionNativeProxy, put map[string]string) {
	t.Helper()
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	if err := client.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	done := make(chan bool, 1)
	go func() { done <- installedSignConnection(t, root, nil, proxy, server) }()
	if err := json.NewEncoder(client).Encode(installedSignRequest{Put: put}); err != nil {
		t.Fatal(err)
	}
	var reply map[string]bool
	if err := json.NewDecoder(client).Decode(&reply); err != nil || !reply["ok"] || <-done {
		t.Fatalf("prepared put reply: %v, %v", reply, err)
	}
}

func TestPreparedFileHTTPContract(t *testing.T) {
	root := installedServingRoot(t)
	proxy := &adoptionNativeProxy{files: make(map[string][]byte)}
	server := httptest.NewTLSServer(http.HandlerFunc(proxy.serve))
	t.Cleanup(server.Close)
	client := server.Client()
	client.Timeout = 5 * time.Second
	payloads := [][]byte{nil, []byte(`{"immutable":"metadata"}`), []byte("<html>fixture</html>"), bytes.Repeat([]byte{0, 1, 255, 17}, 32768)}
	for index, payload := range payloads {
		name := fmt.Sprintf("asset-%d", index)
		installedWrite(t, root, name, payload)
		proxy.put("memory/"+name, payload)
		installedServingPut(t, root, proxy, map[string]string{"disk/" + name: name})
		assertPreparedHTTPContract(t, client, server.URL, name)
	}
	// Both names are exact aliases, including the moving latest-root URL.
	alias := "/ShaulLavo/mesh/releases/latest/download/mesh-update-root.json"
	installedServingPut(t, root, proxy, map[string]string{alias: "asset-1"})
	assertPreparedHTTPBytes(t, client, server.URL+alias, payloads[1])
	for index := range 8 {
		t.Run(fmt.Sprintf("concurrent-%d", index), func(t *testing.T) {
			t.Parallel()
			assertPreparedHTTPBytes(t, client, server.URL+"/ShaulLavo/mesh/releases/download/disk/asset-3", payloads[3])
		})
	}
}

func assertPreparedHTTPContract(t *testing.T, client *http.Client, address, name string) {
	t.Helper()
	cases := []struct {
		method      string
		rangeHeader string
	}{
		{http.MethodGet, ""}, {http.MethodHead, ""}, {http.MethodPost, ""},
		{http.MethodGet, "bytes=1-3"}, {http.MethodHead, "bytes=1-3"},
		{http.MethodGet, "bytes=999999-"}, {http.MethodGet, "bytes=0-1,3-4"},
	}
	for _, test := range cases {
		memory, memoryBody := preparedHTTPResponse(t, client, address+"/ShaulLavo/mesh/releases/download/memory/"+name, test.method, test.rangeHeader)
		disk, diskBody := preparedHTTPResponse(t, client, address+"/ShaulLavo/mesh/releases/download/disk/"+name, test.method, test.rangeHeader)
		if memory.StatusCode != disk.StatusCode || !reflect.DeepEqual(memory.Header, disk.Header) || !bytes.Equal(memoryBody, diskBody) || !reflect.DeepEqual(memory.TransferEncoding, disk.TransferEncoding) {
			t.Fatalf("%s %s %s differs: memory %d %v %v, disk %d %v %v", name, test.method, test.rangeHeader, memory.StatusCode, memory.Header, memory.TransferEncoding, disk.StatusCode, disk.Header, disk.TransferEncoding)
		}
	}
}

func preparedHTTPResponse(t *testing.T, client *http.Client, address, method, rangeHeader string) (*http.Response, []byte) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), method, address, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Range", rangeHeader)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		t.Fatalf("read bounded fixture response: %v", err)
	}
	response.Header.Del("Date")
	return response, data
}

func assertPreparedHTTPBytes(t *testing.T, client *http.Client, address string, expected []byte) {
	t.Helper()
	response, actual := preparedHTTPResponse(t, client, address, http.MethodGet, "")
	if response.StatusCode != http.StatusOK || !bytes.Equal(actual, expected) {
		t.Fatalf("served fixture bytes changed: status %d, length %d", response.StatusCode, len(actual))
	}
}

func TestPreparedFileRegistration(t *testing.T) {
	root := installedServingRoot(t)
	proxy := &adoptionNativeProxy{files: make(map[string][]byte)}
	installedWrite(t, root, "asset", []byte("immutable"))
	if err := root.Chmod("asset", 0); err != nil {
		t.Fatal(err)
	}
	installedServingPut(t, root, proxy, map[string]string{"registered": "asset"})
	if len(proxy.files) != 0 || proxy.filesOnDisk["registered"].size != 9 {
		t.Fatal("prepared registration materialized file bytes")
	}
	if err := root.Chmod("asset", 0600); err != nil {
		t.Fatal(err)
	}
	if err := root.Mkdir("directory", 0700); err != nil {
		t.Fatal(err)
	}
	if err := root.Symlink("/etc/passwd", "escape"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"missing", "directory", "../outside", "/etc/passwd", "escape"} {
		if err := proxy.putFile(root, "registered", path); err == nil {
			t.Fatalf("accepted unavailable or nonlocal asset %q", path)
		}
	}
	assertPreparedRecordedBytes(t, proxy, "registered", []byte("immutable"))
	proxy.put("registered", []byte("snapshot"))
	assertPreparedRecordedBytes(t, proxy, "registered", []byte("snapshot"))
	installedServingPut(t, root, proxy, map[string]string{"registered": "asset"})
	assertPreparedRecordedBytes(t, proxy, "registered", []byte("immutable"))
	request := httptest.NewRequest(http.MethodGet, "https://github.com/unregistered", nil)
	response := httptest.NewRecorder()
	proxy.serve(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("served an unregistered path: %d", response.Code)
	}
}

func assertPreparedRecordedBytes(t *testing.T, proxy *adoptionNativeProxy, name string, expected []byte) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "https://github.com/ShaulLavo/mesh/releases/download/"+name, nil)
	response := httptest.NewRecorder()
	proxy.serve(response, request)
	if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), expected) {
		t.Fatalf("registered bytes changed: %d %q", response.Code, response.Body.Bytes())
	}
}

func TestPreparedChangedFileShape(t *testing.T) {
	for _, kind := range []string{"short", "directory"} {
		t.Run(kind, func(t *testing.T) { assertPreparedChangedShape(t, kind) })
	}
}

func assertPreparedChangedShape(t *testing.T, kind string) {
	t.Helper()
	root, file := preparedMutationFile(t, []byte("immutable"))
	if err := root.Remove("asset"); err != nil {
		t.Fatal(err)
	}
	if kind == "directory" {
		if err := root.Mkdir("asset", 0700); err != nil {
			t.Fatal(err)
		}
	} else {
		installedWrite(t, root, "asset", []byte("short"))
	}
	response := httptest.NewRecorder()
	file.serve(response, httptest.NewRequest(http.MethodGet, "https://github.com/asset", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("changed file shape returned %d", response.Code)
	}
}

func preparedMutationFile(t *testing.T, content []byte) (*os.Root, adoptionNativeFile) {
	t.Helper()
	root := installedServingRoot(t)
	installedWrite(t, root, "asset", content)
	proxy := &adoptionNativeProxy{files: make(map[string][]byte)}
	if err := proxy.putFile(root, "registered", "asset"); err != nil {
		t.Fatal(err)
	}
	return root, proxy.filesOnDisk["registered"]
}

type preparedTruncatingWriter struct {
	http.ResponseWriter
	root *os.Root
	err  error
}

func (writer *preparedTruncatingWriter) Write(data []byte) (int, error) {
	count, err := writer.ResponseWriter.Write(data)
	writer.err = writer.root.WriteFile("asset", nil, 0600)
	return count, err
}

func TestPreparedCopyFailureAbortsResponse(t *testing.T) {
	root, file := preparedMutationFile(t, bytes.Repeat([]byte("x"), 128<<10))
	writer := &preparedTruncatingWriter{ResponseWriter: httptest.NewRecorder(), root: root}
	defer func() {
		if got := recover(); got != http.ErrAbortHandler {
			t.Fatalf("copy failure did not abort the response: %v", got)
		}
		if writer.err != nil {
			t.Fatal(writer.err)
		}
	}()
	file.serve(writer, httptest.NewRequest(http.MethodGet, "https://github.com/asset", nil))
}

func BenchmarkPreparedFileRegistration(b *testing.B) {
	root := installedServingRoot(b)
	payload := bytes.Repeat([]byte("fixture-"), (4<<20)/8)
	paths := make([]string, 10)
	for index := range paths {
		paths[index] = fmt.Sprintf("asset-%d", index)
		data := payload
		if index >= 8 {
			data = payload[:1<<20]
		}
		if err := root.WriteFile(paths[index], data, 0600); err != nil {
			b.Fatal(err)
		}
	}
	b.Run("snapshot-34MiB", func(b *testing.B) {
		proxy := &adoptionNativeProxy{files: make(map[string][]byte)}
		b.ReportAllocs()
		for range b.N {
			benchmarkPreparedSnapshots(b, root, proxy, paths)
		}
	})
	b.Run("register-34MiB", func(b *testing.B) {
		proxy := &adoptionNativeProxy{files: make(map[string][]byte)}
		b.ReportAllocs()
		for range b.N {
			benchmarkPreparedFiles(b, root, proxy, paths)
		}
	})
}

func benchmarkPreparedSnapshots(b *testing.B, root *os.Root, proxy *adoptionNativeProxy, paths []string) {
	b.Helper()
	for _, path := range paths {
		data, err := root.ReadFile(path)
		if err != nil {
			b.Fatal(err)
		}
		proxy.put(path, data)
	}
}

func benchmarkPreparedFiles(b *testing.B, root *os.Root, proxy *adoptionNativeProxy, paths []string) {
	b.Helper()
	for _, path := range paths {
		if err := proxy.putFile(root, path, path); err != nil {
			b.Fatal(err)
		}
	}
}
