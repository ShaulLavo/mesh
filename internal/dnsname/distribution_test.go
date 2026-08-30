package dnsname

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math/big"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

func TestSignedBundleBindsTargetSignerCertificateAndKey(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	signerID, signer := testEd25519Identity(t)
	targetID, _ := testEd25519Identity(t)
	bundle := testBundle(t, 1, now, now.Add(90*24*time.Hour))
	signed, err := SignBundle(bundle, targetID, EnvironmentLive, signer)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := VerifySignedBundle(signed, targetID, signerID, WildcardName, now)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Fingerprint != bundle.Fingerprint {
		t.Fatalf("verified fingerprint = %s, want %s", verified.Fingerprint, bundle.Fingerprint)
	}

	otherTarget, _ := testEd25519Identity(t)
	otherSigner, _ := testEd25519Identity(t)
	for name, mutate := range map[string]func(*SignedBundle) (string, string){
		"environment": func(candidate *SignedBundle) (string, string) {
			candidate.Environment = EnvironmentStaging
			return targetID, signerID
		},
		"target": func(candidate *SignedBundle) (string, string) {
			candidate.TargetID = otherTarget
			return otherTarget, signerID
		},
		"signer": func(candidate *SignedBundle) (string, string) {
			candidate.SignerID = otherSigner
			return targetID, otherSigner
		},
		"certificate": func(candidate *SignedBundle) (string, string) {
			candidate.CertificatePEM[0] ^= 1
			return targetID, signerID
		},
		"key": func(candidate *SignedBundle) (string, string) {
			candidate.PrivateKeyPEM[0] ^= 1
			return targetID, signerID
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := cloneSignedBundle(signed)
			wantTarget, wantSigner := mutate(&candidate)
			if _, err := VerifySignedBundle(candidate, wantTarget, wantSigner, WildcardName, now); err == nil {
				t.Fatal("tampered bundle verified")
			}
		})
	}
}

func TestVerifySignedBundleBoundsPayloadBeforeCrypto(t *testing.T) {
	targetID, _ := testEd25519Identity(t)
	signerID, _ := testEd25519Identity(t)
	signed := SignedBundle{
		TargetID: targetID, SignerID: signerID,
		CertificatePEM: make([]byte, maximumCertificatePEM+1), PrivateKeyPEM: []byte("key"), Signature: make([]byte, ed25519.SignatureSize),
	}
	if _, err := VerifySignedBundle(signed, targetID, signerID, WildcardName, time.Now()); err == nil || !strings.Contains(err.Error(), "size") {
		t.Fatalf("oversized certificate error = %v", err)
	}
	signed.CertificatePEM = []byte("certificate")
	signed.PrivateKeyPEM = make([]byte, maximumPrivateKeyPEM+1)
	if _, err := VerifySignedBundle(signed, targetID, signerID, WildcardName, time.Now()); err == nil || !strings.Contains(err.Error(), "size") {
		t.Fatalf("oversized private key error = %v", err)
	}
	signed.PrivateKeyPEM = []byte("key")
	signed.Signature = make([]byte, ed25519.SignatureSize+1)
	if _, err := VerifySignedBundle(signed, targetID, signerID, WildcardName, time.Now()); err == nil || !strings.Contains(err.Error(), "signature size") {
		t.Fatalf("oversized signature error = %v", err)
	}
}

