module github.com/shaul/mesh

go 1.27.4

replace charm.land/wish/v2 => ./third_party/wish

replace github.com/pkg/sftp => ./third_party/sftp

// This fork avoids upstream putRange rewriting unchanged interior spans; its equal-run test is unreachable.
// Drop this replacement after upstream fixes the equal-run test.
replace github.com/charmbracelet/ultraviolet => ./third_party/ultraviolet

require (
	charm.land/bubbles/v2 v2.2.1
	charm.land/bubbletea/v2 v2.0.9
	charm.land/huh/v2 v2.0.3
	charm.land/lipgloss/v2 v2.0.6
	charm.land/ssh v0.4.2
	charm.land/wish/v2 v2.0.3
	github.com/andybalholm/brotli v1.2.8
	github.com/charmbracelet/colorprofile v0.4.3
	github.com/charmbracelet/fang v1.0.0
	github.com/charmbracelet/ultraviolet v0.0.0-20260811164956-006e29f97886
	github.com/charmbracelet/x/ansi v0.11.8
	github.com/charmbracelet/x/exp/golden v0.0.0-20260830003929-9f48cc723c1c
	github.com/charmbracelet/x/exp/teatest/v2 v2.0.0-20260830003929-9f48cc723c1c
	github.com/charmbracelet/x/term v0.2.2
	github.com/charmbracelet/x/vt v0.0.0-20260828171018-3c30eef5e73e
	github.com/charmbracelet/x/xpty v0.1.4
	github.com/coder/websocket v1.8.15
	github.com/creack/pty v1.1.24
	github.com/ebitengine/purego v0.11.1
	github.com/google/certificate-transparency-go v1.4.0
	github.com/kevinburke/ssh_config v1.6.0
	github.com/muesli/cancelreader v0.2.2
	github.com/pkg/sftp v1.13.11
	github.com/pressly/goose/v3 v3.27.3
	github.com/secure-systems-lab/go-securesystemslib v0.11.1
	github.com/shirou/gopsutil/v4 v4.26.9
	github.com/sigstore/protobuf-specs v0.5.2
	github.com/sigstore/sigstore v1.11.1
	github.com/sigstore/sigstore-go v1.4.0
	github.com/spf13/cobra v1.10.2
	github.com/spf13/pflag v1.0.10
	github.com/theupdateframework/go-tuf/v2 v2.4.2
	golang.org/x/crypto v0.58.0
	golang.org/x/net v0.61.0
	golang.org/x/sys v0.49.0
	golang.org/x/time v0.16.0
	google.golang.org/protobuf v1.36.12
	modernc.org/sqlite v1.57.0
)

