package protocol

import (
	"reflect"
	"testing"
	"time"
)

func TestLifecycleControlRoundTrip(t *testing.T) {
	createdAt := time.Date(2026, time.August, 29, 12, 34, 56, 0, time.UTC)
	attachedAt := createdAt.Add(time.Minute)
	exitCode := 7
	want := Control{
		Type:      TypeListed,
		RequestID: "request-7",
		Sessions: []SessionInfo{{
			ID:                 "7K3D",
			HostID:             "host-public-key",
			Command:            []string{"sh", "-lc", "printf ready"},
			Cwd:                "/tmp/work",
			State:              "exited",
			CreatedAt:          createdAt,
			LastAttachedAt:     &attachedAt,
			ExitCode:           &exitCode,
			LastOutputSequence: 4096,
		}},
	}

	payload, err := want.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeControl(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded control = %#v, want %#v", got, want)
	}
}

func TestHostInfoControlRoundTrip(t *testing.T) {
	want := Control{
		Type:      TypeHostInfoResult,
		RequestID: "request-8",
		Host: &HostInfo{
			ID:            "host-public-key",
			MeshIdentity:  "host-public-key",
			TailscaleName: "desktop.example.ts.net",
		},
	}

	payload, err := want.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeControl(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded control = %#v, want %#v", got, want)
	}
}

func TestLogsControlRoundTrip(t *testing.T) {
	want := Control{
		Type:      TypeLogged,
		RequestID: "logs-1",
		SessionID: "7K3D",
		Tail:      4096,
		Output:    []byte("prompt> printf ready\r\nready\r\n"),
	}

	payload, err := want.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeControl(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded control = %#v, want %#v", got, want)
	}
}

func TestServiceControlRoundTrip(t *testing.T) {
	want := Control{
		Type:        TypeServiceListed,
		RequestID:   "service-list-1",
		ServiceName: "blog",
		Service: &ServiceInfo{
			Name:       "blog",
			Kind:       "static",
			Target:     "/srv/blog",
			PublicName: "blog.shaulavo.dev",
			Healthy:    true,
		},
		Services: []ServiceInfo{{
			Name:          "files",
			Kind:          "files",
			Target:        "/srv/files",
			WakeOnRequest: true,
			Healthy:       false,
			Problem:       "root unavailable",
		}},
	}

	payload, err := want.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeControl(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded control = %#v, want %#v", got, want)
	}
}

func TestCertificateControlRoundTrip(t *testing.T) {
	want := Control{
		Type:      TypeCertificateInstall,
		RequestID: "certificate-1",
		Certificate: &CertificateInstall{
			Profile:        "private-origin",
			Environment:    "staging",
			TargetID:       "origin-key",
			SignerID:       "renewer-key",
			CertificatePEM: []byte("certificate"),
			PrivateKeyPEM:  []byte("private-key"),
			Signature:      []byte("signature"),
		},
		CertificateFingerprint: "sha256-fingerprint",
		CertificateEnvironment: "staging",
		CertificateProfile:     "private-origin",
	}

	payload, err := want.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeControl(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded control = %#v, want %#v", got, want)
	}
}

func TestEdgeRegistrationControlRoundTrip(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	want := Control{
		Type: TypeEdgeRegister, RequestID: "edge-1",
		EdgeSnapshot: &EdgeSnapshot{
			TargetID: "edge", OriginID: "origin", Sequence: 9, IssuedAt: now, ExpiresAt: now.Add(5 * time.Minute),
			Routes: []EdgeRoute{{PublicName: "app.shaulavo.dev", ServiceName: "app", WakeOnRequest: true}}, Signature: []byte("signature"),
		},
		EdgeSequence: 9, EdgeDigest: "digest",
		EdgeRoutes: []EdgeRouteInfo{{PublicName: "app.shaulavo.dev", ServiceName: "app", DisplayAlias: "Desktop", LastSeenAt: now, Online: true}},
	}
	payload, err := want.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeControl(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded control = %#v, want %#v", got, want)
	}
}
