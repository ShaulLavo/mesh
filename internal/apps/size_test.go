package apps

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSizeLimit(t *testing.T) {
	for value, want := range map[string]SizeLimit{"1": 1, "500MB": 500_000_000, "512MiB": 512 << 20, "2GiB": 2 << 30, "1TiB": 1 << 40, "2GB": 2_000_000_000, "unlimited": -1, "0": -1} {
		got, err := ParseSizeLimit(value)
		if err != nil || got != want {
			t.Fatalf("%q: got %d, %v; want %d", value, got, err, want)
		}
	}
	for _, value := range []string{"", "-1", "1.5GiB", "12unknown", "0MiB", "9223372036854775807", "999999999999999GiB"} {
		if _, err := ParseSizeLimit(value); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
}

func TestUploadUsesSavedSizeLimit(t *testing.T) {
	f := newAppFixture(t)
	begun, err := f.origin.Handle(context.Background(), Request{Action: "upload.begin", MaxBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	f.origin.Close()
	restartAppOrigin(t, f)
	t.Cleanup(f.origin.Close)
	_, err = f.origin.Handle(context.Background(), Request{Action: "upload.chunk", UploadID: begun.UploadID, Offset: 1024, Data: []byte("x"), MaxBytes: -1})
	if !errors.Is(err, ErrArchiveTooLarge) {
		t.Fatalf("chunk changed saved limit: %v", err)
	}
	_, err = f.origin.Handle(context.Background(), Request{Action: "upload.chunk", UploadID: begun.UploadID, Offset: 1<<63 - 1, Data: []byte("x")})
	if !errors.Is(err, ErrArchiveTooLarge) {
		t.Fatalf("overflow bypassed limit: %v", err)
	}
}

func TestConfiguredLimitsApplyToExpandedSource(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "data"), bytes.Repeat([]byte("x"), 4096), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Pack(context.Background(), source, io.Discard, 1024); !errors.Is(err, errSourceTooLarge) {
		t.Fatalf("pack accepted expanded source: %v", err)
	}
	archive := filepath.Join(t.TempDir(), "source.tar.gz")
	file, err := os.Create(archive) //nolint:gosec // Archive path belongs to this test's temporary directory.
	if err != nil {
		t.Fatal(err)
	}
	digest, err := Pack(context.Background(), source, file, -1)
	if closeErr := file.Close(); err != nil || closeErr != nil {
		t.Fatal(errors.Join(err, closeErr))
	}
	if err := unpack(archive, t.TempDir(), digest, 1024); !errors.Is(err, errSourceTooLarge) {
		t.Fatalf("unpack accepted expanded source: %v", err)
	}
	if err := unpack(archive, t.TempDir(), digest, -1); err != nil {
		t.Fatalf("unlimited unpack: %v", err)
	}
}

type countedResponse struct {
	header http.Header
	status int
	bytes  int64
}

func (w *countedResponse) Header() http.Header    { return w.header }
func (w *countedResponse) WriteHeader(status int) { w.status = status }
func (w *countedResponse) Write(data []byte) (int, error) {
	w.bytes += int64(len(data))
	return len(data), nil
}

func TestBrowserDownloadStreamsPastDefaultLimit(t *testing.T) {
	f := newAppFixture(t)
	app := createStaticApp(t, f)
	owner := pairedOwner(t, f)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		data := make([]byte, 1<<20)
		for range 65 {
			if _, err := w.Write(data); err != nil {
				return
			}
		}
	}))
	defer origin.Close()
	endpoint := netip.MustParseAddrPort(strings.TrimPrefix(origin.URL, "http://"))
	f.edge.config.Resolve = func(context.Context, string) (netip.AddrPort, error) { return endpoint, nil }
	request := httptest.NewRequest(http.MethodGet, ManagementOrigin()+"/download?id="+app.ID, nil)
	request.AddCookie(owner)
	response := &countedResponse{header: http.Header{}}
	f.edge.downloadSource(response, request, app)
	if response.status != 0 || response.bytes != 65<<20 {
		t.Fatalf("browser download: status=%d bytes=%d", response.status, response.bytes)
	}
}
