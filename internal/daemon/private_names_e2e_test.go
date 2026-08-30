package daemon

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/acme"

	"github.com/shaul/mesh/internal/dnsname"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/tailnet"
	"github.com/shaul/mesh/internal/transport"
)

func TestPrivateNamesStagingComposesACMECloudflareAndWebSocketDistribution(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	targetID, _ := composedIdentity(t)
	signerID, signer := composedIdentity(t)

	originState := t.TempDir()
	liveStore, err := dnsname.NewBundleStore(filepath.Join(originState, "live"), dnsname.WildcardName)
	if err != nil {
		t.Fatal(err)
	}
	liveSource, err := dnsname.NewCertificateSource(liveStore)
	if err != nil {
		t.Fatal(err)
	}
	privateName, err := dnsname.NewPrivateNameSource(liveStore)
	if err != nil {
		t.Fatal(err)
	}
	stagingStore, err := dnsname.NewBundleStore(filepath.Join(originState, "staging"), dnsname.WildcardName)
	if err != nil {
		t.Fatal(err)
	}
	installer, err := dnsname.NewInstaller(dnsname.InstallerConfig{
		Profile: dnsname.ProfilePrivateOrigin, LiveSource: liveSource, StagingStore: stagingStore, PrivateName: privateName,
		TargetID: targetID, SignerID: signerID,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	certificateControl, err := newCertificateController(map[dnsname.CertificateProfile]certificateInstaller{dnsname.ProfilePrivateOrigin: installer})
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, err := newLifecycle(lifecycleConfig{
		Catalog: &serverTestCatalog{}, Connector: failingServerTestConnector(),
		Host:        storage.Host{ID: storage.HostID(targetID), MeshIdentity: targetID, LastSeenAt: now},
		SessionsDir: filepath.Join(originState, "sessions"),
	})
	if err != nil {
		t.Fatal(err)
	}
	clientServer, err := newClientServer(lifecycle, failingServerTestConnector(), disabledEdgeController{}, noServiceControl{}, certificateControl)
	if err != nil {
		t.Fatal(err)
	}

	controlPort := reserveTCPPort(t, "127.0.0.1")
	daemonCtx, stopDaemon := context.WithCancel(context.Background())
	daemonDone := runRuntime(t, daemonCtx, ListenerConfig{
		StateDir: originState, TailnetAddrs: []string{"127.0.0.1"}, TailnetPort: controlPort, WebSocketPath: "/control/ws",
	}, clientServer.Handle)
	waitForHTTPStatus(t, fmt.Sprintf("http://127.0.0.1:%d/not-control", controlPort), http.StatusNotFound)
	t.Cleanup(func() {
		stopDaemon()
		if err := waitRuntime(t, daemonDone); err != nil {
			t.Errorf("stop composed daemon: %v", err)
		}
	})

	cloudflareAPI := newComposedCloudflareAPI(t)
	provider, err := dnsname.NewCloudflare(dnsname.CloudflareConfig{
		ZoneID: "zone-id", APIToken: "test-token", BaseURL: cloudflareAPI.server.URL, HTTPClient: cloudflareAPI.server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := dnsname.NewIssuer(dnsname.IssuerConfig{
		DirectoryURL: dnsname.LetsEncryptStagingURL, Email: "owner@example.com", StateDir: filepath.Join(t.TempDir(), "issuer"),
		Name: dnsname.WildcardName, AcceptTerms: true, Timeout: 2 * time.Second, Now: func() time.Time { return now },
		Solver: dnsname.DNS01Solver{
			Provider: provider, Observer: composedTXTObserver{api: cloudflareAPI}, Zone: dnsname.Zone,
			PropagationTimeout: time.Second, PollInterval: time.Millisecond,
		},
		NewClient: func(crypto.Signer) dnsname.ACMEClient {
			return &composedACME{now: now, api: cloudflareAPI}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	requestNumber := 0
	distributor, err := dnsname.NewDistributor(dnsname.DistributorConfig{
		Profile: dnsname.ProfilePrivateOrigin, Signer: signer, Environment: dnsname.EnvironmentStaging,
		Now: func() time.Time { return now },
		RequestID: func() (string, error) {
			requestNumber++
			return fmt.Sprintf("composed-%d", requestNumber), nil
		},
		Dial: func(ctx context.Context, endpoint string) (transport.Conn, error) {
			want := fmt.Sprintf("ws://100.64.0.9:%d/control/ws", controlPort)
			if endpoint != want {
				return nil, fmt.Errorf("manager endpoint %q, want %q", endpoint, want)
			}
			return transport.Dial(ctx, fmt.Sprintf("ws://127.0.0.1:%d/control/ws", controlPort), transport.DialOptions{})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := dnsname.NewPrivateNamesManager(dnsname.PrivateNamesManagerConfig{
		Provider: provider, Renewer: issuer, Distributor: distributor, PassTimeout: 3 * time.Second,
		Origins: []dnsname.PrivateOrigin{{
			Name: "origin", TailscaleName: "origin.example.ts.net", Identity: targetID,
			ControlPort: controlPort, WebSocketPath: "/control/ws",
		}},
		DiscoverSelf: func(context.Context) (tailnet.Peer, error) {
			return tailnet.Peer{Name: "origin.example.ts.net", Addrs: []string{"100.64.0.9"}}, nil
		},
		DiscoverPeers: func(context.Context) ([]tailnet.Peer, error) { return nil, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.RunOnce(context.Background(), false); err != nil {
		t.Fatal(err)
	}

	addressRecords := cloudflareAPI.recordsFor("origin.mesh.shaulavo.dev", "A")
	if len(addressRecords) != 1 || addressRecords[0].Content != "100.64.0.9" || addressRecords[0].Proxied || addressRecords[0].Comment != dnsname.ManagedARecordComment {
		t.Fatalf("composed A records = %#v", addressRecords)
	}
	if records := cloudflareAPI.recordsFor("_acme-challenge.mesh.shaulavo.dev", "TXT"); len(records) != 0 {
		t.Fatalf("DNS-01 records after exact cleanup = %#v", records)
	}
	staged, err := stagingStore.Load()
	if err != nil || staged.Fingerprint == "" {
		t.Fatalf("staging install = %s, %v", staged.Fingerprint, err)
	}
	if _, err := liveSource.GetCertificate(nil); !errors.Is(err, dnsname.ErrNoCertificate) {
		t.Fatalf("staging certificate reached live TLS source: %v", err)
	}
}

func composedIdentity(t *testing.T) (string, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(public), private
}

type composedCloudflareRecord struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
	Comment string `json:"comment"`
}

type composedCloudflareAPI struct {
	server  *httptest.Server
	mu      sync.Mutex
	nextID  int
	records map[string]composedCloudflareRecord
}

func newComposedCloudflareAPI(t *testing.T) *composedCloudflareAPI {
	t.Helper()
	api := &composedCloudflareAPI{records: make(map[string]composedCloudflareRecord)}
	api.server = httptest.NewServer(http.HandlerFunc(api.serveHTTP))
	t.Cleanup(api.server.Close)
	return api
}

func (a *composedCloudflareAPI) serveHTTP(w http.ResponseWriter, request *http.Request) {
	const basePath = "/client/v4/zones/zone-id/dns_records"
	w.Header().Set("Content-Type", "application/json")
	if !strings.HasPrefix(request.URL.Path, basePath) {
		http.NotFound(w, request)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	write := func(result any) { _ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": result}) }
	switch request.Method {
	case http.MethodGet:
		name, recordType := request.URL.Query().Get("name.exact"), request.URL.Query().Get("type")
		records := make([]composedCloudflareRecord, 0)
		for _, record := range a.records {
			if record.Name == name && record.Type == recordType {
				records = append(records, record)
			}
		}
		sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
		// Cloudflare reports an empty listing as total_pages 0, not 1. Mirroring
		// that here keeps the read-before-write path honest; a mock that always
		// says 1 hides the case every first-run request actually hits.
		totalPages := 1
		if len(records) == 0 {
			totalPages = 0
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true, "result": records, "result_info": map[string]int{"page": 1, "total_pages": totalPages},
		})
	case http.MethodPost, http.MethodPatch:
		var record composedCloudflareRecord
		if err := json.NewDecoder(request.Body).Decode(&record); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if request.Method == http.MethodPost {
			a.nextID++
			record.ID = fmt.Sprintf("record-%d", a.nextID)
		} else {
			record.ID = strings.TrimPrefix(request.URL.Path, basePath+"/")
		}
		a.records[record.ID] = record
		write(record)
	case http.MethodDelete:
		id := strings.TrimPrefix(request.URL.Path, basePath+"/")
		delete(a.records, id)
		write(map[string]string{"id": id})
	default:
		http.Error(w, "unsupported method", http.StatusMethodNotAllowed)
	}
}

func (a *composedCloudflareAPI) recordsFor(name, recordType string) []composedCloudflareRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	records := make([]composedCloudflareRecord, 0)
	for _, record := range a.records {
		if record.Name == name && record.Type == recordType {
			records = append(records, record)
		}
	}
	return records
}

type composedTXTObserver struct {
	api *composedCloudflareAPI
}

func (o composedTXTObserver) ObserveTXT(_ context.Context, _ string, name string) ([]dnsname.TXTObservation, error) {
	records := o.api.recordsFor(name, "TXT")
	values := make([]string, 0, len(records))
	for _, record := range records {
		value, err := strconv.Unquote(record.Content)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return []dnsname.TXTObservation{
		{Server: "ada.ns.cloudflare.com", Values: values},
		{Server: "bob.ns.cloudflare.com", Values: append([]string(nil), values...)},
	}, nil
}

type composedACME struct {
	now time.Time
	api *composedCloudflareAPI
}

func (*composedACME) Register(context.Context, *acme.Account, func(string) bool) (*acme.Account, error) {
	return &acme.Account{URI: "account", Contact: []string{"mailto:owner@example.com"}}, nil
}

func (*composedACME) GetReg(context.Context, string) (*acme.Account, error) {
	return &acme.Account{URI: "account"}, nil
}

func (*composedACME) AuthorizeOrder(_ context.Context, identifiers []acme.AuthzID, _ ...acme.OrderOption) (*acme.Order, error) {
	if len(identifiers) != 1 || identifiers[0].Type != "dns" || identifiers[0].Value != dnsname.WildcardName {
		return nil, fmt.Errorf("unexpected identifiers %#v", identifiers)
	}
	return &acme.Order{URI: "order", AuthzURLs: []string{"authorization"}}, nil
}

func (*composedACME) GetAuthorization(context.Context, string) (*acme.Authorization, error) {
	return &acme.Authorization{
		URI: "authorization", Status: acme.StatusPending,
		Identifier: acme.AuthzID{Type: "dns", Value: dnsname.PrivateZone}, Wildcard: true,
		Challenges: []*acme.Challenge{{Type: "dns-01", Token: "challenge-token"}},
	}, nil
}

func (*composedACME) DNS01ChallengeRecord(string) (string, error) { return "authoritative-value", nil }

func (c *composedACME) Accept(context.Context, *acme.Challenge) (*acme.Challenge, error) {
	records := c.api.recordsFor("_acme-challenge.mesh.shaulavo.dev", "TXT")
	for _, record := range records {
		value, err := strconv.Unquote(record.Content)
		if err == nil && value == "authoritative-value" && record.Comment == dnsname.ManagedTXTRecordComment {
			return &acme.Challenge{Status: acme.StatusPending}, nil
		}
	}
	return nil, errors.New("ACME challenge was accepted before managed TXT presentation")
}

func (*composedACME) WaitAuthorization(context.Context, string) (*acme.Authorization, error) {
	return &acme.Authorization{Status: acme.StatusValid}, nil
}

func (*composedACME) WaitOrder(context.Context, string) (*acme.Order, error) {
	return &acme.Order{Status: acme.StatusReady, FinalizeURL: "finalize"}, nil
}

func (c *composedACME) CreateOrderCert(_ context.Context, _ string, csrDER []byte, _ bool) ([][]byte, string, error) {
	request, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return nil, "", err
	}
	if err := request.CheckSignature(); err != nil {
		return nil, "", err
	}
	publicKey, ok := request.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return nil, "", errors.New("CSR key is not ECDSA")
	}
	issuerKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, "", err
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1200), Subject: pkix.Name{CommonName: dnsname.WildcardName}, DNSNames: request.DNSNames,
		NotBefore: c.now.Add(-time.Hour), NotAfter: c.now.Add(90 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	certificate, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, issuerKey)
	return [][]byte{certificate}, "certificate", err
}

func (*composedACME) FetchCert(context.Context, string, bool) ([][]byte, error) {
	return nil, errors.New("unexpected certificate fetch")
}
