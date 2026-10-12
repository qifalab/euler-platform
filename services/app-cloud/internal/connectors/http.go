package connectors

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Manager struct {
	config  Config
	allowed map[string]bool
	client  *http.Client
}

func New(config Config) (*Manager, error) {
	if config.Timeout == 0 {
		config.Timeout = 8 * time.Second
	}
	if config.Timeout < time.Millisecond || config.Timeout > 30*time.Second {
		return nil, failure("invalid_config", "连接超时须介于 1 毫秒和 30 秒。", 400)
	}
	m := &Manager{config: config, allowed: map[string]bool{}}
	for _, raw := range config.AllowedOrigins {
		origin, err := normalizeOrigin(raw, config.AllowInsecureHTTP)
		if err != nil {
			return nil, err
		}
		m.allowed[origin] = true
	}
	for _, raw := range []string{config.EIDBaseURL, config.TrustBaseURL} {
		if raw != "" {
			if _, err := m.origin(raw); err != nil {
				return nil, err
			}
		}
	}
	for _, raw := range config.ConsoleURLs {
		if _, err := m.consoleAddress(raw); err != nil {
			return nil, err
		}
	}
	transport := &http.Transport{Proxy: nil, DialContext: m.dialContext, ForceAttemptHTTP2: true, TLSHandshakeTimeout: config.Timeout, ResponseHeaderTimeout: config.Timeout, IdleConnTimeout: 30 * time.Second, MaxIdleConns: 10, MaxIdleConnsPerHost: 2}
	m.client = &http.Client{Transport: transport, Timeout: config.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return m, nil
}

func normalizeOrigin(raw string, allowHTTP bool) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.Opaque != "" {
		return "", failure("invalid_origin", "服务地址必须是完整来源地址，不含账号、路径、查询参数或片段。", 400)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "https" && !(allowHTTP && u.Scheme == "http") {
		return "", failure("invalid_origin", "服务地址必须使用 HTTPS。", 400)
	}
	host := strings.ToLower(u.Hostname())
	if strings.HasSuffix(host, ".") || strings.ContainsAny(host, "%\\ \t\r\n") {
		return "", failure("invalid_origin", "服务主机名无效。", 400)
	}
	port := u.Port()
	if port != "" {
		n, e := strconv.Atoi(port)
		if e != nil || n < 1 || n > 65535 {
			return "", failure("invalid_origin", "服务端口无效。", 400)
		}
	}
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		u.Host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	} else {
		u.Host = host
	}
	u.Path = ""
	u.RawPath = ""
	return u.String(), nil
}

func (m *Manager) origin(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", failure("unconfigured", "尚未配置产品服务地址。", 400)
	}
	origin, err := normalizeOrigin(raw, m.config.AllowInsecureHTTP)
	if err != nil {
		return "", err
	}
	if !m.allowed[origin] {
		return "", failure("origin_not_allowed", "此服务来源尚未由部署管理员允许。", 400)
	}
	return origin, nil
}

func (m *Manager) consoleAddress(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", failure("invalid_origin", "控制台地址不能包含账号、查询参数或片段。", 400)
	}
	origin := *u
	origin.Path = ""
	origin.RawPath = ""
	if _, err = m.origin(origin.String()); err != nil {
		return "", err
	}
	return u.String(), nil
}

func (m *Manager) consoleURL(id string, c Connection) string {
	if raw := m.config.ConsoleURLs[id]; raw != "" {
		return raw
	}
	base := c.BaseURL
	if id == "eid" {
		base = m.config.EIDBaseURL
	}
	if id == "trust" {
		base = m.config.TrustBaseURL
	}
	origin, err := m.origin(base)
	if err != nil {
		return ""
	}
	return origin
}

func publicIP(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	a = a.Unmap()
	if !a.IsGlobalUnicast() || a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() {
		return false
	}
	for _, raw := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001:db8::/32", "64:ff9b::/96", "64:ff9b:1::/48"} {
		if netip.MustParsePrefix(raw).Contains(a) {
			return false
		}
	}
	return true
}

