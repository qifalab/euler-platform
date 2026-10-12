// Package main: OpenAPI Explorer debug-console backend (03§9.4 rule ⑤).
//
// The Explorer lets an authenticated console user assemble a product API call in
// the browser and see what the platform would actually send — and, when the
// target is reachable, what it would get back. The signature is computed with
// pkg-go/cps1, the SAME implementation the SDK ships and the gateway verifies
// (07§4.1, adjudication D4). There is no second signer here: the Explorer,
// SDK, and gateway share one definition, and a change to any of them is a
// breaking wire-contract change.
//
// POST /api/v1/apimeta/explorer
//
//	body: { "product_code", "action_name", "method", "path", "query": {...},
//	        "headers": {...}, "body": "...", "ak", "sk", "region" }
//
// The handler:
//  1. resolves the Action's service namespace from the product code (routing
//     subdomain {product}.api.euler.emoera.com → service, 04-middleware §3.2);
//  2. builds a cps1.Request and signs it;
//  3. if a target base URL is configured (EULER_EXPLORER_TARGET or a per-product
//     override), executes the signed request over the wire and returns the
//     upstream response alongside the signature material;
//  4. otherwise returns only the signature material (the "what would be sent"
//     view). This keeps the Explorer useful in source-only dev where no
//     upstream product API is running, while never faking a signature.
//
// Credentials (ak/sk) are supplied by the caller for the debug session. In a
// real deployment the Explorer signs with the console user's own AK/SK fetched
// from svc-iam (never the platform's); the ak/sk-in-body form here is the
// phase-2 dev surface and matches the golden-vector fixture inputs.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/qifalab/euler-platform/cps1"
	"github.com/qifalab/euler-platform/errors"
	"github.com/qifalab/euler-platform/identifier"
)

// productAPIHost is the routing subdomain convention (04-middleware §3.2):
// {product}.api.euler.emoera.com. The service namespace is the product code with
// the leading "eu" stripped (euecs → ecs), matching the gateway verifier's
// derivation and the golden-vector fixture (service "ecs").
func productAPIHost(productCode string) string {
	return productCode + ".api.euler.emoera.com"
}

// serviceNamespace derives the signing service from a product code: euecs→ecs,
// euoss→oss, euvpc→vpc. Unknown products keep the full code as the service so a
// new product is signable before its mapping is taught here (the gateway would
// reject a service it does not recognise, which is the correct gate).
func serviceNamespace(productCode string) string {
	if strings.HasPrefix(productCode, "eu") && len(productCode) > 2 {
		return productCode[2:]
	}
	return productCode
}

// explorerRequest is the POST /api/v1/apimeta/explorer body.
type explorerRequest struct {
	ProductCode   string            `json:"product_code"`
	ActionName    string            `json:"action_name"`
	Method        string            `json:"method"`
	Path          string            `json:"path"`
	Query         map[string]string `json:"query"`
	Headers       map[string]string `json:"headers"`
	Body          string            `json:"body"`
	AK            string            `json:"ak"`
	SK            string            `json:"sk"`
	SecurityToken string            `json:"security_token"`
	Region        string            `json:"region"`
}

// signatureMaterial is the signed output the Explorer renders so a user can see
// exactly what would be sent on the wire. It mirrors cps1.SignedRequest plus the
// canonical pieces a debugger needs.
type signatureMaterial struct {
	Method        string            `json:"method"`
	Host          string            `json:"host"`
	Path          string            `json:"path"`
	Query         map[string]string `json:"query"`
	Body          string            `json:"body"`
	Region        string            `json:"region"`
	Service       string            `json:"service"`
	Date          string            `json:"date"` // x-cps-date value
	Authorization string            `json:"authorization"`
	SignedHeaders string            `json:"signed_headers"`
	Signature     string            `json:"signature"`
	Headers       map[string]string `json:"headers"` // all x-cps-* + host to attach
}

// upstreamResult is the optional proxied response when a target is configured.
type upstreamResult struct {
	StatusCode int               `json:"status_code"`
	Headers    map[string]string `json:"headers"`
	Body       string            `json:"body"`
	Error      string            `json:"error,omitempty"`
}

// explorerResponse is the full Explorer payload.
type explorerResponse struct {
	Signature signatureMaterial `json:"signature"`
	Executed  bool              `json:"executed"` // whether a live call was made
	Upstream  *upstreamResult   `json:"upstream,omitempty"`
}