require (
	charm.land/log/v2 v2.0.0 // indirect
	github.com/ProtonMail/go-crypto v1.4.1 // indirect
	github.com/anmitsu/go-shlex v0.0.0-20200514113438-38f4b401e2be // indirect
	github.com/asaskevich/govalidator v0.0.0-20230301143203-a9d515a09cc2 // indirect
	github.com/atotto/clipboard v0.1.4 // indirect
	github.com/aymanbagabas/go-udiff v0.4.1 // indirect
	github.com/catppuccin/go v0.2.0 // indirect
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/charmbracelet/keygen v0.5.4 // indirect
	github.com/charmbracelet/x/conpty v0.2.0 // indirect
	github.com/charmbracelet/x/exp/charmtone v0.0.0-20250603201427-c31516f43444 // indirect
	github.com/charmbracelet/x/exp/ordered v0.1.0 // indirect
	github.com/charmbracelet/x/exp/strings v0.0.0-20240722160745-212f7b056ed0 // indirect
	github.com/charmbracelet/x/termios v0.1.1 // indirect
	github.com/charmbracelet/x/windows v0.2.2 // indirect
	github.com/clipperhouse/displaywidth v0.11.0 // indirect
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/cloudflare/circl v1.6.3 // indirect
	github.com/coreos/go-oidc/v3 v3.21.0 // indirect
	github.com/cyberphone/json-canonicalization v0.0.0-20241213102144-19d51d7fe467 // indirect
	github.com/digitorus/pkcs7 v0.0.0-20260821105541-05f79448fa77 // indirect
	github.com/digitorus/timestamp v0.0.0-20231217203849-220c5c2851b7 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/go-jose/go-jose/v4 v4.1.6 // indirect
	github.com/go-logfmt/logfmt v0.6.1 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-ole/go-ole v1.2.6 // indirect
	github.com/go-openapi/analysis v1.0.0 // indirect
	github.com/go-openapi/errors v0.22.9 // indirect
	github.com/go-openapi/jsonpointer v1.0.1 // indirect
	github.com/go-openapi/jsonreference v1.0.2 // indirect
	github.com/go-openapi/loads v0.25.3 // indirect
	github.com/go-openapi/runtime v0.33.3 // indirect
	github.com/go-openapi/runtime/server-middleware v0.33.3 // indirect
	github.com/go-openapi/spec v1.0.1 // indirect
	github.com/go-openapi/strfmt v0.27.4 // indirect
	github.com/go-openapi/swag/conv v0.29.2 // indirect
	github.com/go-openapi/swag/fileutils v0.29.2 // indirect
	github.com/go-openapi/swag/jsonutils v0.29.2 // indirect
	github.com/go-openapi/swag/loading v0.29.2 // indirect
	github.com/go-openapi/swag/mangling v0.29.2 // indirect
	github.com/go-openapi/swag/pools v0.29.2 // indirect
	github.com/go-openapi/swag/stringutils v0.29.2 // indirect
	github.com/go-openapi/swag/typeutils v0.29.2 // indirect
	github.com/go-openapi/swag/yamlutils v0.29.2 // indirect
	github.com/go-openapi/validate v1.0.0 // indirect
	github.com/go-viper/mapstructure/v2 v2.5.0 // indirect
	github.com/google/go-containerregistry v0.22.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.32.0 // indirect
	github.com/in-toto/attestation v1.2.0 // indirect
	github.com/in-toto/in-toto-golang v0.11.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/jedisct1/go-minisign v0.0.0-20211028175153-1c139d1cc84b // indirect
	github.com/kr/fs v0.1.0 // indirect
	github.com/lucasb-eyer/go-colorful v1.4.1 // indirect
	github.com/lufia/plan9stats v0.0.0-20211012122336-39d0f177ccd0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/mattn/go-runewidth v0.0.30 // indirect
	github.com/mfridman/interpolate v0.0.2 // indirect
	github.com/mitchellh/hashstructure/v2 v2.0.2 // indirect
	github.com/muesli/mango v0.1.0 // indirect
	github.com/muesli/mango-cobra v1.2.0 // indirect
	github.com/muesli/mango-pflag v0.1.0 // indirect
	github.com/muesli/roff v0.1.0 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/oklog/ulid/v2 v2.1.2 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/pires/go-proxyproto v0.12.0 // indirect
	github.com/pkg/browser v0.0.0-20240102092130-5ac0b6a4141c // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/power-devops/perfstat v0.0.0-20260805114148-88456608a4f6 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/sahilm/fuzzy v0.1.3 // indirect
	github.com/sassoftware/relic/v8 v8.2.0 // indirect
	github.com/sethvargo/go-retry v0.4.0 // indirect
	github.com/shibumi/go-pathspec v1.3.0 // indirect
	github.com/sigstore/rekor v1.5.5 // indirect
	github.com/sigstore/rekor-tiles/v2 v2.4.0 // indirect
	github.com/sigstore/timestamp-authority/v2 v2.2.0 // indirect
	github.com/theupdateframework/go-tuf v0.7.0 // indirect
	github.com/tklauser/go-sysconf v0.4.0 // indirect
	github.com/tklauser/numcpus v0.12.0 // indirect
	github.com/transparency-dev/formats v0.1.1 // indirect
	github.com/transparency-dev/merkle v0.0.2 // indirect
	github.com/xo/terminfo v0.0.0-20220910002029-abceb7e1c41e // indirect
	github.com/youmark/pkcs8 v0.0.0-20240726163527-a2c0da244d78 // indirect
	github.com/yusufpapurcu/wmi v1.2.4 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel v1.47.0 // indirect
	go.opentelemetry.io/otel/log v1.47.0 // indirect
	go.opentelemetry.io/otel/metric v1.47.0 // indirect
	go.opentelemetry.io/otel/trace v1.47.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/exp v0.0.0-20261014143641-f0251eb4aa3e // indirect
	golang.org/x/mod v0.42.0 // indirect
	golang.org/x/oauth2 v0.37.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/term v0.47.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20261012220534-4341edf3f941 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20261005182115-fad411399dd8 // indirect
	google.golang.org/grpc v1.84.0 // indirect
	k8s.io/klog/v2 v2.140.0 // indirect
	modernc.org/libc v1.74.4 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)