func TestInstallerIsIdempotentRejectsRollbackAndAllowsEqualExpiryRotation(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	signerID, signer := testEd25519Identity(t)
	targetID, _ := testEd25519Identity(t)
	root := t.TempDir()
	store, err := NewBundleStore(filepath.Join(root, "live"), WildcardName)
	if err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return now }
	stagingStore, err := NewBundleStore(filepath.Join(root, "staging"), WildcardName)
	if err != nil {
		t.Fatal(err)
	}
	stagingStore.now = func() time.Time { return now }
	source, err := NewCertificateSource(store)
	if err != nil {
		t.Fatal(err)
	}
	installer, err := NewInstaller(InstallerConfig{
		LiveSource: source, StagingStore: stagingStore, TargetID: targetID, SignerID: signerID, ExpectedName: WildcardName, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}

	first := testBundle(t, 1, now, now.Add(90*24*time.Hour))
	firstSigned, err := SignBundle(first, targetID, EnvironmentLive, signer)
	if err != nil {
		t.Fatal(err)
	}
	if installed, changed, err := installer.Install(firstSigned); err != nil || !changed || installed.Fingerprint != first.Fingerprint {
		t.Fatalf("first install = %s, changed %v, error %v", installed.Fingerprint, changed, err)
	}
	if _, changed, err := installer.Install(firstSigned); err != nil || changed {
		t.Fatalf("replay changed = %v, error = %v", changed, err)
	}

	earlier := testBundle(t, 2, now, now.Add(60*24*time.Hour))
	earlierSigned, err := SignBundle(earlier, targetID, EnvironmentLive, signer)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := installer.Install(earlierSigned); err == nil || !strings.Contains(err.Error(), "before current") {
		t.Fatalf("rollback error = %v", err)
	}

	rotation := testBundle(t, 3, now, first.NotAfter)
	rotationSigned, err := SignBundle(rotation, targetID, EnvironmentLive, signer)
	if err != nil {
		t.Fatal(err)
	}
	installed, changed, err := installer.Install(rotationSigned)
	if err != nil || !changed || installed.Fingerprint != rotation.Fingerprint {
		t.Fatalf("equal-expiry rotation = %s, changed %v, error %v", installed.Fingerprint, changed, err)
	}
	current, err := source.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if current.Leaf.SerialNumber.Cmp(big.NewInt(3)) != 0 {
		t.Fatalf("current serial = %s, want 3", current.Leaf.SerialNumber)
	}
}

func TestInstallerPersistsStagingWithoutChangingLiveCertificate(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	signerID, signer := testEd25519Identity(t)
	targetID, _ := testEd25519Identity(t)
	root := t.TempDir()
	liveStore, err := NewBundleStore(filepath.Join(root, "live"), WildcardName)
	if err != nil {
		t.Fatal(err)
	}
	liveStore.now = func() time.Time { return now }
	stagingStore, err := NewBundleStore(filepath.Join(root, "staging"), WildcardName)
	if err != nil {
		t.Fatal(err)
	}
	stagingStore.now = func() time.Time { return now }
	source, err := NewCertificateSource(liveStore)
	if err != nil {
		t.Fatal(err)
	}
	installer, err := NewInstaller(InstallerConfig{
		LiveSource: source, StagingStore: stagingStore, TargetID: targetID, SignerID: signerID,
		ExpectedName: WildcardName, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}

	live := testBundle(t, 10, now, now.Add(90*24*time.Hour))
	liveSigned, err := SignBundle(live, targetID, EnvironmentLive, signer)
	if err != nil {
		t.Fatal(err)
	}
	if _, changed, err := installer.Install(liveSigned); err != nil || !changed {
		t.Fatalf("live install changed = %v, error = %v", changed, err)
	}
	staging := testBundle(t, 20, now, now.Add(91*24*time.Hour))
	stagingSigned, err := SignBundle(staging, targetID, EnvironmentStaging, signer)
	if err != nil {
		t.Fatal(err)
	}
	if _, changed, err := installer.Install(stagingSigned); err != nil || !changed {
		t.Fatalf("staging install changed = %v, error = %v", changed, err)
	}
	currentLive, err := source.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if currentLive.Leaf.SerialNumber.Cmp(big.NewInt(10)) != 0 {
		t.Fatalf("live serial after staging install = %s, want 10", currentLive.Leaf.SerialNumber)
	}
	persistedStaging, err := stagingStore.Load()
	if err != nil {
		t.Fatal(err)
	}
	if persistedStaging.TLSCertificate.Leaf.SerialNumber.Cmp(big.NewInt(20)) != 0 {
		t.Fatalf("staging serial = %s, want 20", persistedStaging.TLSCertificate.Leaf.SerialNumber)
	}

	crossEnvironment := cloneSignedBundle(stagingSigned)
	crossEnvironment.Environment = EnvironmentLive
	if _, _, err := installer.Install(crossEnvironment); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("cross-environment install error = %v", err)
	}
	currentLive, err = source.GetCertificate(nil)
	if err != nil || currentLive.Leaf.SerialNumber.Cmp(big.NewInt(10)) != 0 {
		t.Fatalf("live certificate changed after cross-environment tamper: serial %v, error %v", currentLive.Leaf.SerialNumber, err)
	}
}