// handleExplorer signs (and optionally proxies) a product API call.
//
// It is wired onto the mux in main.go. Like the other public-meta endpoints it
// is account-gated (X-Euler-Account-Id required) so the Explorer is reachable
// only from the authenticated console, not the open internet.
func (s *actionStore) handleExplorer(w http.ResponseWriter, r *http.Request) {
	if accountIDFromRequest(r) == "" {
		writeError(w, errorsx.New("Common.MissingAccountId", errorsx.StatusForbidden,
			"X-Euler-Account-Id header is required (injected by gateway)"))
		return
	}

	var req explorerRequest
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB body cap
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, errorsx.ErrInvalidParameter)
		return
	}
	if req.ProductCode == "" {
		writeError(w, errorsx.New("Common.InvalidParameter", errorsx.StatusBadRequest,
			"product_code is required"))
		return
	}
	if !identifier.IsValidProductCode(req.ProductCode) {
		writeError(w, errorsx.New("Common.InvalidParameter", errorsx.StatusBadRequest,
			fmt.Sprintf("invalid product_code %q", req.ProductCode)))
		return
	}
	if req.AK == "" || req.SK == "" {
		writeError(w, errorsx.New("Common.InvalidParameter", errorsx.StatusBadRequest,
			"ak and sk are required for the debug session"))
		return
	}

	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = "GET"
	}
	if !allowedExplorerMethods[method] {
		writeError(w, errorsx.New("Common.InvalidParameter", errorsx.StatusBadRequest, "unsupported method"))
		return
	}
	if req.Path == "" {
		req.Path = "/"
	}
	// The path is appended to the configured upstream. It must be a plain
	// absolute path: "@evil:8080/x" used to turn the upstream base into a
	// userinfo prefix and send the signed request (and the response back to
	// the caller) to any host — SSRF into the cluster.
	if !validExplorerPath(req.Path) {
		writeError(w, errorsx.New("Common.InvalidParameter", errorsx.StatusBadRequest, "path must be an absolute path without authority, query or fragment"))
		return
	}
	// req.Path is the escaped wire form ("/a%20b"). The gateway unescapes the
	// path before verifying (svc-iam verify: url.PathUnescape), so the signature
	// must cover the decoded path — signing the escaped text would make the
	// canonical URI encode the '%' a second time.
	signPath, err := url.PathUnescape(req.Path)
	if err != nil {
		writeError(w, errorsx.New("Common.InvalidParameter", errorsx.StatusBadRequest, "path contains an invalid percent-escape"))
		return
	}
	if req.Region == "" {
		req.Region = "cn-north-1"
	}
	host := productAPIHost(req.ProductCode)
	service := serviceNamespace(req.ProductCode)
	at := time.Now().UTC()

	// Build the cps1.Request. The caller's headers are merged; host is forced to
	// the routing subdomain so a user cannot sign against a different host than
	// the gateway routes (the host IS part of the canonical string — getting it
	// wrong is the most common signing failure, so we own it here).
	// Only protocol headers (x-cps-*) and content negotiation are taken from
	// the caller. Anything else — X-Euler-Account-Id, X-Euler-Internal-Token,
	// Host, hop-by-hop headers — would otherwise ride the live call into the
	// upstream with the platform's network position.
	hdrs := make(map[string]string, len(req.Headers)+3)
	for k, v := range req.Headers {
		lk := strings.ToLower(strings.TrimSpace(k))
		if strings.HasPrefix(lk, cps1.HeaderPrefix) || lk == "content-type" || lk == "accept" {
			hdrs[lk] = v
		}
	}
	hdrs["host"] = host
	hdrs[cps1.HeaderDate] = at.Format("20060102T150405Z")
	hdrs[cps1.NonceHeader] = explorerNonce(r) // deterministic per request, not random
	if req.SecurityToken != "" {
		hdrs[cps1.SecurityTokenHeader] = req.SecurityToken
	}

	query := url.Values{}
	for k, v := range req.Query {
		query.Set(k, v)
	}

	signed, err := cps1.Sign(cps1.Request{
		Method:  method,
		Host:    host,
		Path:    signPath,
		Query:   query,
		Headers: hdrs,
		Body:    []byte(req.Body),
	}, cps1.Credentials{AK: req.AK, SK: req.SK, SecurityToken: req.SecurityToken},
		req.Region, service, at)
	if err != nil {
		writeError(w, errorsx.New("ApiMeta.SignFailed", errorsx.StatusBadRequest,
			"signing failed: "+err.Error()))
		return
	}

	out := explorerResponse{
		Signature: signatureMaterial{
			Method:        method,
			Host:          host,
			Path:          req.Path,
			Query:         req.Query,
			Body:          req.Body,
			Region:        req.Region,
			Service:       service,
			Date:          signed.Headers[cps1.HeaderDate],
			Authorization: signed.Authorization,
			SignedHeaders: signedHeaderList(signed.Headers),
			Signature:     extractSig(signed.Authorization),
			Headers:       signed.Headers,
		},
	}

	// Optional live execution. EULER_EXPLORER_TARGET points at a reachable product
	// API base (e.g. http://localhost:9102 for a locally-run product); without it
	// the Explorer returns only the signature material (source-only dev).
	target := explorerTarget(req.ProductCode)
	if target != "" {
		out.Executed = true
		out.Upstream = executeSigned(r.Context(), target, method, req.Path, query, signed, []byte(req.Body))
	}

	writeJSON(w, http.StatusOK, out)
}

