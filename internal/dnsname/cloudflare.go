package dnsname

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	cloudflareAPIURL       = "https://api.cloudflare.com"
	cloudflareAPIPath      = "/client/v4"
	cloudflareResponseMax  = 2 << 20
	cloudflarePageSize     = 100
	cloudflareMaximumPages = 100
	cloudflareRecordIDMax  = 128
)

// CloudflareConfig identifies one zone-scoped DNS Write API token.
type CloudflareConfig struct {
	ZoneID     string
	APIToken   string
	BaseURL    string
	HTTPClient *http.Client
}

// Cloudflare implements Provider with the Cloudflare v4 REST API.
type Cloudflare struct {
	zoneID string
	token  string
	base   *url.URL
	client *http.Client
}

type cloudflareRecord struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
	Comment string `json:"comment"`
}

type cloudflareError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type cloudflareEnvelope[T any] struct {
	Success    bool              `json:"success"`
	Result     T                 `json:"result"`
	Errors     []cloudflareError `json:"errors"`
	ResultInfo struct {
		Page       int `json:"page"`
		TotalPages int `json:"total_pages"`
	} `json:"result_info"`
}

type cloudflareRecordBody struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
	Comment string `json:"comment"`
}

// NewCloudflare validates config without making a network request.
func NewCloudflare(config CloudflareConfig) (*Cloudflare, error) {
	if strings.TrimSpace(config.ZoneID) == "" || strings.TrimSpace(config.ZoneID) != config.ZoneID || len(config.ZoneID) > 32 {
		return nil, errors.New("dnsname: Cloudflare zone ID is empty, non-canonical, or longer than 32 characters")
	}
	if config.APIToken == "" || len(config.APIToken) > cloudflareTokenMaximum || strings.TrimSpace(config.APIToken) != config.APIToken || strings.ContainsAny(config.APIToken, "\x00\r\n\t ") {
		return nil, errors.New("dnsname: Cloudflare API token is empty, too large, or contains whitespace")
	}
	baseURL := config.BaseURL
	if baseURL == "" {
		baseURL = cloudflareAPIURL
	}
	base, err := url.Parse(baseURL)
	if err != nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" ||
		base.RawPath != "" || base.Opaque != "" || base.ForceQuery || base.Path != "" && base.Path != "/" {
		return nil, fmt.Errorf("dnsname: invalid Cloudflare API URL %q", baseURL)
	}
	if base.Scheme != "https" && !(base.Scheme == "http" && isLoopbackHost(base.Hostname())) {
		return nil, fmt.Errorf("dnsname: Cloudflare API URL %q must use HTTPS", baseURL)
	}
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	clientCopy := *client
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Cloudflare{zoneID: config.ZoneID, token: config.APIToken, base: base, client: &clientCopy}, nil
}

// ListRecords returns exact-name records of one type across every API page.
func (c *Cloudflare) ListRecords(ctx context.Context, name string, recordType RecordType) ([]Record, error) {
	if ctx == nil {
		return nil, errors.New("dnsname: Cloudflare list with nil context")
	}
	canonicalName, err := canonicalDNSName(name)
	if err != nil || canonicalName != name {
		return nil, fmt.Errorf("dnsname: Cloudflare list name %q is not canonical", name)
	}
	if err := validateCloudflareRecordType(recordType); err != nil {
		return nil, err
	}
	var records []Record
	seenIDs := make(map[string]struct{})
	totalPages := 0
	for page := 1; page <= cloudflareMaximumPages; page++ {
		query := url.Values{
			"name.exact": {name}, "type": {string(recordType)}, "page": {strconv.Itoa(page)},
			"per_page": {strconv.Itoa(cloudflarePageSize)},
		}
		var response cloudflareEnvelope[[]cloudflareRecord]
		if err := c.request(ctx, http.MethodGet, "", query, nil, &response); err != nil {
			return nil, err
		}
		if response.ResultInfo.Page != page || response.ResultInfo.TotalPages < page || response.ResultInfo.TotalPages > cloudflareMaximumPages || len(response.Result) > cloudflarePageSize {
			return nil, fmt.Errorf("dnsname: Cloudflare returned invalid pagination for record page %d", page)
		}
		if totalPages == 0 {
			totalPages = response.ResultInfo.TotalPages
		} else if response.ResultInfo.TotalPages != totalPages {
			return nil, errors.New("dnsname: Cloudflare record page count changed during listing")
		}
		for _, record := range response.Result {
			converted, err := recordFromCloudflare(record)
			if err != nil {
				return nil, err
			}
			if converted.Name != canonicalName || converted.Type != recordType {
				return nil, fmt.Errorf("dnsname: Cloudflare returned %s record %q for exact %s record %q", converted.Type, converted.Name, recordType, canonicalName)
			}
			if _, duplicate := seenIDs[converted.ID]; duplicate {
				return nil, fmt.Errorf("dnsname: Cloudflare returned duplicate record ID %q", converted.ID)
			}
			seenIDs[converted.ID] = struct{}{}
			records = append(records, converted)
		}
		if response.ResultInfo.TotalPages <= page {
			return records, nil
		}
	}
	return nil, fmt.Errorf("dnsname: Cloudflare record list exceeded %d pages", cloudflareMaximumPages)
}

