package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	errorsx "github.com/qifalab/euler-platform/errors"
	"github.com/qifalab/euler-platform/httpmw"
	"github.com/qifalab/euler-platform/identifier"
)

const (
	// accountIDHeader is injected by the gateway after authentication. TRUST
	// NOTE: it is believed only behind httpmw.InternalToken — with
	// EULER_INTERNAL_TOKEN set (mandatory in production), a request that did
	// not pass through the gateway is refused before it reaches an action.
	accountIDHeader = "X-Euler-Account-Id"
	traceIDHeader   = "X-Euler-TraceId"
	requestIDHeader = "X-Request-Id"
)

// actionPattern is the shape of an Action name, the same rule eu-authorize
// applies before prefixing it with the product code.
var actionPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]{0,63}$`)

// versionPattern is the date-type API version (04§3.2).
var versionPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// actionFunc serves one Action. It returns the Data object of the success
// envelope, or an error that call.translate maps onto the API vocabulary.
type actionFunc func(c *call) (any, error)

// call is one Action invocation.
type call struct {
	ctx     context.Context
	product string
	action  string
	account int64
	p       params
	ps      *productSpec
	// specParam names the parameter that selected the SKU of a create or
	// spec change (InstanceType, InstanceClass, SkuCode, ...), so catalogue
	// verdicts are reported against the parameter the caller actually sent.
	specParam string
}

// server is the facade: the downstream clients plus the Action table.
type server struct {
	catalog       *downstream
	order         *downstream
	orchestrator  *downstream
	monitor       *downstream
	defaultRegion string
	// hosts maps each served API host ({productCode}.api.euler.emoera.com)
	// to its product code.
	hosts   map[string]string
	actions map[string]map[string]actionFunc
}

func newServer(cfg config) *server {
	s := &server{
		catalog:       newDownstream("svc-catalog", cfg.CatalogURL),
		order:         newDownstream("svc-order", cfg.OrderURL),
		orchestrator:  newDownstream("svc-orchestrator", cfg.OrchestratorURL),
		monitor:       newDownstream("svc-monitor", cfg.MonitorURL),
		defaultRegion: cfg.DefaultRegion,
	}
	s.actions = s.actionTable()
	s.hosts = make(map[string]string, len(s.actions))
	for product := range s.actions {
		if products[product] == nil {
			panic("svc-openapi: no productSpec for " + product)
		}
		s.hosts[identifier.OpenAPIDomain(product)] = product
	}
	return s
}

// successEnvelope / errorEnvelope are the platform body (03§9.3): the SDKs and
// the Terraform provider read Data on success and Code/Message/RequestId on
// failure.
type successEnvelope struct {
	RequestID string `json:"RequestId"`
	Code      string `json:"Code"`
	Data      any    `json:"Data"`
}

type errorEnvelope struct {
	RequestID string `json:"RequestId"`
	Code      string `json:"Code"`
	Message   string `json:"Message"`
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rid := httpmw.RequestIDFromContext(r.Context())
	c, data, apiErr := s.dispatch(r)
	if apiErr != nil {
		if c != nil {
			slog.Info("openapi action refused", "trace_id", rid, "product", c.product, "action", c.action,
				"account", c.account, "status", apiErr.HTTPStatus, "code", apiErr.Code)
		}
		writeJSON(w, apiErr.HTTPStatus, errorEnvelope{RequestID: rid, Code: apiErr.Code, Message: apiErr.Message})
		return
	}
	if data == nil {
		data = struct{}{}
	}
	slog.Info("openapi action", "trace_id", rid, "product", c.product, "action", c.action, "account", c.account)
	writeJSON(w, http.StatusOK, successEnvelope{RequestID: rid, Code: "OK", Data: data})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// dispatch resolves product, caller and Action, parses the parameters and runs
// the action. The call is returned (when one was built) for logging.
func (s *server) dispatch(r *http.Request) (*call, any, *errorsx.Error) {
	host := requestHost(r.Host)
	product, ok := s.hosts[host]
	if !ok {
		return nil, nil, errorsx.New("InvalidProduct.NotFound", http.StatusBadRequest,
			fmt.Sprintf("No OpenAPI product is served at host %q.", host))
	}
	if r.URL.Path != "/" {
		return nil, nil, errorsx.New("InvalidPath.NotFound", http.StatusNotFound,
			"OpenAPI actions are served at the path / (RPC style: ?Action=...&Version=...).")
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		return nil, nil, errorsx.New("UnsupportedHTTPMethod", http.StatusMethodNotAllowed,
			"OpenAPI actions accept GET and POST only.")
	}
	account, apiErr := accountFrom(r)
	if apiErr != nil {
		return nil, nil, apiErr
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, nil, errorsx.New(codeInvalidParameter, http.StatusBadRequest, "The query string is malformed.")
	}
	action, apiErr := actionFrom(query)
	if apiErr != nil {
		return nil, nil, apiErr
	}
	fn, ok := s.actions[product][action]
	if !ok {
		return nil, nil, errorsx.New(codeInvalidAction, http.StatusBadRequest,
			fmt.Sprintf("The specified action %q is not supported by %s.", action, product))
	}
	p, apiErr := parseParams(r, query)
	if apiErr != nil {
		return nil, nil, apiErr
	}
	c := &call{ctx: r.Context(), product: product, action: action, account: account, p: p, ps: products[product]}
	data, err := fn(c)
	if err != nil {
		return c, nil, c.translate(err)
	}
	return c, data, nil
}

// requestHost is the Host header without port, lower-cased and without a
// trailing dot. APISIX passes the client's Host through (pass_host: pass).
func requestHost(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
}

// accountFrom reads the gateway-injected account id (a positive integer).
func accountFrom(r *http.Request) (int64, *errorsx.Error) {
	raw := strings.TrimSpace(r.Header.Get(accountIDHeader))
	if raw == "" {
		return 0, errorsx.New("Common.MissingAccountId", http.StatusForbidden,
			"X-Euler-Account-Id header is required (injected by the gateway).")
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, errorsx.New("Common.MissingAccountId", http.StatusForbidden, "malformed X-Euler-Account-Id")
	}
	return id, nil
}

// actionFrom reads Action (and checks Version) from the query string. The
// Action MUST come from the query: that is the value eu-authorize authorized,
// so an Action in the body is never consulted.
func actionFrom(q url.Values) (string, *errorsx.Error) {
	actions := q["Action"]
	switch {
	case len(actions) == 0 || strings.TrimSpace(actions[0]) == "":
		return "", missingParam("Action")
	case len(actions) > 1:
		return "", invalidParam("Action", "must be given exactly once")
	case !actionPattern.MatchString(actions[0]):
		return "", invalidParam("Action", "must be an action name such as DescribeInstances")
	}
	if vs := q["Version"]; len(vs) > 1 || (len(vs) == 1 && !versionPattern.MatchString(vs[0])) {
		return "", invalidParam("Version", "must be a date version such as 2026-08-01")
	}
	return actions[0], nil
}
