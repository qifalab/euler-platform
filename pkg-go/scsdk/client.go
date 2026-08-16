// Package scsdk is the StarCloud Go client SDK (03§9.4 SDK generation).
//
// It is the single Go entry point for calling product APIs from off-platform
// (user code, automation) the way the published cloudsdk-{product}-go packages
// wrap. It does NOT reimplement signing: it calls pkg-go/cps1, the SAME
// implementation the gateway verifies and the OpenAPI Explorer signs with
// (07§4.1, adjudication D4, 03§9.4 rule ⑤ — one contract, three consumers).
// Errors are parsed into pkg-go/errors' unified model (03§9.3) so a Go caller
// sees the same Code/HTTPStatus pairing the wire carries.
//
// Usage:
//
//	client := scsdk.New(scsdk.Config{
//	    AK: "SC...", SK: "...", Region: "cn-north-1", Service: "ecs",
//	})
//	resp, err := client.Call(scsdk.ApiRequest{
//	    ProductCode: "scecs", Method: "POST", Path: "/",
//	    Query: url.Values{"Action": {"RunInstances"}, "Version": {"2026-08-01"}},
//	    Body: []byte(`{"ImageId":"img-001","InstanceType":"s2.large"}`),
//	})
//
// The Service field is the signing-service namespace (scecs→ecs). Callers may
// leave it zero and the SDK derives it from ProductCode, matching the gateway
// verifier's derivation (04-middleware §3.2).
package scsdk

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/starcloud/sc-platform/cps1"
	errorsx "github.com/starcloud/sc-platform/errors"
)

// Config is the SDK credential + transport configuration.
type Config struct {
	// AK/SK are the access key pair (07§2.2). SK is held in memory for the
	// duration of the client; at rest it is a KMS envelope ciphertext the
	// caller decrypts before constructing the SDK.
	AK string
	SK string

	// SecurityToken is non-empty only for STS temporary credentials.
	SecurityToken string

	// Region is the signing region (e.g. cn-north-1). Required.
	Region string

	// Service is the signing-service namespace. Optional; derived from
	// ProductCode on each call when zero (scecs→ecs).
	Service string

	// Endpoint overrides the default routing subdomain
	// ({product}.api.starcloud.cn). Used for on-prem / test deployments.
	Endpoint string

	// HTTPClient overrides the default transport. Defaults to a client with a
	// 30s timeout (anti-hang; the gateway has its own timeouts too).
	HTTPClient *http.Client
}

// Client is the SDK handle. Construct once, share across goroutines (the only
// mutable state is an idempotent HTTP client, which is safe for concurrent use).
type Client struct {
	cfg        Config
	httpClient *http.Client
}

// New constructs a Client. AK, SK, and Region are required; a missing one is a
// programmer error surfaced at the first call as a cps1 error.
func New(cfg Config) *Client {
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{cfg: cfg, httpClient: hc}
}

// ApiRequest is one product API call the SDK signs and sends.
type ApiRequest struct {
	// ProductCode is the sc-prefixed product (scecs, scoss, ...). The signing
	// host is derived from it ({product}.api.starcloud.cn) unless Endpoint is
	// set on the Config.
	ProductCode string

	Method string // GET/POST/PUT/DELETE; default GET
	Path   string // raw path; default "/"
	Query  url.Values
	Body   []byte

	// ExtraHeaders are additional request headers that are NOT part of the CPS1
	// signature (only host + x-cps-* are signed; 07§4.1). Use for content-type,
	// correlation ids, etc.
	ExtraHeaders map[string]string
}

