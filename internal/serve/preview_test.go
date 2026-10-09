package serve

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInspectServiceResolvesRemoteHomeAndInfersKind(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "site")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	preview, err := InspectService(context.Background(), home, Service{
		Name: "blog", Target: "./site",
	})
	if err != nil {
		t.Fatal(err)
	}
	if preview.Service.Kind != Static || preview.Service.Target != root {
		t.Fatalf("directory preview = %#v", preview)
	}

	preview, err = InspectService(context.Background(), home, Service{Name: "api", Target: "03000"})
	if err != nil {
		t.Fatal(err)
	}
	if preview.Service.Kind != Proxy || preview.Service.Target != "3000" {
		t.Fatalf("proxy preview = %#v", preview)
	}
}

func TestInspectServiceRejectsMeaninglessFlags(t *testing.T) {
	home := t.TempDir()
	for _, test := range []struct {
		name    string
		service Service
		allow   bool
		want    string
	}{
		{name: "files numeric", service: Service{Name: "files", Kind: Files, Target: "3000"}, want: "numeric"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := InspectService(context.Background(), home, test.service)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("InspectService error = %v, want %q", err, test.want)
			}
		})
	}
}
