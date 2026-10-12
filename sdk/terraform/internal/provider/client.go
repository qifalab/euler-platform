package provider

// The provider's view of the OpenAPI: one signed RPC call (Action + Version in
// the query, a JSON parameter object in the body) answered with the facade's
// envelope — success {RequestId, Code:"OK", Data}, failure {RequestId, Code,
// Message} with a 4xx/5xx status (services/svc-openapi api.go). Signing is
// pkg-go/eusdk → pkg-go/cps1, the one implementation every consumer shares.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	errorsx "github.com/qifalab/euler-platform/errors"
	"github.com/qifalab/euler-platform/eusdk"
)

// apiVersion is the API version the provider speaks.
const apiVersion = "2026-08-01"

// Retry policy for retryable failures (transport errors, 429, 5xx — the
// facade answers a downstream outage or timeout with 503 ServiceUnavailable).
// Every call the provider makes is safe to repeat: reads and renames are
// idempotent, a spec change to the current spec is a no-op, a repeated delete
// answers NotFound (treated as done) and a create carries a ClientToken, so a
// retry lands on the resource the first attempt made. Variables so the tests
// can shorten them.
var (
	maxAttempts  = 4
	retryBackoff = 2 * time.Second
)

// httpTimeout bounds one HTTP round trip. It is longer than the gateway's
// 30s upstream read timeout on the facade routes, so a slow create surfaces
// as the gateway's 504 (retried with the same ClientToken) rather than a
// client-side abort racing the gateway.
const httpTimeout = 60 * time.Second

// apiClient is what the provider hands its resources: the signed client and
// the default region for resources that do not name one.
type apiClient struct {
	sdk    *eusdk.Client
	region string
}

func newAPIClient(cfg eusdk.Config) *apiClient {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: httpTimeout}
	}
	return &apiClient{sdk: eusdk.New(cfg), region: cfg.Region}
}

// errNotConfigured is a CRUD call on a resource whose provider never handed
// over a client.
var errNotConfigured = errors.New("the euler provider is not configured (no API client)")

// apiError is an error answer of the API.
type apiError struct {
	Action    string
	Status    int
	Code      string
	Message   string
	RequestID string
}

func (e *apiError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s", e.Code, e.Message)
	if e.RequestID != "" {
		fmt.Fprintf(&b, " (RequestId %s", e.RequestID)
		if e.Status != 0 {
			fmt.Fprintf(&b, ", HTTP %d", e.Status)
		}
		b.WriteString(")")
	} else if e.Status != 0 {
		fmt.Fprintf(&b, " (HTTP %d)", e.Status)
	}
	return b.String()
}

// transportError is a failure to get any answer (connection, timeout).
type transportError struct{ err error }

func (e *transportError) Error() string { return e.err.Error() }
func (e *transportError) Unwrap() error { return e.err }

// errorCode is the API code of err ("" when it is not an API answer).
func errorCode(err error) string {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.Code
	}
	return ""
}

// retryable reports whether repeating the call may succeed.
func retryable(err error) bool {
	var te *transportError
	if errors.As(err, &te) {
		return true
	}
	var ae *apiError
	if !errors.As(err, &ae) {
		return false
	}
	switch ae.Code {
	case "ServiceUnavailable", "InternalError", "Throttling", "Common.Throttling":
		return true
	}
	return ae.Status == http.StatusTooManyRequests || ae.Status >= 500
}

// call signs and sends one Action, retrying retryable failures, and returns
// the envelope's Data object.
func (c *apiClient) call(ctx context.Context, product, action string, params map[string]any) (map[string]any, error) {
	if c == nil || c.sdk == nil {
		return nil, errNotConfigured
	}
	body, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	for attempt := 1; ; attempt++ {
		data, err := c.callOnce(product, action, body)
		if err == nil || !retryable(err) || attempt >= maxAttempts {
			return data, err
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%w (giving up: %v)", err, ctx.Err())
		case <-time.After(retryBackoff << (attempt - 1)):
		}
	}
}

func (c *apiClient) callOnce(product, action string, body []byte) (map[string]any, error) {
	resp, err := c.sdk.Call(eusdk.ApiRequest{
		ProductCode: product,
		Method:      http.MethodPost,
		Path:        "/",
		Query:       url.Values{"Action": {action}, "Version": {apiVersion}},
		Body:        body,
	})
	var xe *errorsx.Error
	switch {
	case errors.As(err, &xe):
		ae := &apiError{Action: action, Status: xe.HTTPStatus, Code: xe.Code, Message: xe.Message}
		if resp != nil {
			var env struct {
				RequestID string `json:"RequestId"`
			}
			if json.Unmarshal(resp.Body, &env) == nil {
				ae.RequestID = env.RequestID
			}
		}
		return nil, ae
	case err != nil:
		return nil, &transportError{err: fmt.Errorf("%s: %w", action, err)}
	}
	return decodeEnvelope(action, resp.StatusCode, resp.Body)
}

// decodeEnvelope reads a 2xx answer. Numbers stay json.Number so integer
// attributes (Size, Bandwidth) convert exactly.
func decodeEnvelope(action string, status int, raw []byte) (map[string]any, error) {
	var env struct {
		RequestID string          `json:"RequestId"`
		Code      string          `json:"Code"`
		Message   string          `json:"Message"`
		Data      json.RawMessage `json:"Data"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&env); err != nil {
		return nil, fmt.Errorf("%s: the response is not the API envelope: %w", action, err)
	}
	if env.Code != "" && env.Code != "OK" {
		return nil, &apiError{Action: action, Status: status, Code: env.Code, Message: env.Message, RequestID: env.RequestID}
	}
	data := map[string]any{}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return data, nil
	}
	dd := json.NewDecoder(bytes.NewReader(env.Data))
	dd.UseNumber()
	if err := dd.Decode(&data); err != nil {
		return nil, fmt.Errorf("%s: the response Data is not an object (RequestId %s): %w", action, env.RequestID, err)
	}
	return data, nil
}

// newClientToken returns a fresh idempotency token for one create.
func newClientToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("euler provider: no randomness for a ClientToken: " + err.Error())
	}
	return "tf-" + hex.EncodeToString(b)
}