// ApiResponse is the decoded result. StatusCode is the HTTP status; Body is the
// raw response body (the caller unmarshals). Error is non-nil only on transport
// failure or a non-2xx that carried a recognizable platform error body.
type ApiResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// Call signs the request with cps1 and sends it. The signature is computed with
// the SDK's configured credentials; region/service come from the Config (service
// derived from ProductCode when the Config leaves it zero).
func (c *Client) Call(req ApiRequest) (*ApiResponse, error) {
	if req.ProductCode == "" {
		return nil, fmt.Errorf("scsdk: ProductCode is required")
	}
	creds := cps1.Credentials{AK: c.cfg.AK, SK: c.cfg.SK, SecurityToken: c.cfg.SecurityToken}

	host := c.hostFor(req.ProductCode)
	service := c.cfg.Service
	if service == "" {
		service = serviceNamespace(req.ProductCode)
	}
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = "GET"
	}
	if req.Path == "" {
		req.Path = "/"
	}
	if req.Query == nil {
		req.Query = url.Values{}
	}

	// Build the header set cps1 signs. host is forced; date/content-sha256 are
	// filled by cps1.Sign; a nonce is generated per call (anti-replay, 07§4.2).
	hdrs := map[string]string{"host": host}
	for k, v := range req.ExtraHeaders {
		hdrs[k] = v
	}
	hdrs[cps1.NonceHeader] = newNonce()

	now := time.Now().UTC()
	signed, err := cps1.Sign(cps1.Request{
		Method:  method,
		Host:    host,
		Path:    req.Path,
		Query:   req.Query,
		Headers: hdrs,
		Body:    req.Body,
	}, creds, c.cfg.Region, service, now)
	if err != nil {
		return nil, fmt.Errorf("scsdk: sign: %w", err)
	}

	u := c.urlFor(host, req)
	httpReq, err := http.NewRequest(method, u, bytes.NewReader(req.Body))
	if err != nil {
		return nil, fmt.Errorf("scsdk: build request: %w", err)
	}
	httpReq.Header = http.Header{}
	for k, v := range signed.Headers {
		httpReq.Header.Set(k, v)
	}
	httpReq.Header.Set("Authorization", signed.Authorization)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("scsdk: %w", err)
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(resp.Body)

	out := &ApiResponse{StatusCode: resp.StatusCode, Headers: resp.Header, Body: raw}
	if readErr != nil {
		return out, fmt.Errorf("scsdk: read body: %w", readErr)
	}
	// Map a non-2xx into the unified error model (03§9.3). A body shaped like
	// the platform envelope {Code, Message} becomes an *errorsx.Error carrying
	// the business code; an unstructured body becomes Common.InternalError.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return out, parseError(raw, resp.StatusCode)
	}
	return out, nil
}

// hostFor resolves the signing host: the configured Endpoint if set, else the
// routing subdomain {product}.api.starcloud.cn (04-middleware §3.2).
func (c *Client) hostFor(productCode string) string {
	if c.cfg.Endpoint != "" {
		// Strip the scheme so the host header is bare, matching what cps1 signs.
		h := c.cfg.Endpoint
		h = strings.TrimPrefix(h, "https://")
		h = strings.TrimPrefix(h, "http://")
		if i := strings.Index(h, "/"); i >= 0 {
			h = h[:i]
		}
		return h
	}
	return productAPIHost(productCode)
}

// urlFor builds the full request URL, prefixing http(s):// onto the host.
func (c *Client) urlFor(host string, req ApiRequest) string {
	scheme := "https"
	if c.cfg.Endpoint != "" && strings.HasPrefix(c.cfg.Endpoint, "http://") {
		scheme = "http"
	}
	u := scheme + "://" + host + req.Path
	if len(req.Query) > 0 {
		u += "?" + req.Query.Encode()
	}
	return u
}

// newNonce returns a fresh anti-replay nonce per call (07§4.2 layer 2). cps1.Sign
// requires one be present; the SDK owns nonce generation so cps1.Sign stays
// deterministic (a property the golden-vector regression relies on).
func newNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return uuid.NewString() + "-" + hex.EncodeToString(b)
}

// productAPIHost is the routing subdomain convention (shared with the Explorer).
func productAPIHost(productCode string) string {
	return productCode + ".api.starcloud.cn"
}

// serviceNamespace derives the signing service: scecs→ecs, scoss→oss, ...
// (shared with the Explorer and the gateway verifier's derivation).
func serviceNamespace(productCode string) string {
	if strings.HasPrefix(productCode, "sc") && len(productCode) > 2 {
		return productCode[2:]
	}
	return productCode
}

// parseError decodes a non-2xx response body into the unified error model
// (03§9.3). A platform envelope {Code, Message} yields an *errorsx.Error with
// the business code and the real HTTP status; anything else becomes
// Common.InternalError (never a silent swallow).
func parseError(body []byte, status int) *errorsx.Error {
	// Cheap shape check without pulling encoding/json into a generic path: the
	// platform body has a top-level "Code" field.
	if len(body) > 0 && bytes.Contains(body, []byte(`"Code"`)) {
		var env struct {
			Code    string `json:"Code"`
			Message string `json:"Message"`
		}
		if err := json.Unmarshal(body, &env); err == nil && env.Code != "" {
			return errorsx.New(env.Code, status, env.Message)
		}
	}
	return errorsx.New("Common.InternalError", status,
		fmt.Sprintf("HTTP %d: %s", status, truncate(string(body), 200)))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
