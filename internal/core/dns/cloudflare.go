package dns

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// defaultCloudflareBaseURL is Cloudflare's API v4 endpoint.
const defaultCloudflareBaseURL = "https://api.cloudflare.com/client/v4"

// CloudflareConfig configures the "cloudflare" DNS driver (DNS-001) against
// one DNS zone.
//
// ZoneID is non-secret bundle driver config (bundle.DriverRef); APIToken is
// not — a caller resolves it through a keystore driver before building
// CloudflareConfig, the same split blob.S3Config draws for its
// SecretAccessKey.
type CloudflareConfig struct {
	ZoneID   string
	APIToken string

	// BaseURL overrides Cloudflare's API endpoint; empty uses the real
	// one. Tests point it at a local httptest.Server.
	BaseURL string
	// HTTPClient overrides the client used for API calls; nil uses
	// http.DefaultClient.
	HTTPClient *http.Client
}

// CloudflareDriver is the "cloudflare" DNS driver: Set and Delete against
// the Cloudflare DNS records API for one zone.
type CloudflareDriver struct {
	zoneID  string
	token   string
	baseURL string
	client  *http.Client
}

// NewCloudflare builds a CloudflareDriver from cfg.
func NewCloudflare(cfg CloudflareConfig) (*CloudflareDriver, error) {
	if cfg.ZoneID == "" {
		return nil, fmt.Errorf("dns: cloudflare: zone id is required")
	}
	if cfg.APIToken == "" {
		return nil, fmt.Errorf("dns: cloudflare: api token is required")
	}

	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = defaultCloudflareBaseURL
	}
	client := cfg.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}

	return &CloudflareDriver{
		zoneID:  cfg.ZoneID,
		token:   cfg.APIToken,
		baseURL: baseURL,
		client:  client,
	}, nil
}

// cfRecord is one DNS record as the Cloudflare API represents it.
type cfRecord struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
}

// cfError is one entry of a Cloudflare API error response.
type cfError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// cfResponse is the envelope every Cloudflare API v4 call returns.
type cfResponse struct {
	Success bool            `json:"success"`
	Errors  []cfError       `json:"errors"`
	Result  json.RawMessage `json:"result"`
}

// Set upserts record via the Cloudflare API (DNS-001): any existing record
// at that name, of any type, is deleted first, then the desired record is
// created. Deleting before creating (rather than updating in place) is
// what lets Set switch a record's type — e.g. from a CNAME standby target
// back to an A record — without a stale record of the old type left
// behind.
func (d *CloudflareDriver) Set(ctx context.Context, record, value string, ttl time.Duration) error {
	if err := validateSetArgs(record, value, ttl); err != nil {
		return err
	}

	existing, err := d.listRecords(ctx, record)
	if err != nil {
		return fmt.Errorf("dns: cloudflare: set %s: %w", record, err)
	}
	for _, rec := range existing {
		if err := d.deleteRecord(ctx, rec.ID); err != nil {
			return fmt.Errorf("dns: cloudflare: set %s: %w", record, err)
		}
	}

	body := cfRecord{
		Type:    recordType(value),
		Name:    record,
		Content: value,
		TTL:     int(ttl.Seconds()),
		Proxied: false,
	}
	if err := d.call(ctx, http.MethodPost, "/dns_records", body, nil); err != nil {
		return fmt.Errorf("dns: cloudflare: set %s: %w", record, err)
	}
	return nil
}

// Delete removes every record at name via the Cloudflare API (DNS-001).
func (d *CloudflareDriver) Delete(ctx context.Context, record string) error {
	if err := validateDeleteArgs(record); err != nil {
		return err
	}

	existing, err := d.listRecords(ctx, record)
	if err != nil {
		return fmt.Errorf("dns: cloudflare: delete %s: %w", record, err)
	}
	for _, rec := range existing {
		if err := d.deleteRecord(ctx, rec.ID); err != nil {
			return fmt.Errorf("dns: cloudflare: delete %s: %w", record, err)
		}
	}
	return nil
}

func (d *CloudflareDriver) listRecords(ctx context.Context, name string) ([]cfRecord, error) {
	var result []cfRecord
	path := "/dns_records?name=" + name
	if err := d.call(ctx, http.MethodGet, path, nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (d *CloudflareDriver) deleteRecord(ctx context.Context, id string) error {
	return d.call(ctx, http.MethodDelete, "/dns_records/"+id, nil, nil)
}

// call makes one Cloudflare API v4 request against zone d.zoneID and
// decodes its "result" into out, if out is non-nil.
func (d *CloudflareDriver) call(ctx context.Context, method, path string, body, out any) error {
	var reqBody io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reqBody = bytes.NewReader(encoded)
	}

	url := d.baseURL + "/zones/" + d.zoneID + path
	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+d.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	var envelope cfResponse
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return fmt.Errorf("decode response (status %d): %w", resp.StatusCode, err)
	}
	if !envelope.Success {
		return fmt.Errorf("api error (status %d): %s", resp.StatusCode, formatCFErrors(envelope.Errors))
	}
	if out != nil && len(envelope.Result) > 0 {
		if err := json.Unmarshal(envelope.Result, out); err != nil {
			return fmt.Errorf("decode result: %w", err)
		}
	}
	return nil
}

func formatCFErrors(errs []cfError) string {
	if len(errs) == 0 {
		return "no error detail returned"
	}
	msgs := make([]string, len(errs))
	for i, e := range errs {
		msgs[i] = fmt.Sprintf("%d: %s", e.Code, e.Message)
	}
	out := msgs[0]
	for _, m := range msgs[1:] {
		out += "; " + m
	}
	return out
}
