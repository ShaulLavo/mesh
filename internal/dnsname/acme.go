package dnsname

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/acme"
)

const (
	LetsEncryptProductionURL = acme.LetsEncryptURL
	LetsEncryptStagingURL    = "https://acme-staging-v02.api.letsencrypt.org/directory"

	defaultRenewBefore      = 30 * 24 * time.Hour
	defaultIssuanceTimeout  = 10 * time.Minute
	maximumIssuanceTimeout  = 30 * time.Minute
	defaultPropagationWait  = 2 * time.Minute
	defaultPropagationPoll  = 2 * time.Second
	defaultChallengeCleanup = 15 * time.Second
	maximumACMEResponse     = 2 << 20
)

// ErrACMEResponseTooLarge reports an ACME response outside Mesh's fixed
// external-input boundary.
var ErrACMEResponseTooLarge = errors.New("dnsname: ACME response is too large")

// ACMEClient is the modern RFC 8555 order flow used by Issuer. It exists so
// the complete order can be tested without a public CA.
type ACMEClient interface {
	Register(context.Context, *acme.Account, func(string) bool) (*acme.Account, error)
	GetReg(context.Context, string) (*acme.Account, error)
	AuthorizeOrder(context.Context, []acme.AuthzID, ...acme.OrderOption) (*acme.Order, error)
	GetAuthorization(context.Context, string) (*acme.Authorization, error)
	DNS01ChallengeRecord(string) (string, error)
	Accept(context.Context, *acme.Challenge) (*acme.Challenge, error)
	WaitAuthorization(context.Context, string) (*acme.Authorization, error)
	WaitOrder(context.Context, string) (*acme.Order, error)
	CreateOrderCert(context.Context, string, []byte, bool) ([][]byte, string, error)
	FetchCert(context.Context, string, bool) ([][]byte, error)
}

// ChallengeSolver owns DNS-01 presentation, authoritative propagation, and
// exact cleanup. T13 can reuse it for public-name orders.
type ChallengeSolver interface {
	Present(context.Context, string, string) (ChallengeRecord, error)
	Wait(context.Context, ChallengeRecord) error
	Cleanup(context.Context, ChallengeRecord) error
}

// DNS01Solver implements ChallengeSolver with a Provider and authoritative
// DNS observations.
type DNS01Solver struct {
	Provider           Provider
	Observer           TXTObserver
	Zone               string
	PropagationTimeout time.Duration
	PollInterval       time.Duration
	CleanupTimeout     time.Duration
}

// Present creates one comment-marked TXT record.
func (s DNS01Solver) Present(ctx context.Context, name, value string) (ChallengeRecord, error) {
	return PresentTXT(ctx, s.Provider, name, value)
}

