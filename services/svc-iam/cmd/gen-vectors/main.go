// Command gen-vectors emits the CPS1 golden-vector fixture consumed by every
// implementation of the signing contract (Go, Python, and future SDK languages).
//
// The algorithm is the contract (07§4.1, adjudication D4): it ships with the
// SDK and can never take a breaking change. Independent implementations drift
// silently unless they are pinned to shared vectors — 03§9.4 rule ⑤ requires
// the SDK, doc-site examples, and gateway to share one definition, and this
// fixture is how that requirement is enforced across a language boundary.
//
// Regenerate with:
//
//	go run ./cmd/gen-vectors > ../../proto-hub/testdata/cps1-golden-vectors.json
//
// A change to the output of this command is a change to the wire contract and
// must be treated as a breaking API change.
package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/qifalab/euler-platform/cps1"
)

type vector struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Method      string            `json:"method"`
	Host        string            `json:"host"`
	Path        string            `json:"path"`
	Query       map[string]string `json:"query"`
	Headers     map[string]string `json:"headers"`
	Body        string            `json:"body"`
	AK          string            `json:"ak"`
	SK          string            `json:"sk"`
	Region      string            `json:"region"`
	Service     string            `json:"service"`
	Date        string            `json:"date"`

	// Expected outputs — every implementation must reproduce these exactly.
	ExpectedAuthorization string `json:"expected_authorization"`
	ExpectedSignature     string `json:"expected_signature"`
}

func main() {
	at := time.Date(2026, 8, 4, 9, 30, 0, 0, time.UTC)

	cases := []struct {
		name, desc, method, path string
		query                    map[string]string
		body                     string
		extraHeaders             map[string]string
	}{
		{
			name:   "get-no-body",
			desc:   "GET with query parameters and an empty body",
			method: "GET",
			path:   "/",
			query:  map[string]string{"Action": "DescribeInstances", "Version": "2026-08-01"},
			body:   "",
		},
		{
			name:   "post-json-body",
			desc:   "POST with a JSON body; body hash binds the payload to the signature",
			method: "POST",
			path:   "/",
			query:  map[string]string{"Action": "RunInstances", "Version": "2026-08-01"},
			body:   `{"ImageId":"img-001","InstanceType":"s2.large"}`,
		},
		{
			name:   "path-with-encoding",
			desc:   "Path segments requiring percent-encoding; '/' separators preserved",
			method: "GET",
			path:   "/buckets/my bucket/objects",
			query:  map[string]string{"Action": "GetObject", "Version": "2026-08-01"},
			body:   "",
		},
		{
			name: "path-reserved-chars",
			desc: "Path with $ @ : = & + — characters where naive encoders disagree. " +
				"Strict RFC 3986 escapes all of them; Go's url.PathEscape does not. " +
				"This vector pins the interoperable behaviour across SDK languages.",
			method: "GET",
			path:   "/objects/a$b@c:d=e&f+g",
			query:  map[string]string{"Action": "GetObject", "Version": "2026-08-01"},
			body:   "",
		},
		{
			name:   "query-reserved-chars",
			desc:   "Query values containing reserved characters must be strictly encoded",
			method: "GET",
			path:   "/",
			query: map[string]string{
				"Action":  "ListObjects",
				"Version": "2026-08-01",
				"Prefix":  "a+b c/d=e&f",
			},
			body: "",
		},
		{
			name:   "query-ordering",
			desc:   "Query keys must sort lexicographically regardless of input order",
			method: "GET",
			path:   "/",
			query:  map[string]string{"Zebra": "1", "Alpha": "2", "Action": "List", "Version": "2026-08-01"},
			body:   "",
		},
		{
			name:   "sts-security-token",
			desc:   "STS temporary credential; x-cps-security-token participates in the signature",
			method: "POST",
			path:   "/",
			query:  map[string]string{"Action": "RunInstances", "Version": "2026-08-01"},
			body:   `{"x":1}`,
			extraHeaders: map[string]string{
				cps1.SecurityTokenHeader: "sts-token-abcdef0123456789",
			},
		},
	}

	const (
		ak      = "EUAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		sk      = "test-secret-key-0123456789"
		region  = "cn-north-1"
		service = "ecs"
		host    = "euecs.api.euler.emoera.com"
		nonce   = "550e8400-e29b-41d4-a716-446655440000"
	)

	var vectors []vector
	for _, c := range cases {
		hdrs := map[string]string{
			"host":                host,
			cps1.HeaderDate:       at.Format("20060102T150405Z"),
			cps1.HeaderContentSHA: "",
			cps1.NonceHeader:      nonce,
		}
		for k, v := range c.extraHeaders {
			hdrs[k] = v
		}

		q := url.Values{}
		for k, v := range c.query {
			q.Set(k, v)
		}

		signed, err := cps1.Sign(cps1.Request{
			Method:  c.method,
			Host:    host,
			Path:    c.path,
			Query:   q,
			Headers: hdrs,
			Body:    []byte(c.body),
		}, cps1.Credentials{AK: ak, SK: sk}, region, service, at)
		if err != nil {
			fmt.Fprintln(os.Stderr, "sign:", err)
			os.Exit(1)
		}

		vectors = append(vectors, vector{
			Name:                  c.name,
			Description:           c.desc,
			Method:                c.method,
			Host:                  host,
			Path:                  c.path,
			Query:                 c.query,
			Headers:               signed.Headers,
			Body:                  c.body,
			AK:                    ak,
			SK:                    sk,
			Region:                region,
			Service:               service,
			Date:                  at.Format(time.RFC3339),
			ExpectedAuthorization: signed.Authorization,
			ExpectedSignature:     extractSignature(signed.Authorization),
		})
	}

	out := map[string]any{
		"_comment": "CPS1-HMAC-SHA256 golden vectors. Generated by services/svc-iam/cmd/gen-vectors. " +
			"Every implementation of the signing contract (Go pkg-go/cps1, Python SDK, and all " +
			"published SDKs) MUST reproduce expected_signature exactly. A change here is a breaking " +
			"wire-contract change (07-security.md §4.1, adjudication D4).",
		"algorithm": cps1.Algorithm,
		"vectors":   vectors,
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fmt.Fprintln(os.Stderr, "encode:", err)
		os.Exit(1)
	}
}

// extractSignature pulls the Signature= value out of an Authorization header.
func extractSignature(auth string) string {
	const marker = "Signature="
	for i := 0; i+len(marker) <= len(auth); i++ {
		if auth[i:i+len(marker)] == marker {
			return auth[i+len(marker):]
		}
	}
	return ""
}
