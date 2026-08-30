package edge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRuntimeConfigValidatesListenerModeAndAllowlist(t *testing.T) {
	originID, _ := testIdentity(t)
	renewerID, _ := testIdentity(t)
	origin := testOriginConfig(originID)

	proxy := writeConfigFile(t, runtimeConfigFile{Mode: ModeProxy, Origins: []OriginConfig{origin}})
	loaded, err := LoadRuntimeConfig(proxy)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ListenAddress != DefaultPublicListenAddress || loaded.Mode != ModeProxy {
		t.Fatalf("proxy defaults = %#v", loaded)
	}

	direct := writeConfigFile(t, runtimeConfigFile{
		Mode: ModeDirectTLS, ListenAddress: ":8443", CertificateRenewerID: renewerID,
		Origins: []OriginConfig{origin},
	})
	loaded, err = LoadRuntimeConfig(direct)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ListenAddress != ":8443" || loaded.CertificateRenewerID != renewerID {
		t.Fatalf("direct config = %#v", loaded)
	}

	cases := map[string]runtimeConfigFile{
		"proxy public bind": {
			Mode: ModeProxy, ListenAddress: "0.0.0.0:8080", Origins: []OriginConfig{origin},
		},
		"proxy certificate": {
			Mode: ModeProxy, CertificateRenewerID: renewerID, Origins: []OriginConfig{origin},
		},
		"direct no pin": {
			Mode: ModeDirectTLS, Origins: []OriginConfig{origin},
		},
		"direct loopback": {
			Mode: ModeDirectTLS, ListenAddress: "127.0.0.1:443", CertificateRenewerID: renewerID, Origins: []OriginConfig{origin},
		},
		"direct private": {
			Mode: ModeDirectTLS, ListenAddress: "192.168.1.2:443", CertificateRenewerID: renewerID, Origins: []OriginConfig{origin},
		},
		"direct link local": {
			Mode: ModeDirectTLS, ListenAddress: "169.254.1.2:443", CertificateRenewerID: renewerID, Origins: []OriginConfig{origin},
		},
		"direct Tailscale v4": {
			Mode: ModeDirectTLS, ListenAddress: "100.64.0.2:443", CertificateRenewerID: renewerID, Origins: []OriginConfig{origin},
		},
		"direct Tailscale v6": {
			Mode: ModeDirectTLS, ListenAddress: "[fd7a:115c:a1e0::2]:443", CertificateRenewerID: renewerID, Origins: []OriginConfig{origin},
		},
		"direct mapped Tailscale": {
			Mode: ModeDirectTLS, ListenAddress: "[::ffff:100.64.0.2]:443", CertificateRenewerID: renewerID, Origins: []OriginConfig{origin},
		},
		"noncanonical port": {
			Mode: ModeProxy, ListenAddress: "127.0.0.1:080", Origins: []OriginConfig{origin},
		},
		"duplicate identity": {
			Mode: ModeProxy, Origins: []OriginConfig{origin, origin},
		},
	}
	for name, candidate := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadRuntimeConfig(writeConfigFile(t, candidate)); err == nil {
				t.Fatal("invalid runtime config loaded")
			}
		})
	}
}

func TestLoadConfigRejectsNoncanonicalAndLooseJSON(t *testing.T) {
	originID, _ := testIdentity(t)
	runtime := writeConfigFile(t, runtimeConfigFile{Mode: ModeProxy, Origins: []OriginConfig{testOriginConfig(originID)}})
	unclean := filepath.Dir(runtime) + string(filepath.Separator) + ".." + string(filepath.Separator) + filepath.Base(filepath.Dir(runtime)) + string(filepath.Separator) + filepath.Base(runtime)
	if _, err := LoadRuntimeConfig(unclean); err == nil {
		t.Fatal("noncanonical path loaded")
	}
	unknown := filepath.Join(t.TempDir(), "unknown.json")
	if err := os.WriteFile(unknown, []byte(`{"mode":"proxy","origins":[],"unknown":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRuntimeConfig(unknown); err == nil {
		t.Fatal("unknown field loaded")
	}
	trailing := filepath.Join(t.TempDir(), "trailing.json")
	if err := os.WriteFile(trailing, []byte(`{"mode":"proxy"} {}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRuntimeConfig(trailing); err == nil {
		t.Fatal("multiple JSON values loaded")
	}
	oversized := filepath.Join(t.TempDir(), "oversized.json")
	if err := os.WriteFile(oversized, []byte(strings.Repeat("x", maximumConfigBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRuntimeConfig(oversized); err == nil {
		t.Fatal("oversized config loaded")
	}
}

func TestLoadTargetConfig(t *testing.T) {
	targetID, _ := testIdentity(t)
	target := TargetConfig{Identity: targetID, TailscaleName: "edge.example.ts.net", ControlPort: 7337, WebSocketPath: "/control/ws"}
	loaded, err := LoadTargetConfig(writeConfigFile(t, target))
	if err != nil {
		t.Fatal(err)
	}
	if loaded != target {
		t.Fatalf("target = %#v, want %#v", loaded, target)
	}
	for name, mutate := range map[string]func(*TargetConfig){
		"path traversal": func(value *TargetConfig) { value.WebSocketPath = "/a/../control" },
		"query":          func(value *TargetConfig) { value.WebSocketPath = "/control?x=1" },
		"escaped":        func(value *TargetConfig) { value.WebSocketPath = "/control%2fws" },
		"backslash":      func(value *TargetConfig) { value.WebSocketPath = `/control\ws` },
		"zero port":      func(value *TargetConfig) { value.ControlPort = 0 },
		"bad name":       func(value *TargetConfig) { value.TailscaleName = "Edge.example.ts.net" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := target
			mutate(&candidate)
			if _, err := LoadTargetConfig(writeConfigFile(t, candidate)); err == nil {
				t.Fatal("invalid target config loaded")
			}
		})
	}
}

func writeConfigFile(t *testing.T, value any) string {
	t.Helper()
	contents, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
