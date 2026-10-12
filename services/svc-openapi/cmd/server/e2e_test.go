package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/cps1"
	errorsx "github.com/qifalab/euler-platform/errors"
	"github.com/qifalab/euler-platform/eusdk"
	"github.com/qifalab/euler-platform/httpmw"
)

const (
	e2eAK = "EUAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	e2eSK = "test-secret-key-0123456789"
)

// fakeGateway stands in for APISIX's OpenAPI zone route: it verifies the CPS1
// signature with pkg-go/cps1 (eu-auth signature mode, region/service bound to
// the product host), then forwards with the headers eu-auth injects — the
// account id, the internal token and the trace id — and the client's Host.
func fakeGateway(t *testing.T, next http.Handler, account int64) *httptest.Server {
	var n atomic.Int64
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		headers := map[string]string{}
		for k, vs := range r.Header {
			headers[strings.ToLower(k)] = vs[0]
		}
		headers["host"] = r.Host
		product := strings.TrimSuffix(requestHost(r.Host), ".api.euler.emoera.com")
		err = cps1.Verify(cps1.Request{Method: r.Method, Host: r.Host, Path: r.URL.Path, Query: r.URL.Query(),
			Headers: headers, Body: body}, cps1.Credentials{AK: e2eAK, SK: e2eSK},
			"cn-north-1", strings.TrimPrefix(product, "eu"), time.Now().UTC())
		if err != nil {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"Code":"IAM.SignatureDoesNotMatch","Message":"signature does not match"}`))
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.Header.Del("Authorization")
		r.Header.Set(accountIDHeader, strconv.FormatInt(account, 10))
		r.Header.Set(httpmw.InternalTokenHeader, testToken)
		r.Header.Set(traceIDHeader, "gw-trace-"+strconv.FormatInt(n.Add(1), 10))
		next.ServeHTTP(w, r)
	}))
}

// sdkClient signs for {product}.api.euler.emoera.com and dials the gateway.
func sdkClient(gw *httptest.Server, product string) *eusdk.Client {
	addr := gw.Listener.Addr().String()
	return eusdk.New(eusdk.Config{
		AK: e2eAK, SK: e2eSK, Region: "cn-north-1",
		Endpoint: "http://" + product + ".api.euler.emoera.com",
		HTTPClient: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		}},
	})
}

// sdkCall runs one action the way sdk/terraform's crudCall does: a signed
// POST / with a JSON body, Data decoded from the envelope.
func sdkCall(t *testing.T, c *eusdk.Client, product, action string, params map[string]any) (map[string]any, error) {
	t.Helper()
	body, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Call(eusdk.ApiRequest{ProductCode: product, Method: "POST", Path: "/",
		Query: url.Values{"Action": {action}, "Version": {"2026-08-01"}}, Body: body})
	if err != nil {
		return nil, err
	}
	var env struct {
		RequestID string         `json:"RequestId"`
		Code      string         `json:"Code"`
		Data      map[string]any `json:"Data"`
	}
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		t.Fatalf("%s: undecodable body %q: %v", action, resp.Body, err)
	}
	if env.Code != "OK" || !strings.HasPrefix(env.RequestID, "gw-trace-") || resp.Headers.Get(traceIDHeader) != env.RequestID {
		t.Fatalf("%s: envelope %+v, trace header %q", action, env, resp.Headers.Get(traceIDHeader))
	}
	return env.Data, nil
}

func TestSDKEndToEnd(t *testing.T) {
	e := newTestEnv(t)
	gw := fakeGateway(t, e.h, acctA)
	defer gw.Close()
	ecs := sdkClient(gw, "euecs")

	data, err := sdkCall(t, ecs, "euecs", "RunInstances", map[string]any{
		"ImageId": "img-001", "InstanceType": "s2.large", "ZoneId": "cn-north-1-a", "ClientToken": "sdk-e2e-1"})
	if err != nil {
		t.Fatalf("RunInstances: %v", err)
	}
	id, _ := data["InstanceId"].(string)
	if id == "" || data["Status"] != "Running" {
		t.Fatalf("RunInstances Data = %v", data)
	}
	data, err = sdkCall(t, ecs, "euecs", "DescribeInstances", map[string]any{"InstanceId": id})
	if err != nil {
		t.Fatalf("DescribeInstances: %v", err)
	}
	if data["TotalCount"] != float64(1) || single(t, data, "Instances")["InstanceType"] != "s2.large" {
		t.Fatalf("DescribeInstances Data = %v", data)
	}

	// Errors surface through the SDK's unified error model.
	_, err = sdkCall(t, ecs, "euecs", "StopInstance", map[string]any{"InstanceId": "euecs-cn-north-1-ffffffff"})
	var apiErr *errorsx.Error
	if !errors.As(err, &apiErr) || apiErr.Code != "InvalidInstanceId.NotFound" || apiErr.HTTPStatus != http.StatusNotFound {
		t.Fatalf("StopInstance(unknown) error = %#v", err)
	}
	_, err = sdkCall(t, ecs, "euecs", "NoSuchAction", nil)
	if !errors.As(err, &apiErr) || apiErr.Code != "InvalidAction.NotFound" || apiErr.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("NoSuchAction error = %#v", err)
	}

	// Another product host, another signing service (euvpc → vpc).
	vpc := sdkClient(gw, "euvpc")
	data, err = sdkCall(t, vpc, "euvpc", "CreateVpc", map[string]any{"CidrBlock": "10.8.0.0/16"})
	if err != nil || data["VpcId"] == "" {
		t.Fatalf("CreateVpc = %v, %v", data, err)
	}

	// A request signed for one product and sent to another host is refused
	// by the gateway, not answered by the facade.
	_, err = sdkCall(t, sdkClient(gw, "euvpc"), "euecs", "DescribeInstances", nil)
	if !errors.As(err, &apiErr) || apiErr.HTTPStatus != http.StatusForbidden {
		t.Fatalf("cross-product signature error = %#v", err)
	}

	if _, err := sdkCall(t, ecs, "euecs", "TerminateInstances", map[string]any{"InstanceId": id}); err != nil {
		t.Fatalf("TerminateInstances: %v", err)
	}
}