func TestDistributorPinsOriginIdentityAndSignedInstall(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	signerID, signer := testEd25519Identity(t)
	firstID, _ := testEd25519Identity(t)
	secondID, _ := testEd25519Identity(t)
	bundle := testBundle(t, 1, now, now.Add(90*24*time.Hour))
	identities := map[string]string{
		"ws://100.64.0.1:7337/mesh": firstID,
		"ws://100.64.0.2:7337/mesh": secondID,
	}
	var dials atomic.Int64
	distributor := mustTestDistributor(t, DistributorConfig{
		Signer: signer, Environment: EnvironmentLive, ExpectedName: WildcardName, Now: func() time.Time { return now },
		Dial: func(_ context.Context, endpoint string) (transport.Conn, error) {
			dials.Add(1)
			identity := identities[endpoint]
			return newDistributionTestConn(t, identity, signerID, bundle.Fingerprint, now), nil
		},
	})
	targets := []OriginTarget{
		{Name: "desktop", Endpoint: "ws://100.64.0.1:7337/mesh", Identity: firstID},
		{Name: "laptop", Endpoint: "ws://100.64.0.2:7337/mesh", Identity: secondID},
	}
	callerBundle := bundle
	callerBundle.Fingerprint = "stale-caller-fingerprint"
	if err := distributor.Distribute(context.Background(), callerBundle, targets); err != nil {
		t.Fatal(err)
	}
	if dials.Load() != 2 {
		t.Fatalf("dials = %d, want 2", dials.Load())
	}

	wrong := append([]OriginTarget(nil), targets[:1]...)
	wrong[0].Identity = secondID
	if err := distributor.Distribute(context.Background(), callerBundle, wrong); err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("identity mismatch error = %v", err)
	}

	invalid := append(append([]OriginTarget(nil), targets...), OriginTarget{Name: "unsafe", Endpoint: "ws://user:secret@100.64.0.3/mesh", Identity: testIdentityID(t)})
	before := dials.Load()
	if err := distributor.Distribute(context.Background(), callerBundle, invalid); err == nil {
		t.Fatal("unsafe endpoint accepted")
	}
	if dials.Load() != before {
		t.Fatalf("pre-validation made %d network calls", dials.Load()-before)
	}
}