// CreateRecord creates one complete record.
func (c *Cloudflare) CreateRecord(ctx context.Context, input RecordInput) (Record, error) {
	if err := validateCloudflareRecordInput(input); err != nil {
		return Record{}, err
	}
	var response cloudflareEnvelope[cloudflareRecord]
	if err := c.request(ctx, http.MethodPost, "", nil, cloudflareBody(input), &response); err != nil {
		return Record{}, err
	}
	record, err := recordFromCloudflare(response.Result)
	if err != nil {
		return Record{}, err
	}
	if !recordMatches(record, input) {
		return Record{}, fmt.Errorf("dnsname: Cloudflare create response does not match the requested %s record %q", input.Type, input.Name)
	}
	return record, nil
}

// UpdateRecord patches one complete record by provider ID.
func (c *Cloudflare) UpdateRecord(ctx context.Context, id string, input RecordInput) (Record, error) {
	if err := validateCloudflareRecordID(id); err != nil {
		return Record{}, fmt.Errorf("dnsname: Cloudflare update: %w", err)
	}
	if err := validateCloudflareRecordInput(input); err != nil {
		return Record{}, err
	}
	var response cloudflareEnvelope[cloudflareRecord]
	if err := c.request(ctx, http.MethodPatch, "/"+url.PathEscape(id), nil, cloudflareBody(input), &response); err != nil {
		return Record{}, err
	}
	record, err := recordFromCloudflare(response.Result)
	if err != nil {
		return Record{}, err
	}
	if record.ID != id || !recordMatches(record, input) {
		return Record{}, fmt.Errorf("dnsname: Cloudflare update response does not match record %q", id)
	}
	return record, nil
}

// DeleteRecord deletes one record by provider ID.
func (c *Cloudflare) DeleteRecord(ctx context.Context, id string) error {
	if err := validateCloudflareRecordID(id); err != nil {
		return fmt.Errorf("dnsname: Cloudflare delete: %w", err)
	}
	var response cloudflareEnvelope[struct {
		ID string `json:"id"`
	}]
	if err := c.request(ctx, http.MethodDelete, "/"+url.PathEscape(id), nil, nil, &response); err != nil {
		return err
	}
	if err := validateCloudflareRecordID(response.Result.ID); err != nil {
		return fmt.Errorf("dnsname: Cloudflare delete response: %w", err)
	}
	if response.Result.ID != id {
		return fmt.Errorf("dnsname: Cloudflare delete response ID %q does not match record %q", response.Result.ID, id)
	}
	return nil
}

