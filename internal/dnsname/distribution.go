package dnsname

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

const (
	certificateSignatureDomain = "mesh/certificate-bundle/v1"
	defaultDistributionLimit   = 4
	maximumDistributionLimit   = 16
	defaultDistributionTimeout = 10 * time.Second
	maximumDistributionTimeout = time.Minute
	maximumDistributionTargets = 256
)

// SignedBundle binds one certificate and key to an exact origin identity and
// renewer identity. The signature covers a domain-separated canonical digest.
type SignedBundle struct {
	Environment    RenewalEnvironment
	TargetID       string
	SignerID       string
	CertificatePEM []byte
	PrivateKeyPEM  []byte
	Signature      []byte
}

// SignBundle signs bundle for one exact target with the renewer's Mesh
// identity key.
func SignBundle(bundle Bundle, targetID string, environment RenewalEnvironment, signer ed25519.PrivateKey) (SignedBundle, error) {
	if len(signer) != ed25519.PrivateKeySize {
		return SignedBundle{}, errors.New("dnsname: certificate signer is not an Ed25519 private key")
	}
	if _, err := decodeIdentity("certificate target", targetID); err != nil {
		return SignedBundle{}, err
	}
	if err := validateRenewalEnvironment(environment); err != nil {
		return SignedBundle{}, err
	}
	signerID := base64.RawURLEncoding.EncodeToString(signer.Public().(ed25519.PublicKey))
	digest, err := certificateDigest(environment, targetID, signerID, bundle.CertificatePEM, bundle.PrivateKeyPEM)
	if err != nil {
		return SignedBundle{}, err
	}
	return SignedBundle{
		Environment:    environment,
		TargetID:       targetID,
		SignerID:       signerID,
		CertificatePEM: append([]byte(nil), bundle.CertificatePEM...),
		PrivateKeyPEM:  append([]byte(nil), bundle.PrivateKeyPEM...),
		Signature:      ed25519.Sign(signer, digest[:]),
	}, nil
}

// VerifySignedBundle verifies the exact target and renewer pins before
// parsing and validating the certificate bundle.
func VerifySignedBundle(signed SignedBundle, targetID, signerID, expectedName string, now time.Time) (Bundle, error) {
	if len(signed.CertificatePEM) == 0 || len(signed.CertificatePEM) > maximumCertificatePEM {
		return Bundle{}, fmt.Errorf("dnsname: distributed certificate PEM size %d is outside 1..%d", len(signed.CertificatePEM), maximumCertificatePEM)
	}
	if len(signed.PrivateKeyPEM) == 0 || len(signed.PrivateKeyPEM) > maximumPrivateKeyPEM {
		return Bundle{}, fmt.Errorf("dnsname: distributed private key PEM size %d is outside 1..%d", len(signed.PrivateKeyPEM), maximumPrivateKeyPEM)
	}
	if len(signed.Signature) != ed25519.SignatureSize {
		return Bundle{}, fmt.Errorf("dnsname: distributed signature size %d, want %d", len(signed.Signature), ed25519.SignatureSize)
	}
	if err := validateRenewalEnvironment(signed.Environment); err != nil {
		return Bundle{}, err
	}
	if signed.TargetID != targetID {
		return Bundle{}, fmt.Errorf("dnsname: certificate targets identity %q, want %q", signed.TargetID, targetID)
	}
	if signed.SignerID != signerID {
		return Bundle{}, fmt.Errorf("dnsname: certificate signer identity %q, want pinned renewer %q", signed.SignerID, signerID)
	}
	if _, err := decodeIdentity("certificate target", targetID); err != nil {
		return Bundle{}, err
	}
	publicKey, err := decodeIdentity("certificate renewer", signerID)
	if err != nil {
		return Bundle{}, err
	}
	digest, err := certificateDigest(signed.Environment, signed.TargetID, signed.SignerID, signed.CertificatePEM, signed.PrivateKeyPEM)
	if err != nil {
		return Bundle{}, err
	}
	if !ed25519.Verify(publicKey, digest[:], signed.Signature) {
		return Bundle{}, errors.New("dnsname: distributed certificate signature is invalid")
	}
	return ValidateBundle(signed.CertificatePEM, signed.PrivateKeyPEM, expectedName, now)
}

func certificateDigest(environment RenewalEnvironment, targetID, signerID string, certificatePEM, privateKeyPEM []byte) ([sha256.Size]byte, error) {
	if len(certificatePEM) == 0 || len(certificatePEM) > maximumCertificatePEM {
		return [sha256.Size]byte{}, fmt.Errorf("dnsname: certificate PEM size %d is outside 1..%d", len(certificatePEM), maximumCertificatePEM)
	}
	if len(privateKeyPEM) == 0 || len(privateKeyPEM) > maximumPrivateKeyPEM {
		return [sha256.Size]byte{}, fmt.Errorf("dnsname: private key PEM size %d is outside 1..%d", len(privateKeyPEM), maximumPrivateKeyPEM)
	}
	hash := sha256.New()
	writeDigestField(hash, []byte(certificateSignatureDomain))
	writeDigestField(hash, []byte(environment))
	writeDigestField(hash, []byte(targetID))
	writeDigestField(hash, []byte(signerID))
	writeDigestField(hash, certificatePEM)
	writeDigestField(hash, privateKeyPEM)
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return digest, nil
}