func TestDistributorBoundsConcurrentOrigins(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	_, signer := testEd25519Identity(t)
	bundle := testBundle(t, 1, now, now.Add(90*24*time.Hour))
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	var active atomic.Int64
	var maximum atomic.Int64
	distributor := mustTestDistributor(t, DistributorConfig{
		Signer: signer, Environment: EnvironmentLive, ExpectedName: WildcardName, Concurrency: 2, Timeout: time.Second, Now: func() time.Time { return now },
		Dial: func(ctx context.Context, _ string) (transport.Conn, error) {
			current := active.Add(1)
			defer active.Add(-1)
			for {
				prior := maximum.Load()
				if current <= prior || maximum.CompareAndSwap(prior, current) {
					break
				}
			}
			started <- struct{}{}
			select {
			case <-release:
				return nil, errors.New("released test dial")
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	})
	targets := make([]OriginTarget, 4)
	for index := range targets {
		targets[index] = OriginTarget{
			Name: fmt.Sprintf("origin-%d", index), Endpoint: fmt.Sprintf("ws://100.64.0.%d:7337/mesh", index+1), Identity: testIdentityID(t),
		}
	}
	done := make(chan error, 1)
	go func() { done <- distributor.Distribute(context.Background(), bundle, targets) }()
	<-started
	<-started
	select {
	case <-started:
		t.Fatal("third origin started while two distribution slots were occupied")
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if err := <-done; err == nil {
		t.Fatal("released test dials unexpectedly succeeded")
	}
	if maximum.Load() != 2 {
		t.Fatalf("maximum concurrent dials = %d, want 2", maximum.Load())
	}
}

func mustTestDistributor(t *testing.T, config DistributorConfig) *Distributor {
	t.Helper()
	var sequence atomic.Uint64
	config.RequestID = func() (string, error) { return fmt.Sprintf("request-%d", sequence.Add(1)), nil }
	distributor, err := NewDistributor(config)
	if err != nil {
		t.Fatal(err)
	}
	return distributor
}

func testBundle(t *testing.T, serial int64, now, notAfter time.Time) Bundle {
	t.Helper()
	certificatePEM, privateKeyPEM := testCertificate(t, serial, WildcardName, now.Add(-time.Hour), notAfter)
	bundle, err := ValidateBundle(certificatePEM, privateKeyPEM, WildcardName, now)
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

func testEd25519Identity(t *testing.T) (string, ed25519.PrivateKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(publicKey), privateKey
}

func testIdentityID(t *testing.T) string {
	t.Helper()
	identity, _ := testEd25519Identity(t)
	return identity
}

func cloneSignedBundle(signed SignedBundle) SignedBundle {
	signed.CertificatePEM = append([]byte(nil), signed.CertificatePEM...)
	signed.PrivateKeyPEM = append([]byte(nil), signed.PrivateKeyPEM...)
	signed.Signature = append([]byte(nil), signed.Signature...)
	return signed
}

type distributionTestConn struct {
	t           *testing.T
	identity    string
	signerID    string
	fingerprint string
	now         time.Time
	mu          sync.Mutex
	response    protocol.Frame
	closed      bool
}

func newDistributionTestConn(t *testing.T, identity, signerID, fingerprint string, now time.Time) *distributionTestConn {
	t.Helper()
	return &distributionTestConn{t: t, identity: identity, signerID: signerID, fingerprint: fingerprint, now: now}
}

func (c *distributionTestConn) WriteFrame(frame protocol.Frame) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return io.ErrClosedPipe
	}
	if frame.Kind != protocol.KindControl {
		return errors.New("test connection received non-control frame")
	}
	request, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return err
	}
	response := protocol.Control{RequestID: request.RequestID}
	switch request.Type {
	case protocol.TypeHostInfo:
		response.Type = protocol.TypeHostInfoResult
		response.Host = &protocol.HostInfo{ID: c.identity, MeshIdentity: c.identity}
	case protocol.TypeCertificateInstall:
		if request.Certificate == nil {
			return errors.New("test connection received nil certificate")
		}
		bundle, err := VerifySignedBundle(SignedBundle{
			Environment: RenewalEnvironment(request.Certificate.Environment),
			TargetID:    request.Certificate.TargetID, SignerID: request.Certificate.SignerID,
			CertificatePEM: request.Certificate.CertificatePEM, PrivateKeyPEM: request.Certificate.PrivateKeyPEM,
			Signature: request.Certificate.Signature,
		}, c.identity, c.signerID, WildcardName, c.now)
		if err != nil {
			response.Type = protocol.TypeError
			response.Message = err.Error()
		} else {
			response.Type = protocol.TypeCertificateInstalled
			response.CertificateFingerprint = bundle.Fingerprint
			response.CertificateEnvironment = request.Certificate.Environment
			if response.CertificateFingerprint != c.fingerprint {
				return fmt.Errorf("test installed fingerprint %s, want %s", response.CertificateFingerprint, c.fingerprint)
			}
		}
	default:
		return fmt.Errorf("test connection received request %q", request.Type)
	}
	payload, err := response.Encode()
	if err != nil {
		return err
	}
	c.response = protocol.Frame{Kind: protocol.KindControl, Payload: payload}
	return nil
}

func (c *distributionTestConn) ReadFrame() (protocol.Frame, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return protocol.Frame{}, io.ErrClosedPipe
	}
	if c.response.Payload == nil {
		return protocol.Frame{}, errors.New("test connection has no response")
	}
	response := c.response
	c.response = protocol.Frame{}
	return response, nil
}

func (c *distributionTestConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}