// explorerNonce returns a fresh random nonce. It used to be derived from the
// (client-influenced) request id; a nonce is an anti-replay value and must not
// be predictable or caller-chosen.
func explorerNonce(_ *http.Request) string {
	return "explorer-" + uuid.NewString()
}

// allowedExplorerMethods are the verbs an OpenAPI Action is called with.
var allowedExplorerMethods = map[string]bool{"GET": true, "POST": true, "PUT": true, "DELETE": true, "HEAD": true}

// validExplorerPath accepts an absolute path made of RFC 3986 path characters
// only — no scheme, authority ("//", "@"), backslash, query or fragment. A '%'
// must start a well-formed escape ("%XX"): the path is the escaped wire form,
// sent as typed and unescaped once by the gateway.
func validExplorerPath(p string) bool {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || len(p) > 1024 {
		return false
	}
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '%':
			if i+2 >= len(p) || !isHex(p[i+1]) || !isHex(p[i+2]) {
				return false
			}
			i += 2
		case strings.IndexByte("/-._~!$&'()*+,;=:", c) >= 0:
		default:
			return false
		}
	}
	return true
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// explorerTarget resolves a per-product upstream base URL. Env override
// EULER_EXPLORER_TARGET_<PRODUCT> (uppercased, product chars kept) takes priority,
// then a blanket EULER_EXPLORER_TARGET. Empty = no live call.
func explorerTarget(productCode string) string {
	key := "EULER_EXPLORER_TARGET_" + strings.ToUpper(productCode)
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv("EULER_EXPLORER_TARGET"))
}

// explorerHTTPClient is the upstream client for live Explorer calls: a bounded
// timeout so a hung upstream cannot pin an Explorer request indefinitely
// (never http.DefaultClient, which has no timeout).
// Redirects are not followed: a 3xx from the upstream must not become a hop to
// an arbitrary host carrying the signed headers.
var explorerHTTPClient = &http.Client{
	Timeout: 10 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// maxExplorerResponse caps how much of the upstream body is echoed back.
const maxExplorerResponse = 1 << 20

// executeSigned performs the signed HTTP request against a configured target and
// captures the response for display. A transport error becomes a structured
// upstreamResult.Error rather than a 503 — the signature already succeeded, and
// an unreachable upstream is exactly the kind of thing the Explorer exists to
// surface without masking. The caller's request context bounds the call so a
// disconnected client cancels the upstream fetch.
//
// The request carries exactly what was signed: the x-cps-* headers, the
// Authorization header (cps1 returns it separately from Headers), and the
// signed routing host as the Host header — the gateway routes and verifies by
// Host, so dialing the target with the target's own host name would fail
// verification even with a correct signature.
func executeSigned(ctx context.Context, target, method, wirePath string, query url.Values, signed cps1.SignedRequest, body []byte) *upstreamResult {
	// Build the URL structurally from the configured base so the caller's
	// path can only ever extend the base path — never change scheme or host.
	base, err := url.Parse(strings.TrimSpace(target))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return &upstreamResult{Error: "explorer target is misconfigured"}
	}
	decoded, err := url.PathUnescape(wirePath)
	if err != nil {
		return &upstreamResult{Error: "path contains an invalid percent-escape"}
	}
	u := *base
	u.User = nil
	// Path is the decoded form; RawPath keeps the caller's escaping so "%20"
	// goes out once instead of being re-escaped to "%2520" (an empty RawPath
	// makes url.URL escape the '%' itself).
	u.Path = strings.TrimRight(base.Path, "/") + decoded
	u.RawPath = strings.TrimRight(base.EscapedPath(), "/") + wirePath
	u.RawQuery = query.Encode()
	u.Fragment = ""
	if u.Host != base.Host {
		return &upstreamResult{Error: "refusing to call a host other than the configured target"}
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), strings.NewReader(string(body)))
	if err != nil {
		return &upstreamResult{Error: "build request: " + err.Error()}
	}
	for k, v := range signed.Headers {
		if strings.EqualFold(k, "host") {
			// The signed host is the routing subdomain. It travels as the Host
			// header only; the dial target stays u.Host, fixed above.
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	req.Header.Set("Authorization", signed.Authorization)
	resp, err := explorerHTTPClient.Do(req)
	if err != nil {
		return &upstreamResult{Error: "upstream unreachable: " + err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxExplorerResponse))
	rh := make(map[string]string)
	for k := range resp.Header {
		rh[k] = resp.Header.Get(k)
	}
	return &upstreamResult{
		StatusCode: resp.StatusCode,
		Headers:    rh,
		Body:       string(raw),
	}
}

// extractSig pulls the trailing Signature= value out of an Authorization header.
func extractSig(auth string) string {
	const marker = "Signature="
	if i := strings.LastIndex(auth, marker); i >= 0 {
		return auth[i+len(marker):]
	}
	return ""
}

// signedHeaderList returns the SignedHeaders clause the gateway expects to see.
func signedHeaderList(headers map[string]string) string {
	list, err := cps1.SignedHeaderList(headers)
	if err != nil {
		return ""
	}
	return list
}
