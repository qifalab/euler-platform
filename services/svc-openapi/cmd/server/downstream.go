package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/qifalab/euler-platform/httpmw"
)

const (
	// downstreamTimeout bounds one internal call. Creates chain several
	// (catalog → order → pay → fulfil); a gateway timeout in between is safe
	// to retry with the same ClientToken.
	downstreamTimeout = 10 * time.Second
	// maxDownstreamBody bounds a downstream response (resource lists).
	maxDownstreamBody = 8 << 20
)

// downstream is a JSON client for one internal service. Every call carries the
// caller's account id, the internal shared secret (httpmw.SetInternalToken)
// and the trace id, exactly as the gateway would, so the downstream service
// scopes the call to the caller's account and logs it under the same trace.
type downstream struct {
	name string
	base string
	http *http.Client
}

func newDownstream(name, base string) *downstream {
	return &downstream{
		name: name,
		base: strings.TrimRight(base, "/"),
		http: &http.Client{
			Timeout: downstreamTimeout,
			// Internal services never redirect; following one would replay
			// the account header and internal token to another location.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// downstreamError is a failed internal call: either a business verdict (a
// 4xx carrying the platform envelope's Code/Message) or an unavailability
// (transport error, 5xx, or a response that breaks the envelope contract).
type downstreamError struct {
	Service string
	Status  int    // HTTP status; 0 when no response arrived
	Code    string // envelope Code; "" when the body carried none
	Message string
	Err     error // transport / decoding failure
}

func (e *downstreamError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: HTTP %d: %v", e.Service, e.Status, e.Err)
	}
	return fmt.Sprintf("%s: HTTP %d %s: %s", e.Service, e.Status, e.Code, e.Message)
}

func (e *downstreamError) Unwrap() error { return e.Err }

// unavailable reports a failure that is not a business verdict.
func (e *downstreamError) unavailable() bool {
	return e.Err != nil || e.Code == "" || e.Status < 400 || e.Status >= 500
}

// envelope is the platform response body every service emits (03§9.3).
type envelope struct {
	Code    string          `json:"Code"`
	Message string          `json:"Message"`
	Data    json.RawMessage `json:"Data"`
}

// do performs one request on behalf of the call's account and decodes the
// envelope's Data into out (when non-nil). It returns the HTTP status
// (svc-orchestrator's fulfil answers 202 while a resource is provisioning).
func (d *downstream) do(c *call, method, path string, query url.Values, body, out any) (int, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, &downstreamError{Service: d.name, Err: fmt.Errorf("encode request: %w", err)}
		}
		rdr = bytes.NewReader(raw)
	}
	target := d.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(c.ctx, method, target, rdr)
	if err != nil {
		return 0, &downstreamError{Service: d.name, Err: err}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set(accountIDHeader, strconv.FormatInt(c.account, 10))
	if rid := httpmw.RequestIDFromContext(c.ctx); rid != "" {
		// The platform services adopt X-Euler-TraceId; httpmw.RequestID
		// adopts X-Request-Id. Both carry the same id.
		req.Header.Set(traceIDHeader, rid)
		req.Header.Set(requestIDHeader, rid)
	}
	httpmw.SetInternalToken(req)

	resp, err := d.http.Do(req)
	if err != nil {
		return 0, &downstreamError{Service: d.name, Err: err}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxDownstreamBody))
	if err != nil {
		return resp.StatusCode, &downstreamError{Service: d.name, Status: resp.StatusCode, Err: fmt.Errorf("read response: %w", err)}
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || env.Code == "" {
		return resp.StatusCode, &downstreamError{Service: d.name, Status: resp.StatusCode,
			Err: fmt.Errorf("%s %s: response is not a platform envelope: %q", method, path, truncate(string(raw), 200))}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 || env.Code != "OK" {
		return resp.StatusCode, &downstreamError{Service: d.name, Status: resp.StatusCode, Code: env.Code, Message: env.Message}
	}
	if out != nil {
		if len(env.Data) == 0 || string(env.Data) == "null" {
			return resp.StatusCode, &downstreamError{Service: d.name, Status: resp.StatusCode,
				Err: fmt.Errorf("%s %s: response carries no Data", method, path)}
		}
		if err := json.Unmarshal(env.Data, out); err != nil {
			return resp.StatusCode, &downstreamError{Service: d.name, Status: resp.StatusCode,
				Err: fmt.Errorf("%s %s: decode Data: %w", method, path, err)}
		}
	}
	return resp.StatusCode, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
