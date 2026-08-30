package dnsname

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func TestCloudflareUsesCurrentRecordEndpointsAndBearerToken(t *testing.T) {
	type requestLog struct {
		Method, Path, Authorization string
		Query                       url.Values
		Body                        cloudflareRecordBody
	}
	var requests []requestLog
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		logged := requestLog{Method: request.Method, Path: request.URL.Path, Authorization: request.Header.Get("Authorization"), Query: request.URL.Query()}
		if request.Body != nil {
			_ = json.NewDecoder(request.Body).Decode(&logged.Body)
		}
		requests = append(requests, logged)
		w.Header().Set("Content-Type", "application/json")
		switch request.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte(`{"success":true,"result":[],"result_info":{"page":1,"total_pages":1}}`))
		case http.MethodPost, http.MethodPatch:
			logged.Body.Content = `"challenge"`
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{
				"id": "record-id", "type": logged.Body.Type, "name": logged.Body.Name, "content": logged.Body.Content,
				"ttl": logged.Body.TTL, "proxied": logged.Body.Proxied, "comment": logged.Body.Comment,
			}})
		case http.MethodDelete:
			_, _ = w.Write([]byte(`{"success":true,"result":{"id":"record-id"}}`))
		}
	}))
	defer server.Close()

	provider, err := NewCloudflare(CloudflareConfig{ZoneID: "zone-id", APIToken: "secret-token", BaseURL: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := provider.ListRecords(ctx, "pc.mesh.shaulavo.dev", RecordA); err != nil {
		t.Fatal(err)
	}
	input := RecordInput{Type: RecordTXT, Name: "_acme-challenge.mesh.shaulavo.dev", Content: "challenge", TTL: 60, Comment: ManagedTXTRecordComment}
	created, err := provider.CreateRecord(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if created.Content != "challenge" {
		t.Fatalf("decoded TXT content = %q", created.Content)
	}
	if created.Comment != ManagedTXTRecordComment {
		t.Fatalf("decoded TXT comment = %q", created.Comment)
	}
	if _, err := provider.UpdateRecord(ctx, "record-id", input); err != nil {
		t.Fatal(err)
	}
	if err := provider.DeleteRecord(ctx, "record-id"); err != nil {
		t.Fatal(err)
	}

	if len(requests) != 4 {
		t.Fatalf("request count = %d, want 4", len(requests))
	}
	for _, request := range requests {
		if request.Authorization != "Bearer secret-token" {
			t.Fatalf("authorization = %q", request.Authorization)
		}
		if request.Path != "/client/v4/zones/zone-id/dns_records" && request.Path != "/client/v4/zones/zone-id/dns_records/record-id" {
			t.Fatalf("path = %q", request.Path)
		}
	}
	if requests[0].Query.Get("name.exact") != "pc.mesh.shaulavo.dev" || requests[0].Query.Get("type") != "A" {
		t.Fatalf("list query = %#v", requests[0].Query)
	}
	if requests[1].Body.Content != `"challenge"` || requests[1].Body.Proxied || requests[1].Body.Comment != ManagedTXTRecordComment {
		t.Fatalf("create body = %#v", requests[1].Body)
	}
	if !reflect.DeepEqual([]string{requests[0].Method, requests[1].Method, requests[2].Method, requests[3].Method}, []string{"GET", "POST", "PATCH", "DELETE"}) {
		t.Fatalf("methods = %#v", requests)
	}
}

func TestCloudflareBoundsAndRedactsFailures(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []map[string]any{{
					"code": 9109, "message": "ATTACKER\r\nnever-print-this other-secret 100.64.0.8" + strings.Repeat("x", 4096),
				}}})
			}))
			defer server.Close()
			provider, err := NewCloudflare(CloudflareConfig{ZoneID: "zone", APIToken: "never-print-this", BaseURL: server.URL, HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			_, err = provider.ListRecords(context.Background(), "pc.mesh.shaulavo.dev", RecordA)
			if err == nil || !contains(err.Error(), "request failed") {
				t.Fatalf("failure = %v", err)
			}
			for _, secret := range []string{"ATTACKER", "never-print-this", "other-secret", "100.64.0.8"} {
				if contains(err.Error(), secret) {
					t.Fatalf("Cloudflare failure leaked %q: %v", secret, err)
				}
			}
		})
	}
}