// Wait blocks until every authoritative nameserver returns the value.
func (s DNS01Solver) Wait(ctx context.Context, challenge ChallengeRecord) error {
	if ctx == nil {
		return errors.New("dnsname: wait for DNS-01 with nil context")
	}
	timeout := s.PropagationTimeout
	if timeout == 0 {
		timeout = defaultPropagationWait
	}
	interval := s.PollInterval
	if interval == 0 {
		interval = defaultPropagationPoll
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return WaitForTXT(waitCtx, s.Observer, s.Zone, challenge.Name, challenge.Value, interval)
}

// Cleanup uses a separate bound so cancellation does not strand a TXT record.
func (s DNS01Solver) Cleanup(ctx context.Context, challenge ChallengeRecord) error {
	if ctx == nil {
		return errors.New("dnsname: clean DNS-01 with nil context")
	}
	timeout := s.CleanupTimeout
	if timeout == 0 {
		timeout = defaultChallengeCleanup
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	return CleanupTXT(cleanupCtx, s.Provider, challenge)
}

// IssuerConfig defines one ACME account, certificate name, and persisted state.
type IssuerConfig struct {
	DirectoryURL string
	Email        string
	StateDir     string
	Name         string
	AcceptTerms  bool
	RenewBefore  time.Duration
	Timeout      time.Duration
	Solver       ChallengeSolver
	HTTPClient   *http.Client
	Now          func() time.Time
	Random       io.Reader
	NewClient    func(crypto.Signer) ACMEClient
}

// Issuer renews one certificate with the ACME RFC 8555 order flow.
type Issuer struct {
	config    IssuerConfig
	store     *BundleStore
	renewGate chan struct{}
}

type accountState struct {
	DirectoryURL string   `json:"directoryUrl"`
	URI          string   `json:"uri"`
	Contact      []string `json:"contact"`
}

// NewIssuer validates config and opens its environment-specific bundle store.
func NewIssuer(config IssuerConfig) (*Issuer, error) {
	if config.DirectoryURL == "" {
		config.DirectoryURL = LetsEncryptProductionURL
	}
	directoryURL, err := url.Parse(config.DirectoryURL)
	if err != nil || directoryURL.Scheme != "https" || directoryURL.Host == "" || directoryURL.User != nil ||
		directoryURL.RawQuery != "" || directoryURL.Fragment != "" || directoryURL.Opaque != "" || directoryURL.ForceQuery {
		return nil, errors.New("dnsname: ACME directory must be an HTTPS URL without credentials, query, or fragment")
	}
	if strings.TrimSpace(config.Email) == "" || strings.ContainsAny(config.Email, "\r\n") {
		return nil, errors.New("dnsname: ACME account email is empty or invalid")
	}
	if !config.AcceptTerms {
		return nil, errors.New("dnsname: ACME terms must be accepted explicitly")
	}
	if config.Solver == nil {
		return nil, errors.New("dnsname: ACME challenge solver is nil")
	}
	if strings.TrimSpace(config.StateDir) == "" {
		return nil, errors.New("dnsname: ACME state directory is empty")
	}
	if config.Name == "" {
		config.Name = WildcardName
	}
	if _, err := certificateProbeName(config.Name); err != nil {
		return nil, err
	}
	if config.RenewBefore == 0 {
		config.RenewBefore = defaultRenewBefore
	}
	if config.RenewBefore <= 0 {
		return nil, errors.New("dnsname: renewal window must be positive")
	}
	if config.Timeout == 0 {
		config.Timeout = defaultIssuanceTimeout
	}
	if config.Timeout <= 0 || config.Timeout > maximumIssuanceTimeout {
		return nil, fmt.Errorf("dnsname: issuance timeout %s is outside (0,%s]", config.Timeout, maximumIssuanceTimeout)
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	config.HTTPClient = boundedACMEHTTPClient(config.HTTPClient)
	store, err := NewBundleStore(filepath.Join(config.StateDir, "bundle"), config.Name)
	if err != nil {
		return nil, err
	}
	store.now = config.Now
	renewGate := make(chan struct{}, 1)
	renewGate <- struct{}{}
	return &Issuer{config: config, store: store, renewGate: renewGate}, nil
}

func boundedACMEHTTPClient(client *http.Client) *http.Client {
	if client == nil {
		client = &http.Client{}
	}
	copy := *client
	transport := copy.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	copy.Transport = boundedACMERoundTripper{transport: transport}
	return &copy
}

type boundedACMERoundTripper struct {
	transport http.RoundTripper
}

func (t boundedACMERoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.transport.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	contents, readErr := io.ReadAll(io.LimitReader(response.Body, maximumACMEResponse+1))
	closeErr := response.Body.Close()
	if readErr != nil {
		return nil, errors.Join(fmt.Errorf("dnsname: read ACME response: %w", readErr), closeErr)
	}
	if len(contents) > maximumACMEResponse {
		return nil, errors.Join(ErrACMEResponseTooLarge, closeErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("dnsname: close ACME response: %w", closeErr)
	}
	response.Body = io.NopCloser(bytes.NewReader(contents))
	response.ContentLength = int64(len(contents))
	return response, nil
}

// Renew returns the current bundle or issues a new one when force is true or
// the certificate enters the renewal window.
func (i *Issuer) Renew(ctx context.Context, force bool) (bundle Bundle, renewed bool, resultErr error) {
	if ctx == nil {
		return Bundle{}, false, errors.New("dnsname: renew certificate with nil context")
	}
	ctx, cancel := context.WithTimeout(ctx, i.config.Timeout)
	defer cancel()
	select {
	case <-ctx.Done():
		return Bundle{}, false, fmt.Errorf("dnsname: wait for renewal: %w", ctx.Err())
	case <-i.renewGate:
	}
	defer func() {
		i.renewGate <- struct{}{}
	}()
	if err := ctx.Err(); err != nil {
		return Bundle{}, false, fmt.Errorf("dnsname: wait for renewal: %w", err)
	}

	if err := os.MkdirAll(i.config.StateDir, 0o700); err != nil {
		return Bundle{}, false, fmt.Errorf("dnsname: create ACME state directory: %w", err)
	}
	if err := os.Chmod(i.config.StateDir, 0o700); err != nil { //nolint:gosec // private directories require owner execute permission
		return Bundle{}, false, fmt.Errorf("dnsname: secure ACME state directory: %w", err)
	}
	lock, err := acquireRenewalLock(ctx, filepath.Join(i.config.StateDir, "renew.lock"))
	if err != nil {
		return Bundle{}, false, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, lock.release())
	}()

	existing, err := i.store.Load()
	hasExisting := err == nil
	failed := func(cause error) (Bundle, bool, error) {
		if hasExisting {
			return existing, false, cause
		}
		return Bundle{}, false, cause
	}
	if err == nil && !force && i.config.Now().UTC().Add(i.config.RenewBefore).Before(existing.NotAfter) {
		return existing, false, nil
	}
	if err != nil && !errors.Is(err, ErrNoCertificate) && !errors.Is(err, ErrCertificateExpired) {
		return Bundle{}, false, err
	}

	accountKey, err := loadOrCreateECDSAKey(filepath.Join(i.config.StateDir, "account.key"), i.config.Random)
	if err != nil {
		return failed(err)
	}
	certificateKey, err := loadOrCreateECDSAKey(filepath.Join(i.config.StateDir, "certificate.key"), i.config.Random)
	if err != nil {
		return failed(err)
	}
	client := i.client(accountKey)
	account, err := client.Register(ctx, &acme.Account{Contact: []string{"mailto:" + i.config.Email}}, acme.AcceptTOS)
	if errors.Is(err, acme.ErrAccountAlreadyExists) {
		account, err = client.GetReg(ctx, "")
	}
	if err != nil {
		return failed(fmt.Errorf("dnsname: register ACME account: %w", err))
	}
	if err := i.persistAccount(account); err != nil {
		return failed(err)
	}

	certificateDER, err := i.issue(ctx, client, certificateKey)
	if err != nil {
		return failed(err)
	}
	certificatePEM := make([]byte, 0)
	for _, certificate := range certificateDER {
		certificatePEM = append(certificatePEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate})...)
	}
	privateKeyPEM, err := marshalPrivateKey(certificateKey)
	if err != nil {
		return failed(err)
	}
	bundle, err = i.store.Install(certificatePEM, privateKeyPEM)
	if err != nil {
		return failed(fmt.Errorf("dnsname: persist issued certificate: %w", err))
	}
	return bundle, true, nil
}

func (i *Issuer) client(key *ecdsa.PrivateKey) ACMEClient {
	if i.config.NewClient != nil {
		return i.config.NewClient(key)
	}
	return &acme.Client{
		Key: key, DirectoryURL: i.config.DirectoryURL, HTTPClient: i.config.HTTPClient,
		UserAgent: "mesh/0 private-names",
	}
}

func (i *Issuer) issue(ctx context.Context, client ACMEClient, certificateKey *ecdsa.PrivateKey) ([][]byte, error) {
	order, err := client.AuthorizeOrder(ctx, []acme.AuthzID{{Type: "dns", Value: i.config.Name}})
	if err != nil {
		return nil, fmt.Errorf("dnsname: create ACME order for %s: %w", i.config.Name, err)
	}
	for _, authorizationURL := range order.AuthzURLs {
		if err := i.authorize(ctx, client, authorizationURL); err != nil {
			return nil, err
		}
	}
	ready, err := client.WaitOrder(ctx, order.URI)
	if err != nil {
		return nil, fmt.Errorf("dnsname: wait for ACME order: %w", err)
	}
	if ready.Status == acme.StatusValid && ready.CertURL != "" {
		certificates, err := client.FetchCert(ctx, ready.CertURL, true)
		if err != nil {
			return nil, fmt.Errorf("dnsname: fetch ACME certificate: %w", err)
		}
		return certificates, nil
	}
	request, err := x509.CreateCertificateRequest(i.config.Random, &x509.CertificateRequest{DNSNames: []string{i.config.Name}}, certificateKey)
	if err != nil {
		return nil, fmt.Errorf("dnsname: create certificate request: %w", err)
	}
	certificates, _, err := client.CreateOrderCert(ctx, ready.FinalizeURL, request, true)
	if err != nil {
		return nil, fmt.Errorf("dnsname: finalize ACME order: %w", err)
	}
	return certificates, nil
}

func (i *Issuer) authorize(ctx context.Context, client ACMEClient, authorizationURL string) (resultErr error) {
	authorization, err := client.GetAuthorization(ctx, authorizationURL)
	if err != nil {
		return fmt.Errorf("dnsname: get ACME authorization: %w", err)
	}
	if authorization.Status == acme.StatusValid {
		return nil
	}
	var challenge *acme.Challenge
	for _, candidate := range authorization.Challenges {
		if candidate.Type == "dns-01" {
			challenge = candidate
			break
		}
	}
	if challenge == nil {
		return fmt.Errorf("dnsname: authorization for %s offers no dns-01 challenge", authorization.Identifier.Value)
	}
	value, err := client.DNS01ChallengeRecord(challenge.Token)
	if err != nil {
		return fmt.Errorf("dnsname: compute DNS-01 value: %w", err)
	}
	identifier := strings.TrimPrefix(authorization.Identifier.Value, "*.")
	record, err := i.config.Solver.Present(ctx, "_acme-challenge."+identifier, value)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, i.config.Solver.Cleanup(ctx, record))
	}()
	if err := i.config.Solver.Wait(ctx, record); err != nil {
		return err
	}
	if _, err := client.Accept(ctx, challenge); err != nil {
		return fmt.Errorf("dnsname: accept DNS-01 challenge: %w", err)
	}
	if _, err := client.WaitAuthorization(ctx, authorization.URI); err != nil {
		return fmt.Errorf("dnsname: wait for ACME authorization: %w", err)
	}
	return nil
}

func (i *Issuer) persistAccount(account *acme.Account) error {
	if account == nil {
		return errors.New("dnsname: ACME server returned a nil account")
	}
	contents, err := json.MarshalIndent(accountState{
		DirectoryURL: i.config.DirectoryURL, URI: account.URI, Contact: append([]string(nil), account.Contact...),
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("dnsname: encode ACME account state: %w", err)
	}
	contents = append(contents, '\n')
	if err := writeAtomicFile(filepath.Join(i.config.StateDir, "account.json"), contents, 0o600); err != nil {
		return fmt.Errorf("dnsname: persist ACME account: %w", err)
	}
	return nil
}
