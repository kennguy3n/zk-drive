package fabric

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is an HTTP client for the zk-object-fabric console API. It
// covers the placement-policy endpoints used by the ZK Drive admin
// surface; richer endpoints (bucket creation, key rotation) can be
// added on demand.
type Client struct {
	baseURL    string
	adminToken string
	httpc      *http.Client
}

// ClientConfig configures a Client. BaseURL is required; AdminToken
// is optional but required by the console for the placement
// endpoints.
type ClientConfig struct {
	BaseURL    string
	AdminToken string
	HTTPClient *http.Client
}

// NewClient builds a fabric console Client.
func NewClient(cfg ClientConfig) *Client {
	httpc := cfg.HTTPClient
	if httpc == nil {
		httpc = &http.Client{Timeout: 10 * time.Second}
	}
	return &Client{
		baseURL:    strings.TrimRight(cfg.BaseURL, "/"),
		adminToken: cfg.AdminToken,
		httpc:      httpc,
	}
}

// ErrNotConfigured is returned when a method is called on a Client
// that has no base URL.
var ErrNotConfigured = errors.New("fabric: client base URL not configured")

// ErrPlacementNotFound mirrors the console's 404 on
// GET /api/tenants/{id}/placement before any policy has been set.
var ErrPlacementNotFound = errors.New("fabric: placement policy not set")

// GetPlacement reads the tenant's placement policy via
// GET /api/tenants/{id}/placement.
func (c *Client) GetPlacement(ctx context.Context, tenantID string) (*Policy, error) {
	if c.baseURL == "" {
		return nil, ErrNotConfigured
	}
	endpoint, err := url.JoinPath(c.baseURL, "/api/tenants/", tenantID, "/placement")
	if err != nil {
		return nil, fmt.Errorf("build placement URL: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	c.applyAuth(req)
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call console: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrPlacementNotFound
	}
	if resp.StatusCode/100 != 2 {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("console GET placement: status %d: %s", resp.StatusCode, strings.TrimSpace(string(preview)))
	}
	var out Policy
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode placement: %w", err)
	}
	return &out, nil
}

// PutPlacement replaces the tenant's placement policy via
// PUT /api/tenants/{id}/placement.
func (c *Client) PutPlacement(ctx context.Context, tenantID string, p *Policy) error {
	if c.baseURL == "" {
		return ErrNotConfigured
	}
	if err := p.Validate(); err != nil {
		return err
	}
	endpoint, err := url.JoinPath(c.baseURL, "/api/tenants/", tenantID, "/placement")
	if err != nil {
		return fmt.Errorf("build placement URL: %w", err)
	}
	body, err := json.Marshal(p)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.applyAuth(req)
	resp, err := c.httpc.Do(req)
	if err != nil {
		return fmt.Errorf("call console: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("console PUT placement: status %d: %s", resp.StatusCode, strings.TrimSpace(string(preview)))
	}
	return nil
}

func (c *Client) applyAuth(req *http.Request) {
	if c.adminToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.adminToken)
	}
}