func TestCloudflareNeverLeaksMalformedRecordFields(t *testing.T) {
	marker := "ATTACKER\r\nother-secret 100.64.0.8"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"result": []cloudflareRecord{{
				ID: marker + strings.Repeat("x", 512), Type: "A", Name: "pc.mesh.shaulavo.dev", Content: "100.64.0.8",
			}},
			"result_info": map[string]int{"page": 1, "total_pages": 1},
		})
	}))
	defer server.Close()
	provider, err := NewCloudflare(CloudflareConfig{ZoneID: "zone", APIToken: "secret-token", BaseURL: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.ListRecords(context.Background(), "pc.mesh.shaulavo.dev", RecordA)
	if err == nil {
		t.Fatal("malformed record response succeeded")
	}
	for _, secret := range []string{"ATTACKER", "other-secret", "100.64.0.8", "secret-token"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("malformed record error leaked %q: %v", secret, err)
		}
	}
}

func TestCloudflareRejectsBearerTokenRedirectingBaseURLs(t *testing.T) {
	for _, baseURL := range []string{
		"https://user:password@api.cloudflare.com",
		"https://api.cloudflare.com/client/v4",
		"https://api.cloudflare.com?destination=other",
		"https://api.cloudflare.com#fragment",
	} {
		t.Run(baseURL, func(t *testing.T) {
			if _, err := NewCloudflare(CloudflareConfig{ZoneID: "zone", APIToken: "secret", BaseURL: baseURL}); err == nil {
				t.Fatalf("unsafe base URL %q succeeded", baseURL)
			}
		})
	}
}

func TestCloudflareRejectsNonCanonicalCredentials(t *testing.T) {
	for name, config := range map[string]CloudflareConfig{
		"zone whitespace":  {ZoneID: " zone-id", APIToken: "token"},
		"token whitespace": {ZoneID: "zone-id", APIToken: "token value"},
		"token newline":    {ZoneID: "zone-id", APIToken: "token\n"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewCloudflare(config); err == nil {
				t.Fatal("non-canonical Cloudflare credential was accepted")
			}
		})
	}
}

func TestCloudflareNeverForwardsBearerTokenAcrossRedirect(t *testing.T) {
	destinationCalls := 0
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		destinationCalls++
	}))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("source authorization = %q", request.Header.Get("Authorization"))
		}
		http.Redirect(w, request, destination.URL, http.StatusFound)
	}))
	defer source.Close()
	provider, err := NewCloudflare(CloudflareConfig{ZoneID: "zone", APIToken: "secret", BaseURL: source.URL, HTTPClient: source.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.ListRecords(context.Background(), "pc.mesh.shaulavo.dev", RecordA); err == nil {
		t.Fatal("redirect response succeeded")
	}
	if destinationCalls != 0 {
		t.Fatalf("redirect destination received %d requests", destinationCalls)
	}
}

func TestCloudflareRejectsMalformedListRecordsBeforeMutation(t *testing.T) {
	name := "pc.mesh.shaulavo.dev"
	base := cloudflareRecord{
		ID: "record-id", Type: string(RecordA), Name: name, Content: "100.64.0.2",
		TTL: DefaultRecordTTL, Proxied: false, Comment: ManagedARecordComment,
	}
	tests := []struct {
		name   string
		mutate func(*cloudflareRecord)
	}{
		{name: "empty ID", mutate: func(record *cloudflareRecord) { record.ID = "" }},
		{name: "oversized ID", mutate: func(record *cloudflareRecord) { record.ID = strings.Repeat("a", cloudflareRecordIDMax+1) }},
		{name: "non-canonical ID", mutate: func(record *cloudflareRecord) { record.ID = "record/id" }},
		{name: "wrong type", mutate: func(record *cloudflareRecord) { record.Type = string(RecordTXT) }},
		{name: "wrong name", mutate: func(record *cloudflareRecord) { record.Name = "other.mesh.shaulavo.dev" }},
		{name: "non-canonical name", mutate: func(record *cloudflareRecord) { record.Name += "." }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			record := base
			test.mutate(&record)
			writes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodGet {
					writes++
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"success": true, "result": []cloudflareRecord{record},
					"result_info": map[string]int{"page": 1, "total_pages": 1},
				})
			}))
			defer server.Close()
			provider, err := NewCloudflare(CloudflareConfig{ZoneID: "zone", APIToken: "secret", BaseURL: server.URL, HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			_, err = ReconcileHostA(context.Background(), provider, HostAddress{Name: "pc", Address: netip.MustParseAddr("100.64.0.3")})
			if err == nil {
				t.Fatal("malformed Cloudflare list result was trusted")
			}
			if writes != 0 {
				t.Fatalf("malformed list result caused %d writes", writes)
			}
		})
	}
}

