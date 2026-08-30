package bootstrap

import (
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestParseTarget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  target
	}{
		{input: "shaul@pc", want: target{user: "shaul", host: "pc", port: 22}},
		{input: "shaul@pc:2222", want: target{user: "shaul", host: "pc", port: 2222}},
		{input: "shaul@[fd7a:115c:a1e0::1]:2200", want: target{user: "shaul", host: "fd7a:115c:a1e0::1", port: 2200}},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := parseTarget(tt.input)
			if err != nil {
				t.Fatalf("parseTarget() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("parseTarget() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestCheckBinaryPlatformNamesWrongArchitecture(t *testing.T) {
	t.Parallel()

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	want := Platform{OS: OS(runtime.GOOS), Arch: AMD64}
	if runtime.GOARCH == string(AMD64) {
		want.Arch = ARM64
	}
	err = checkBinaryPlatform(executable, want)
	assertDiagnosticCode(t, err, DiagnosticWrongArch)
}

func TestParseTargetRejectsAmbiguousInput(t *testing.T) {
	t.Parallel()

	for _, input := range []string{"", "pc", "@pc", "shaul@", "shaul:secret@pc", "shaul@pc:70000", "shaul@pc/path"} {
		t.Run(input, func(t *testing.T) {
			if _, err := parseTarget(input); err == nil {
				t.Fatalf("parseTarget(%q) succeeded", input)
			}
		})
	}
}

func TestParsePlatform(t *testing.T) {
	t.Parallel()

	tests := []struct {
		uname string
		want  Platform
	}{
		{uname: "Linux\nx86_64\n", want: Platform{OS: Linux, Arch: AMD64}},
		{uname: "Linux\naarch64\n", want: Platform{OS: Linux, Arch: ARM64}},
		{uname: "Darwin\nx86_64\n", want: Platform{OS: Darwin, Arch: AMD64}},
		{uname: "Darwin\narm64\n", want: Platform{OS: Darwin, Arch: ARM64}},
	}
	for _, tt := range tests {
		t.Run(strings.ReplaceAll(strings.TrimSpace(tt.uname), "\n", "/"), func(t *testing.T) {
			got, err := parsePlatform([]byte(tt.uname))
			if err != nil {
				t.Fatalf("parsePlatform() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("parsePlatform() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestEveryRequiredDiagnosticIsTypedAndActionable(t *testing.T) {
	t.Parallel()

	for _, code := range []DiagnosticCode{
		DiagnosticSSHAuth,
		DiagnosticWrongArch,
		DiagnosticNoSystemd,
		DiagnosticNoUserLingering,
		DiagnosticTailscaleLoggedOut,
		DiagnosticPortBlocked,
		DiagnosticClockSkew,
	} {
		t.Run(string(code), func(t *testing.T) {
			cause := errors.New("fixture failure")
			err := diagnostic(code, cause)
			var got *DiagnosticError
			if !errors.As(err, &got) {
				t.Fatalf("diagnostic() error %T is not a DiagnosticError", err)
			}
			if got.Code != code || !errors.Is(got, cause) {
				t.Fatalf("diagnostic() = %#v", got)
			}
			if got.Suggestion == "" || !strings.Contains(got.Error(), string(code)) {
				t.Fatalf("diagnostic text is not actionable: %q", got.Error())
			}
		})
	}
}
