package dnsname

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/acme"
)

func TestIssuerBoundsACMEResponses(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), maximumACMEResponse+1))
	}))
	defer server.Close()
	issuer, err := NewIssuer(IssuerConfig{
		DirectoryURL: server.URL, Email: "owner@example.com", StateDir: t.TempDir(), Name: WildcardName,
		AcceptTerms: true, Solver: &fakeSolver{}, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	client, ok := issuer.client(key).(*acme.Client)
	if !ok {
		t.Fatal("issuer did not create the production ACME client")
	}
	_, err = client.Discover(context.Background())
	if !errors.Is(err, ErrACMEResponseTooLarge) {
		t.Fatalf("oversized ACME directory error = %v", err)
	}
}

func TestIssuerRejectsUnsafeDirectoryURLs(t *testing.T) {
	for _, directoryURL := range []string{
		"http://acme.example/directory",
		"https:///directory",
		"https://user:secret@acme.example/directory",
		"https://acme.example/directory?token=secret",
		"https://acme.example/directory#fragment",
		"https://acme.example/directory?",
	} {
		t.Run(directoryURL, func(t *testing.T) {
			_, err := NewIssuer(IssuerConfig{
				DirectoryURL: directoryURL,
				Email:        "owner@example.com",
				StateDir:     t.TempDir(),
				AcceptTerms:  true,
				Solver:       &fakeSolver{},
			})
			if err == nil {
				t.Fatal("unsafe ACME directory URL was accepted")
			}
		})
	}
}

func TestIssuerUsesModernOrderFlowAndCleansChallenge(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	solver := &fakeSolver{}
	fake := &fakeACME{t: t, now: now}
	issuer, err := NewIssuer(IssuerConfig{
		DirectoryURL: LetsEncryptStagingURL, Email: "owner@example.com", StateDir: filepath.Join(t.TempDir(), "staging"),
		Name: WildcardName, AcceptTerms: true, Solver: solver, Now: func() time.Time { return now }, Random: rand.Reader,
		NewClient: func(crypto.Signer) ACMEClient { return fake },
	})
	if err != nil {
		t.Fatal(err)
	}
	bundle, renewed, err := issuer.Renew(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !renewed || bundle.NotAfter.Before(now.Add(89*24*time.Hour)) {
		t.Fatalf("renewed = %v, bundle expiry = %s", renewed, bundle.NotAfter)
	}
	wantCalls := []string{"register", "authorize-order", "get-authorization", "dns-value", "accept", "wait-authorization", "wait-order", "create-order-cert"}
	if !reflect.DeepEqual(fake.calls, wantCalls) {
		t.Fatalf("ACME calls = %#v, want %#v", fake.calls, wantCalls)
	}
	if !reflect.DeepEqual(solver.calls, []string{"present", "wait", "cleanup"}) {
		t.Fatalf("solver calls = %#v", solver.calls)
	}
	if solver.record.Name != "_acme-challenge.mesh.shaulavo.dev" || solver.record.Value != "txt-token" {
		t.Fatalf("challenge record = %#v", solver.record)
	}

	if _, renewed, err := issuer.Renew(context.Background(), false); err != nil || renewed {
		t.Fatalf("second renewal = %v, renewed = %v", err, renewed)
	}
}

func TestIssuerCleansChallengeAfterPropagationFailure(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	solver := &fakeSolver{waitErr: errors.New("not propagated")}
	fake := &fakeACME{t: t, now: now}
	issuer, err := NewIssuer(IssuerConfig{
		DirectoryURL: LetsEncryptStagingURL, Email: "owner@example.com", StateDir: t.TempDir(), Name: WildcardName,
		AcceptTerms: true, Solver: solver, Now: func() time.Time { return now }, Random: rand.Reader,
		NewClient: func(crypto.Signer) ACMEClient { return fake },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := issuer.Renew(context.Background(), true); err == nil {
		t.Fatal("propagation failure succeeded")
	}
	if !reflect.DeepEqual(solver.calls, []string{"present", "wait", "cleanup"}) {
		t.Fatalf("solver calls = %#v", solver.calls)
	}
}

func TestIssuerSerializesOverlappingRenewals(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	solver := &fakeSolver{presented: make(chan struct{}), release: make(chan struct{})}
	firstACME := &fakeACME{t: t, now: now}
	stateDir := t.TempDir()
	issuer, err := NewIssuer(IssuerConfig{
		DirectoryURL: LetsEncryptStagingURL, Email: "owner@example.com", StateDir: stateDir, Name: WildcardName,
		AcceptTerms: true, Solver: solver, Now: func() time.Time { return now }, Random: rand.Reader,
		NewClient: func(crypto.Signer) ACMEClient { return firstACME },
	})
	if err != nil {
		t.Fatal(err)
	}
	secondACME := &fakeACME{t: t, now: now}
	secondIssuer, err := NewIssuer(IssuerConfig{
		DirectoryURL: LetsEncryptStagingURL, Email: "owner@example.com", StateDir: stateDir, Name: WildcardName,
		AcceptTerms: true, Solver: &fakeSolver{}, Now: func() time.Time { return now }, Random: rand.Reader,
		NewClient: func(crypto.Signer) ACMEClient { return secondACME },
	})
	if err != nil {
		t.Fatal(err)
	}

	result := make(chan error, 1)
	go func() {
		_, _, err := issuer.Renew(context.Background(), false)
		result <- err
	}()
	<-solver.presented

	sameIssuerCtx, cancelSameIssuer := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelSameIssuer()
	if _, _, err := issuer.Renew(sameIssuerCtx, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("same-issuer contention error = %v, want deadline exceeded", err)
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, _, err := secondIssuer.Renew(waitCtx, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contending renewal error = %v, want deadline exceeded", err)
	}
	if len(secondACME.calls) != 0 {
		t.Fatalf("contending issuer entered ACME flow: %#v", secondACME.calls)
	}

	close(solver.release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if _, renewed, err := secondIssuer.Renew(context.Background(), false); err != nil || renewed {
		t.Fatalf("renewal after lock release = %v, renewed = %v", err, renewed)
	}
	if len(secondACME.calls) != 0 {
		t.Fatalf("second issuer created a duplicate order: %#v", secondACME.calls)
	}
}

func TestIssuerRenewsExpiredPersistedCertificate(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	currentTime := now
	solver := &fakeSolver{}
	fake := &fakeACME{t: t, now: currentTime}
	issuer, err := NewIssuer(IssuerConfig{
		DirectoryURL: LetsEncryptStagingURL, Email: "owner@example.com", StateDir: t.TempDir(), Name: WildcardName,
		AcceptTerms: true, Solver: solver, Now: func() time.Time { return currentTime }, Random: rand.Reader,
		NewClient: func(crypto.Signer) ACMEClient { return fake },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, renewed, err := issuer.Renew(context.Background(), false); err != nil || !renewed {
		t.Fatalf("initial renewal = %v, renewed = %v", err, renewed)
	}
	currentTime = now.Add(91 * 24 * time.Hour)
	fake.now = currentTime
	if _, renewed, err := issuer.Renew(context.Background(), false); err != nil || !renewed {
		t.Fatalf("expired renewal = %v, renewed = %v", err, renewed)
	}
	orders := 0
	for _, call := range fake.calls {
		if call == "authorize-order" {
			orders++
		}
	}
	if orders != 2 {
		t.Fatalf("ACME orders = %d, want 2", orders)
	}
}

func TestIssuerReturnsUsableCurrentBundleWhenRenewalFails(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	currentTime := now
	fake := &fakeACME{t: t, now: now}
	issuer, err := NewIssuer(IssuerConfig{
		DirectoryURL: LetsEncryptStagingURL, Email: "owner@example.com", StateDir: t.TempDir(), Name: WildcardName,
		AcceptTerms: true, Solver: &fakeSolver{}, Now: func() time.Time { return currentTime }, Random: rand.Reader,
		NewClient: func(crypto.Signer) ACMEClient { return fake },
	})
	if err != nil {
		t.Fatal(err)
	}
	current, renewed, err := issuer.Renew(context.Background(), false)
	if err != nil || !renewed {
		t.Fatalf("initial renewal = %v, renewed = %v", err, renewed)
	}
	currentTime = now.Add(61 * 24 * time.Hour)
	fake.authorizeErr = errors.New("ACME unavailable")
	got, renewed, err := issuer.Renew(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "ACME unavailable") {
		t.Fatalf("renewal failure = %v", err)
	}
	if renewed || got.Fingerprint != current.Fingerprint {
		t.Fatalf("fallback bundle = %s, renewed = %v; want %s", got.Fingerprint, renewed, current.Fingerprint)
	}
}

func TestIssuerBoundsNeverCompletingACMEOrder(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	fake := &fakeACME{t: t, now: now, blockAuthorize: true}
	issuer, err := NewIssuer(IssuerConfig{
		DirectoryURL: LetsEncryptStagingURL, Email: "owner@example.com", StateDir: t.TempDir(), Name: WildcardName,
		AcceptTerms: true, Solver: &fakeSolver{}, Now: func() time.Time { return now }, Random: rand.Reader,
		Timeout: 50 * time.Millisecond, NewClient: func(crypto.Signer) ACMEClient { return fake },
	})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, _, err := issuer.Renew(context.Background(), false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked ACME renewal error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("blocked ACME renewal took %s", elapsed)
	}
}

type fakeSolver struct {
	calls     []string
	record    ChallengeRecord
	waitErr   error
	presented chan struct{}
	release   chan struct{}
}

func (s *fakeSolver) Present(_ context.Context, name, value string) (ChallengeRecord, error) {
	s.calls = append(s.calls, "present")
	s.record = ChallengeRecord{ID: "txt-id", Name: name, Value: value}
	if s.presented != nil {
		close(s.presented)
		<-s.release
	}
	return s.record, nil
}

func (s *fakeSolver) Wait(context.Context, ChallengeRecord) error {
	s.calls = append(s.calls, "wait")
	return s.waitErr
}

func (s *fakeSolver) Cleanup(context.Context, ChallengeRecord) error {
	s.calls = append(s.calls, "cleanup")
	return nil
}

type fakeACME struct {
	t              *testing.T
	now            time.Time
	calls          []string
	authorizeErr   error
	blockAuthorize bool
}

func (c *fakeACME) Register(context.Context, *acme.Account, func(string) bool) (*acme.Account, error) {
	c.calls = append(c.calls, "register")
	return &acme.Account{URI: "account", Contact: []string{"mailto:owner@example.com"}}, nil
}

func (c *fakeACME) GetReg(context.Context, string) (*acme.Account, error) {
	c.calls = append(c.calls, "get-reg")
	return &acme.Account{URI: "account"}, nil
}

func (c *fakeACME) AuthorizeOrder(ctx context.Context, identifiers []acme.AuthzID, _ ...acme.OrderOption) (*acme.Order, error) {
	c.calls = append(c.calls, "authorize-order")
	if c.blockAuthorize {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if c.authorizeErr != nil {
		return nil, c.authorizeErr
	}
	if len(identifiers) != 1 || identifiers[0].Value != WildcardName {
		c.t.Fatalf("identifiers = %#v", identifiers)
	}
	return &acme.Order{URI: "order", AuthzURLs: []string{"authorization"}}, nil
}

func (c *fakeACME) GetAuthorization(context.Context, string) (*acme.Authorization, error) {
	c.calls = append(c.calls, "get-authorization")
	return &acme.Authorization{
		URI: "authorization", Status: acme.StatusPending, Identifier: acme.AuthzID{Type: "dns", Value: PrivateZone}, Wildcard: true,
		Challenges: []*acme.Challenge{{Type: "dns-01", Token: "token"}},
	}, nil
}

func (c *fakeACME) DNS01ChallengeRecord(string) (string, error) {
	c.calls = append(c.calls, "dns-value")
	return "txt-token", nil
}

func (c *fakeACME) Accept(context.Context, *acme.Challenge) (*acme.Challenge, error) {
	c.calls = append(c.calls, "accept")
	return &acme.Challenge{Status: acme.StatusPending}, nil
}

func (c *fakeACME) WaitAuthorization(context.Context, string) (*acme.Authorization, error) {
	c.calls = append(c.calls, "wait-authorization")
	return &acme.Authorization{Status: acme.StatusValid}, nil
}

func (c *fakeACME) WaitOrder(context.Context, string) (*acme.Order, error) {
	c.calls = append(c.calls, "wait-order")
	return &acme.Order{Status: acme.StatusReady, FinalizeURL: "finalize"}, nil
}

func (c *fakeACME) CreateOrderCert(_ context.Context, _ string, csrDER []byte, _ bool) ([][]byte, string, error) {
	c.calls = append(c.calls, "create-order-cert")
	request, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return nil, "", err
	}
	if err := request.CheckSignature(); err != nil {
		return nil, "", err
	}
	key, ok := request.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return nil, "", errors.New("CSR key is not ECDSA")
	}
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, "", err
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(42), Subject: pkix.Name{CommonName: WildcardName}, DNSNames: request.DNSNames,
		NotBefore: c.now.Add(-time.Hour), NotAfter: c.now.Add(90 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key, caKey)
	return [][]byte{der}, "certificate", err
}

func (c *fakeACME) FetchCert(context.Context, string, bool) ([][]byte, error) {
	c.calls = append(c.calls, "fetch-cert")
	return nil, errors.New("unexpected FetchCert")
}