func TestCloudflareRejectsMismatchedMutationResponses(t *testing.T) {
	input := RecordInput{
		Type: RecordA, Name: "pc.mesh.shaulavo.dev", Content: "100.64.0.3", TTL: DefaultRecordTTL,
		Comment: ManagedARecordComment,
	}
	base := cloudflareRecord{
		ID: "record-id", Type: string(input.Type), Name: input.Name, Content: input.Content,
		TTL: input.TTL, Proxied: input.Proxied, Comment: input.Comment,
	}
	tests := []struct {
		name   string
		mutate func(*cloudflareRecord)
	}{
		{name: "type", mutate: func(record *cloudflareRecord) { record.Type = string(RecordTXT) }},
		{name: "name", mutate: func(record *cloudflareRecord) { record.Name = "other.mesh.shaulavo.dev" }},
		{name: "content", mutate: func(record *cloudflareRecord) { record.Content = "100.64.0.4" }},
		{name: "ownership", mutate: func(record *cloudflareRecord) { record.Comment = "human" }},
		{name: "proxy", mutate: func(record *cloudflareRecord) { record.Proxied = true }},
		{name: "TTL", mutate: func(record *cloudflareRecord) { record.TTL++ }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			record := base
			test.mutate(&record)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": record})
			}))
			defer server.Close()
			provider, err := NewCloudflare(CloudflareConfig{ZoneID: "zone", APIToken: "secret", BaseURL: server.URL, HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := provider.CreateRecord(context.Background(), input); err == nil {
				t.Fatal("mismatched Cloudflare create response was trusted")
			}
		})
	}

	t.Run("update ID", func(t *testing.T) {
		record := base
		record.ID = "different-id"
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": record})
		}))
		defer server.Close()
		provider, err := NewCloudflare(CloudflareConfig{ZoneID: "zone", APIToken: "secret", BaseURL: server.URL, HTTPClient: server.Client()})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := provider.UpdateRecord(context.Background(), base.ID, input); err == nil {
			t.Fatal("mismatched Cloudflare update ID was trusted")
		}
	})

	t.Run("delete ID", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"success":true,"result":{"id":"different-id"}}`))
		}))
		defer server.Close()
		provider, err := NewCloudflare(CloudflareConfig{ZoneID: "zone", APIToken: "secret", BaseURL: server.URL, HTTPClient: server.Client()})
		if err != nil {
			t.Fatal(err)
		}
		if err := provider.DeleteRecord(context.Background(), base.ID); err == nil {
			t.Fatal("mismatched Cloudflare delete ID was trusted")
		}
	})
}

func TestCloudflareRejectsRepeatedPagesAndDuplicateIDsBeforeMutation(t *testing.T) {
	for _, test := range []struct {
		name           string
		secondPage     int
		secondRecordID string
	}{
		{name: "repeated page", secondPage: 1, secondRecordID: "record-two"},
		{name: "duplicate ID", secondPage: 2, secondRecordID: "record-one"},
	} {
		t.Run(test.name, func(t *testing.T) {
			writes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodGet {
					writes++
				}
				requestedPage := request.URL.Query().Get("page")
				page := 1
				id := "record-one"
				if requestedPage == "2" {
					page = test.secondPage
					id = test.secondRecordID
				}
				record := cloudflareRecord{
					ID: id, Type: string(RecordA), Name: "pc.mesh.shaulavo.dev", Content: "100.64.0.2",
					TTL: DefaultRecordTTL, Comment: ManagedARecordComment,
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"success": true, "result": []cloudflareRecord{record},
					"result_info": map[string]int{"page": page, "total_pages": 2},
				})
			}))
			defer server.Close()
			provider, err := NewCloudflare(CloudflareConfig{ZoneID: "zone", APIToken: "secret", BaseURL: server.URL, HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			_, err = ReconcileHostA(context.Background(), provider, HostAddress{Name: "pc", Address: netip.MustParseAddr("100.64.0.3")})
			if err == nil {
				t.Fatal("malformed Cloudflare pagination was trusted")
			}
			if writes != 0 {
				t.Fatalf("malformed pagination caused %d writes", writes)
			}
		})
	}
}

func contains(value, part string) bool {
	for i := 0; i+len(part) <= len(value); i++ {
		if value[i:i+len(part)] == part {
			return true
		}
	}
	return false
}