func (m *Manager) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errors.New("invalid upstream address")
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(ips) == 0 {
		return nil, errors.New("upstream resolution failed")
	}
	for _, ip := range ips {
		if !m.config.AllowPrivateNetwork && !publicIP(ip.IP) {
			return nil, errors.New("upstream network is not allowed")
		}
	}
	// Dial the checked addresses, not the hostname: DNS cannot change between
	// validation and connection. Environment proxies are deliberately disabled.
	dialer := &net.Dialer{Timeout: m.config.Timeout}
	for _, ip := range ips {
		conn, e := dialer.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
		if e == nil {
			return conn, nil
		}
	}
	return nil, errors.New("upstream connection failed")
}

func parseCredential(raw, product string) (Credential, error) {
	if strings.TrimSpace(raw) == "" {
		return Credential{}, failure("unconfigured", "请配置此项目独立外部账号的凭据。", 400)
	}
	if len(raw) > 16384 {
		return Credential{}, failure("invalid_credential", "凭据过长。", 400)
	}
	var c Credential
	if strings.HasPrefix(strings.TrimSpace(raw), "{") {
		d := json.NewDecoder(strings.NewReader(raw))
		d.DisallowUnknownFields()
		if err := d.Decode(&c); err != nil {
			return c, failure("invalid_credential", "凭据 JSON 格式无效。", 400)
		}
		if err := d.Decode(new(any)); err != io.EOF {
			return c, failure("invalid_credential", "凭据 JSON 格式无效。", 400)
		}
	} else {
		c = Credential{Kind: "user_bearer", Token: strings.TrimSpace(raw)}
	}
	if product == "storage" {
		if c.Kind != "access_key_pair" || c.AccessKey == "" || c.SecretKey == "" || c.Token != "" {
			return Credential{}, failure("invalid_credential", "存储服务需要 access_key_pair JSON，包含 accessKey 和 secretKey。", 400)
		}
	} else if c.Kind != "user_bearer" || c.Token == "" || c.AccessKey != "" || c.SecretKey != "" {
		return Credential{}, failure("invalid_credential", "此服务需要 user_bearer 用户会话令牌。", 400)
	}
	for _, v := range []string{c.Token, c.AccessKey, c.SecretKey} {
		if strings.ContainsAny(v, "\r\n") {
			return Credential{}, failure("invalid_credential", "凭据包含无效字符。", 400)
		}
	}
	return c, nil
}

func (m *Manager) request(ctx context.Context, base, method, path string, q url.Values, cred Credential, input, output any) error {
	origin, err := m.origin(base)
	if err != nil {
		return err
	}
	u := origin + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var body io.Reader
	if input != nil {
		b, e := json.Marshal(input)
		if e != nil {
			return failure("invalid_input", "请求数据无效。", 400)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return failure("invalid_input", "请求无效。", 400)
	}
	req.Header.Set("Accept", "application/json")
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cred.Kind == "user_bearer" {
		req.Header.Set("Authorization", "Bearer "+cred.Token)
	}
	if cred.Kind == "access_key_pair" {
		req.Header.Set("X-Access-Key", cred.AccessKey)
		req.Header.Set("X-Secret-Key", cred.SecretKey)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return failure("upstream_unavailable", "产品服务暂时无法连接，请稍后重试。", 502)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		e := failure("upstream_unavailable", "产品服务返回失败，请到原控制台检查。", 502)
		e.UpstreamStatus = resp.StatusCode
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			e.Code = "credential_rejected"
			e.Message = "产品凭据已失效或权限不足，请重新连接。"
		}
		if resp.StatusCode == 404 {
			e.Code = "upstream_not_found"
			e.Message = "产品服务中未找到对应记录。"
		}
		return e
	}
	const limit = 2 << 20
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || len(b) > limit {
		return failure("invalid_response", "产品响应无效或过大。", 502)
	}
	if output == nil {
		return nil
	}
	if len(b) == 0 || json.Unmarshal(b, output) != nil {
		return failure("invalid_response", "产品响应不符合已验证接口格式。", 502)
	}
	return nil
}