func (c *Cloudflare) request(ctx context.Context, method, suffix string, query url.Values, body any, output any) error {
	if ctx == nil {
		return errors.New("dnsname: Cloudflare request with nil context")
	}
	endpoint := *c.base
	endpoint.Path = cloudflareAPIPath + "/zones/" + url.PathEscape(c.zoneID) + "/dns_records" + suffix
	endpoint.RawQuery = query.Encode()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("dnsname: encode Cloudflare request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), reader)
	if err != nil {
		return fmt.Errorf("dnsname: build Cloudflare request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.client.Do(request)
	if err != nil {
		return fmt.Errorf("dnsname: call Cloudflare %s %s: %w", method, endpoint.Path, err)
	}
	defer response.Body.Close() //nolint:errcheck // request result is already decided
	limited := io.LimitReader(response.Body, cloudflareResponseMax+1)
	contents, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Errorf("dnsname: read Cloudflare response: %w", err)
	}
	if len(contents) > cloudflareResponseMax {
		return fmt.Errorf("dnsname: Cloudflare response exceeds %d bytes", cloudflareResponseMax)
	}
	if err := json.Unmarshal(contents, output); err != nil {
		return fmt.Errorf("dnsname: decode Cloudflare HTTP %d response: %w", response.StatusCode, err)
	}
	success, failures := cloudflareResult(output, c.token)
	if response.StatusCode < 200 || response.StatusCode >= 300 || !success {
		return fmt.Errorf("dnsname: Cloudflare HTTP %d: %s", response.StatusCode, failures)
	}
	return nil
}

func cloudflareResult(value any, token string) (bool, string) {
	encoded, _ := json.Marshal(value)
	var envelope struct {
		Success bool              `json:"success"`
		Errors  []cloudflareError `json:"errors"`
	}
	_ = json.Unmarshal(encoded, &envelope)
	parts := make([]string, 0, len(envelope.Errors))
	for _, failure := range envelope.Errors {
		message := strings.ReplaceAll(failure.Message, token, "[redacted]")
		parts = append(parts, fmt.Sprintf("%d %s", failure.Code, message))
	}
	if len(parts) == 0 {
		return envelope.Success, "request failed without an API error"
	}
	return envelope.Success, strings.Join(parts, "; ")
}

func cloudflareBody(input RecordInput) cloudflareRecordBody {
	content := input.Content
	if input.Type == RecordTXT {
		content = strconv.Quote(content)
	}
	return cloudflareRecordBody{
		Type: string(input.Type), Name: input.Name, Content: content, TTL: input.TTL,
		Proxied: input.Proxied, Comment: input.Comment,
	}
}

func recordFromCloudflare(record cloudflareRecord) (Record, error) {
	if err := validateCloudflareRecordID(record.ID); err != nil {
		return Record{}, fmt.Errorf("dnsname: Cloudflare record: %w", err)
	}
	recordType := RecordType(record.Type)
	if err := validateCloudflareRecordType(recordType); err != nil {
		return Record{}, err
	}
	canonicalName, err := canonicalDNSName(record.Name)
	if err != nil || canonicalName != record.Name {
		return Record{}, fmt.Errorf("dnsname: Cloudflare record %q has non-canonical name %q", record.ID, record.Name)
	}
	content := record.Content
	if recordType == RecordTXT && strings.HasPrefix(content, "\"") {
		decoded, err := strconv.Unquote(content)
		if err != nil {
			return Record{}, fmt.Errorf("dnsname: decode Cloudflare TXT record %s: %w", record.ID, err)
		}
		content = decoded
	}
	return Record{
		ID: record.ID, Type: recordType, Name: record.Name,
		Content: content, TTL: record.TTL, Proxied: record.Proxied, Comment: record.Comment,
	}, nil
}

func validateCloudflareRecordInput(input RecordInput) error {
	if err := validateCloudflareRecordType(input.Type); err != nil {
		return err
	}
	canonicalName, err := canonicalDNSName(input.Name)
	if err != nil || canonicalName != input.Name {
		return fmt.Errorf("dnsname: Cloudflare record name %q is not canonical", input.Name)
	}
	return nil
}

func validateCloudflareRecordType(recordType RecordType) error {
	switch recordType {
	case RecordA, RecordTXT:
		return nil
	default:
		return fmt.Errorf("dnsname: Cloudflare record type %q is unsupported", recordType)
	}
}

func validateCloudflareRecordID(id string) error {
	if id == "" || len(id) > cloudflareRecordIDMax {
		return fmt.Errorf("record ID is empty or longer than %d characters", cloudflareRecordIDMax)
	}
	for _, character := range id {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '-' || character == '_' {
			continue
		}
		return fmt.Errorf("record ID %q is not canonical", id)
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}