type digestWriter interface {
	Write([]byte) (int, error)
}

func writeDigestField(writer digestWriter, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = writer.Write(size[:])
	_, _ = writer.Write(value)
}

func decodeIdentity(label, value string) (ed25519.PublicKey, error) {
	if value == "" || strings.TrimSpace(value) != value {
		return nil, fmt.Errorf("dnsname: %s identity is empty or not canonical", label)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) != ed25519.PublicKeySize || base64.RawURLEncoding.EncodeToString(decoded) != value {
		return nil, fmt.Errorf("dnsname: %s identity is not a canonical Ed25519 public key", label)
	}
	return ed25519.PublicKey(decoded), nil
}

// InstallerConfig pins the only renewer and target accepted by one origin.
type InstallerConfig struct {
	LiveSource   *CertificateSource
	StagingStore *BundleStore
	TargetID     string
	SignerID     string
	ExpectedName string
	Now          func() time.Time
}

// Installer verifies and hot-installs signed bundles on one origin.
type Installer struct {
	liveSource   *CertificateSource
	stagingStore *BundleStore
	targetID     string
	signerID     string
	expectedName string
	now          func() time.Time
	mu           sync.Mutex
}

// NewInstaller validates the origin's immutable identity pins.
func NewInstaller(config InstallerConfig) (*Installer, error) {
	if config.LiveSource == nil {
		return nil, errors.New("dnsname: nil live certificate source")
	}
	if config.StagingStore == nil {
		return nil, errors.New("dnsname: nil staging certificate store")
	}
	if _, err := decodeIdentity("certificate target", config.TargetID); err != nil {
		return nil, err
	}
	if _, err := decodeIdentity("certificate renewer", config.SignerID); err != nil {
		return nil, err
	}
	if _, err := certificateProbeName(config.ExpectedName); err != nil {
		return nil, err
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Installer{
		liveSource: config.LiveSource, stagingStore: config.StagingStore, targetID: config.TargetID, signerID: config.SignerID,
		expectedName: config.ExpectedName, now: config.Now,
	}, nil
}

// Install verifies, persists, and hot-publishes signed. Replaying the current
// fingerprint is a no-op; a strictly earlier expiry is rejected as rollback.
func (i *Installer) Install(signed SignedBundle) (Bundle, bool, error) {
	i.mu.Lock()
	defer i.mu.Unlock()

	bundle, err := VerifySignedBundle(signed, i.targetID, i.signerID, i.expectedName, i.now().UTC())
	if err != nil {
		return Bundle{}, false, err
	}
	var currentFingerprint string
	var currentNotAfter time.Time
	var install func([]byte, []byte) (Bundle, error)
	switch signed.Environment {
	case EnvironmentLive:
		if current := i.liveSource.current.Load(); current != nil && current.Leaf != nil {
			fingerprint := sha256.Sum256(current.Leaf.Raw)
			currentFingerprint = hex.EncodeToString(fingerprint[:])
			currentNotAfter = current.Leaf.NotAfter.UTC()
		}
		install = i.liveSource.Install
	case EnvironmentStaging:
		current, loadErr := i.stagingStore.Load()
		if loadErr == nil {
			currentFingerprint = current.Fingerprint
			currentNotAfter = current.NotAfter
		} else if !errors.Is(loadErr, ErrNoCertificate) && !errors.Is(loadErr, ErrCertificateExpired) && !errors.Is(loadErr, ErrCertificateNotYetValid) {
			return Bundle{}, false, loadErr
		}
		install = i.stagingStore.Install
	default:
		return Bundle{}, false, fmt.Errorf("dnsname: unsupported certificate environment %q", signed.Environment)
	}
	if currentFingerprint == bundle.Fingerprint {
		return bundle, false, nil
	}
	if !currentNotAfter.IsZero() && bundle.NotAfter.Before(currentNotAfter) {
		return Bundle{}, false, fmt.Errorf("dnsname: distributed certificate expires at %s before current %s certificate at %s", bundle.NotAfter.Format(time.RFC3339), signed.Environment, currentNotAfter.Format(time.RFC3339))
	}
	installed, err := install(bundle.CertificatePEM, bundle.PrivateKeyPEM)
	if err != nil {
		return Bundle{}, false, err
	}
	return installed, true, nil
}

// OriginTarget is one configured and identity-pinned origin daemon.
type OriginTarget struct {
	Name     string
	Endpoint string
	Identity string
}

// OriginDial opens one direct Mesh control connection.
type OriginDial func(context.Context, string) (transport.Conn, error)

// DistributorConfig bounds certificate fan-out from the renewer.
type DistributorConfig struct {
	Signer       ed25519.PrivateKey
	Environment  RenewalEnvironment
	ExpectedName string
	Dial         OriginDial
	Concurrency  int
	Timeout      time.Duration
	Now          func() time.Time
	RequestID    func() (string, error)
}

// Distributor sends one validated bundle to configured origin daemons.
type Distributor struct {
	signer       ed25519.PrivateKey
	environment  RenewalEnvironment
	expectedName string
	dial         OriginDial
	concurrency  int
	timeout      time.Duration
	now          func() time.Time
	requestID    func() (string, error)
}

// NewDistributor validates immutable bounds before any network activity.
func NewDistributor(config DistributorConfig) (*Distributor, error) {
	if len(config.Signer) != ed25519.PrivateKeySize {
		return nil, errors.New("dnsname: certificate distributor signer is not an Ed25519 private key")
	}
	if err := validateRenewalEnvironment(config.Environment); err != nil {
		return nil, err
	}
	if _, err := certificateProbeName(config.ExpectedName); err != nil {
		return nil, err
	}
	if config.Dial == nil {
		config.Dial = func(ctx context.Context, endpoint string) (transport.Conn, error) {
			return transport.Dial(ctx, endpoint, transport.DialOptions{})
		}
	}
	if config.Concurrency == 0 {
		config.Concurrency = defaultDistributionLimit
	}
	if config.Concurrency < 1 || config.Concurrency > maximumDistributionLimit {
		return nil, fmt.Errorf("dnsname: certificate distribution concurrency %d is outside 1..%d", config.Concurrency, maximumDistributionLimit)
	}
	if config.Timeout == 0 {
		config.Timeout = defaultDistributionTimeout
	}
	if config.Timeout <= 0 || config.Timeout > maximumDistributionTimeout {
		return nil, fmt.Errorf("dnsname: certificate distribution timeout %s is outside (0,%s]", config.Timeout, maximumDistributionTimeout)
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.RequestID == nil {
		config.RequestID = randomRequestID
	}
	return &Distributor{
		signer: append(ed25519.PrivateKey(nil), config.Signer...), environment: config.Environment, expectedName: config.ExpectedName,
		dial: config.Dial, concurrency: config.Concurrency, timeout: config.Timeout, now: config.Now, requestID: config.RequestID,
	}, nil
}

// Distribute validates all targets first, then fans out with a bounded number
// of concurrent network operations. Returned errors retain target order.
func (d *Distributor) Distribute(ctx context.Context, bundle Bundle, targets []OriginTarget) error {
	if ctx == nil {
		return errors.New("dnsname: distribute certificate with nil context")
	}
	if len(targets) > maximumDistributionTargets {
		return fmt.Errorf("dnsname: certificate target count %d exceeds %d", len(targets), maximumDistributionTargets)
	}
	validated, err := ValidateBundle(bundle.CertificatePEM, bundle.PrivateKeyPEM, d.expectedName, d.now().UTC())
	if err != nil {
		return err
	}
	seenIdentities := make(map[string]struct{}, len(targets))
	seenEndpoints := make(map[string]struct{}, len(targets))
	for index, target := range targets {
		if err := validateOriginTarget(target); err != nil {
			return fmt.Errorf("dnsname: certificate target %d: %w", index, err)
		}
		if _, exists := seenIdentities[target.Identity]; exists {
			return fmt.Errorf("dnsname: certificate target %d duplicates identity %q", index, target.Identity)
		}
		if _, exists := seenEndpoints[target.Endpoint]; exists {
			return fmt.Errorf("dnsname: certificate target %d duplicates endpoint %q", index, target.Endpoint)
		}
		seenIdentities[target.Identity] = struct{}{}
		seenEndpoints[target.Endpoint] = struct{}{}
	}

	semaphore := make(chan struct{}, d.concurrency)
	errorsByTarget := make([]error, len(targets))
	var group sync.WaitGroup
	for index, target := range targets {
		group.Add(1)
		go func() {
			defer group.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				errorsByTarget[index] = ctx.Err()
				return
			}
			targetCtx, cancel := context.WithTimeout(ctx, d.timeout)
			defer cancel()
			if err := d.distributeOne(targetCtx, validated, target); err != nil {
				errorsByTarget[index] = fmt.Errorf("%s: %w", target.Name, err)
			}
		}()
	}
	group.Wait()
	return errors.Join(errorsByTarget...)
}

func validateOriginTarget(target OriginTarget) error {
	if target.Name == "" || strings.TrimSpace(target.Name) != target.Name || len(target.Name) > 128 || strings.ContainsAny(target.Name, "\r\n") {
		return errors.New("origin name is empty or invalid")
	}
	if _, err := decodeIdentity("origin", target.Identity); err != nil {
		return err
	}
	endpoint, err := url.Parse(target.Endpoint)
	if err != nil || (endpoint.Scheme != "ws" && endpoint.Scheme != "wss") || endpoint.Host == "" || endpoint.Path == "" ||
		endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Opaque != "" || endpoint.ForceQuery || endpoint.RawPath != "" {
		return errors.New("origin endpoint must be an absolute ws/wss URL without credentials, query, fragment, or escaped path")
	}
	return nil
}

func (d *Distributor) distributeOne(ctx context.Context, bundle Bundle, target OriginTarget) error {
	connection, err := d.dial(ctx, target.Endpoint)
	if err != nil {
		return fmt.Errorf("dial control endpoint: %w", err)
	}
	defer connection.Close()

	hostRequestID, err := d.requestID()
	if err != nil {
		return fmt.Errorf("create host-info request ID: %w", err)
	}
	hostResponse, err := controlRoundTrip(ctx, connection, protocol.Control{Type: protocol.TypeHostInfo, RequestID: hostRequestID})
	if err != nil {
		return err
	}
	if hostResponse.Type != protocol.TypeHostInfoResult || hostResponse.Host == nil {
		return fmt.Errorf("host-info response has type %q or no host", hostResponse.Type)
	}
	if hostResponse.Host.ID != target.Identity || hostResponse.Host.MeshIdentity != target.Identity {
		return fmt.Errorf("origin identity changed: expected %q, received %q / %q", target.Identity, hostResponse.Host.ID, hostResponse.Host.MeshIdentity)
	}

	signed, err := SignBundle(bundle, target.Identity, d.environment, d.signer)
	if err != nil {
		return err
	}
	installRequestID, err := d.requestID()
	if err != nil {
		return fmt.Errorf("create certificate-install request ID: %w", err)
	}
	installResponse, err := controlRoundTrip(ctx, connection, protocol.Control{
		Type: protocol.TypeCertificateInstall, RequestID: installRequestID,
		Certificate: &protocol.CertificateInstall{
			Environment: string(signed.Environment),
			TargetID:    signed.TargetID, SignerID: signed.SignerID,
			CertificatePEM: signed.CertificatePEM, PrivateKeyPEM: signed.PrivateKeyPEM, Signature: signed.Signature,
		},
	})
	if err != nil {
		return err
	}
	if installResponse.Type != protocol.TypeCertificateInstalled {
		return fmt.Errorf("certificate-install response has type %q", installResponse.Type)
	}
	if installResponse.CertificateFingerprint != bundle.Fingerprint {
		return fmt.Errorf("origin installed certificate fingerprint %q, want %q", installResponse.CertificateFingerprint, bundle.Fingerprint)
	}
	if installResponse.CertificateEnvironment != string(d.environment) {
		return fmt.Errorf("origin installed certificate environment %q, want %q", installResponse.CertificateEnvironment, d.environment)
	}
	return nil
}

func controlRoundTrip(ctx context.Context, connection transport.Conn, request protocol.Control) (protocol.Control, error) {
	if request.RequestID == "" {
		return protocol.Control{}, errors.New("dnsname: empty certificate request ID")
	}
	payload, err := request.Encode()
	if err != nil {
		return protocol.Control{}, fmt.Errorf("dnsname: encode %s request: %w", request.Type, err)
	}
	stopCancellation := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stopCancellation()
	if err := connection.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload}); err != nil {
		return protocol.Control{}, fmt.Errorf("dnsname: write %s request: %w", request.Type, err)
	}
	frame, err := connection.ReadFrame()
	if err != nil {
		if ctx.Err() != nil {
			return protocol.Control{}, ctx.Err()
		}
		return protocol.Control{}, fmt.Errorf("dnsname: read %s response: %w", request.Type, err)
	}
	if frame.Kind != protocol.KindControl {
		return protocol.Control{}, fmt.Errorf("dnsname: %s response is not a control frame", request.Type)
	}
	response, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return protocol.Control{}, fmt.Errorf("dnsname: decode %s response: %w", request.Type, err)
	}
	if response.RequestID != request.RequestID {
		return protocol.Control{}, fmt.Errorf("dnsname: %s response request ID %q, want %q", request.Type, response.RequestID, request.RequestID)
	}
	if response.Type == protocol.TypeError {
		return protocol.Control{}, fmt.Errorf("dnsname: origin rejected %s: %s", request.Type, response.Message)
	}
	return response, nil
}

func randomRequestID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("dnsname: create random request ID: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}
