package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ValidateConnection ignores client-supplied account identifiers. Only the
// upstream's authenticated current-user endpoint determines account ownership.
func (m *Manager) ValidateConnection(app string, c Connection) (Connection, error) {
	return m.ValidateConnectionContext(context.Background(), app, c)
}
func (m *Manager) ValidateConnectionContext(ctx context.Context, app string, c Connection) (Connection, error) {
	if !known(app) {
		return Connection{}, failure("unsupported", "未知应用。", 501)
	}
	if app == "eid" || app == "trust" {
		base := m.config.EIDBaseURL
		if app == "trust" {
			base = m.config.TrustBaseURL
		}
		origin, err := m.origin(base)
		if err != nil {
			return Connection{}, err
		}
		if c.Credential != "" {
			return Connection{}, failure("invalid_credential", "身份摘要不接受项目凭据。", 400)
		}
		if c.BaseURL != "" {
			candidate, err := m.origin(c.BaseURL)
			if err != nil || candidate != origin {
				return Connection{}, failure("invalid_origin", "身份摘要使用部署者配置的固定来源。", 400)
			}
		}
		return Connection{BaseURL: origin}, nil
	}
	origin, err := m.origin(c.BaseURL)
	if err != nil {
		return Connection{}, err
	}
	c.BaseURL = origin
	if app == "statistics" || app == "lottery" || app == "witshield" {
		if c.Credential != "" {
			return Connection{}, failure("unsupported", "此应用仅连接独立实例入口，不接受管理员凭据。", 501)
		}
		c.ExternalAccountID = "instance:" + origin
		return c, nil
	}
	cred, err := parseCredential(c.Credential, app)
	if err != nil {
		return Connection{}, err
	}
	id, err := m.accountID(ctx, app, c.BaseURL, cred)
	if err != nil {
		return Connection{}, err
	}
	c.ExternalAccountID = origin + "#" + id
	return c, nil
}

func (m *Manager) prepare(ctx context.Context, app string, c Connection) (Credential, error) {
	if c.ExternalAccountID == "" {
		return Credential{}, failure("unconfigured", "外部账号尚未验证，请重新保存连接。", 400)
	}
	validated, err := m.ValidateConnectionContext(ctx, app, c)
	if err != nil {
		return Credential{}, err
	}
	if validated.ExternalAccountID != c.ExternalAccountID {
		return Credential{}, failure("account_mismatch", "当前凭据与已绑定外部账号不一致，请重新连接。", 409)
	}
	return parseCredential(c.Credential, app)
}

type upstreamID string

func (id *upstreamID) UnmarshalJSON(data []byte) error {
	var value string
	if len(data) > 0 && data[0] == '"' {
		if err := json.Unmarshal(data, &value); err != nil {
			return err
		}
	} else {
		var number json.Number
		if err := json.Unmarshal(data, &number); err != nil {
			return err
		}
		value = number.String()
	}
	if value == "" || value == "null" || len(value) > 200 || strings.ContainsAny(value, "#\r\n\x00") {
		return errors.New("invalid account identifier")
	}
	*id = upstreamID(value)
	return nil
}

type storageQuota struct {
	UserID       upstreamID `json:"user_id"`
	StorageQuota *int64     `json:"storage_quota"`
	StorageUsed  *int64     `json:"storage_used"`
	BucketCount  *int64     `json:"bucket_count"`
}

func (m *Manager) accountID(ctx context.Context, app, base string, c Credential) (string, error) {
	var id upstreamID
	switch app {
	case "weauth", "database":
		path := "/api/auth/me"
		if app == "database" {
			path = "/api/users/me"
		}
		var out struct {
			ID upstreamID `json:"id"`
		}
		if err := m.request(ctx, base, http.MethodGet, path, nil, c, nil, &out); err != nil {
			return "", err
		}
		id = out.ID
	case "storage":
		var out storageQuota
		if err := m.request(ctx, base, http.MethodGet, "/api/s3/user/quota", nil, c, nil, &out); err != nil {
			return "", err
		}
		id = out.UserID
	default:
		return "", failure("unsupported", "此应用没有可用账号验证接口。", 501)
	}
	if id == "" {
		return "", failure("invalid_response", "上游未返回可验证的账号标识。", 502)
	}
	return string(id), nil
}

func summaryError(s Summary, err error) Summary {
	s.State = "unavailable"
	s.Message = "产品服务暂时不可用。"
	var e *Error
	if errors.As(err, &e) {
		s.Message = e.Message
		if e.Code == "unconfigured" {
			s.State = "unconfigured"
		}
		if e.Code == "unsupported" {
			s.State = "unsupported"
		}
	}
	return s
}

func (m *Manager) Summary(ctx context.Context, app string, c Connection, subject Subject) Summary {
	s := Summary{State: "ready", CheckedAt: time.Now().UTC(), ConsoleURL: m.consoleURL(app, c)}
	if !known(app) {
		return summaryError(s, failure("unsupported", "未知应用。", 501))
	}
	if app == "eid" || app == "trust" {
		return m.verificationSummary(ctx, app, c, subject, s)
	}
	if strings.TrimSpace(c.BaseURL) == "" {
		return summaryError(s, failure("unconfigured", "请先连接项目的独立实例或外部账号。", 400))
	}
	if app == "statistics" || app == "lottery" {
		validated, err := m.ValidateConnectionContext(ctx, app, c)
		if err != nil {
			return summaryError(s, err)
		}
		if c.ExternalAccountID != "" && validated.ExternalAccountID != c.ExternalAccountID {
			return summaryError(s, failure("account_mismatch", "实例地址与已保存绑定不一致。", 409))
		}
		s.State = "unsupported"
		s.Message = "已配置独立实例入口。当前适配器不读取业务摘要，请在原应用登录。"
		return s
	}
	if app == "witshield" {
		validated, err := m.ValidateConnectionContext(ctx, app, c)
		if err != nil {
			return summaryError(s, err)
		}
		if c.ExternalAccountID != "" && validated.ExternalAccountID != c.ExternalAccountID {
			return summaryError(s, failure("account_mismatch", "实例地址与已保存绑定不一致。", 409))
		}
		var health struct {
			Status string `json:"status"`
		}
		if err := m.request(ctx, c.BaseURL, http.MethodGet, "/readyz", nil, Credential{}, nil, &health); err != nil {
			return summaryError(s, err)
		}
		if health.Status != "ok" {
			return summaryError(s, failure("upstream_unavailable", "Controller 尚未就绪。", 502))
		}
		s.Message = "Controller 健康检查通过。设备与修复操作请在原控制台登录后管理。"
		s.Metrics = []Metric{{Label: "Controller", Value: "就绪"}}
		return s
	}
	if app == "storage" {
		cred, err := m.prepare(ctx, app, c)
		if err != nil {
			return summaryError(s, err)
		}
		var out storageQuota
		if err := m.request(ctx, c.BaseURL, http.MethodGet, "/api/s3/user/quota", nil, cred, nil, &out); err != nil {
			return summaryError(s, err)
		}
		if out.StorageQuota == nil || out.StorageUsed == nil || out.BucketCount == nil || *out.StorageQuota < 0 || *out.StorageUsed < 0 || *out.BucketCount < 0 {
			return summaryError(s, failure("invalid_response", "上游存储配额响应缺失或无效。", 502))
		}
		s.Message = "已读取外部账号的基础配额；以原控制台为准。"
		s.Metrics = []Metric{{Label: "存储桶", Value: strconv.FormatInt(*out.BucketCount, 10)}, {Label: "已用字节", Value: strconv.FormatInt(*out.StorageUsed, 10)}, {Label: "基础配额字节", Value: strconv.FormatInt(*out.StorageQuota, 10)}}
		return s
	}
	resources, err := m.Resources(ctx, app, c)
	if err != nil {
		return summaryError(s, err)
	}
	label := "站点"
	if app == "database" {
		label = "数据库"
	}
	s.Metrics = []Metric{{Label: label, Value: strconv.Itoa(len(resources))}}
	s.Message = "已读取项目独立外部账号的现有资源。"
	return s
}

func (m *Manager) verificationSummary(ctx context.Context, app string, c Connection, subject Subject, s Summary) Summary {
	if m.config.VerificationProvider == "" {
		return summaryError(s, failure("unconfigured", "部署者尚未配置资格查询的身份来源。", 400))
	}
	if subject.ID == "" || subject.Provider != m.config.VerificationProvider {
		return summaryError(s, failure("unsupported", "当前登录来源没有配置对应的资格查询。", 501))
	}
	if len(subject.ID) > 256 || strings.ContainsAny(subject.ID, "\r\n\x00") {
		return summaryError(s, failure("unsupported", "当前登录主体无法用于资格查询。", 501))
	}
	if app == "eid" {
		var out struct {
			Status       string `json:"status"`
			IdentityType string `json:"identity_type"`
		}
		err := m.request(ctx, m.config.EIDBaseURL, http.MethodGet, "/api/latest-verification/", url.Values{"oauth_id": {subject.ID}}, Credential{}, nil, &out)
		if upstreamNotFound(err) {
			s.Verification = &Verification{Status: "not_found"}
			s.Message = "未找到已通过的成员资格记录；此结果不授予平台权限。"
			return s
		}
		if err != nil {
			return summaryError(s, err)
		}
		if out.Status != "approved" || out.IdentityType == "" {
			return summaryError(s, failure("invalid_response", "成员资格响应不符合已验证格式。", 502))
		}
		s.Verification = &Verification{Status: out.Status, IdentityType: out.IdentityType}
		s.Message = "上游返回最新已通过记录；该接口不提供完整的有效期或撤销状态。"
		return s
	}
	if m.config.TrustSchemeID == "" {
		return summaryError(s, failure("unconfigured", "部署者尚未配置 Trust 认证方案。", 400))
	}
	var out struct {
		Success bool `json:"success"`
		Data    *struct {
			Verified *bool  `json:"verified"`
			Status   string `json:"status"`
		} `json:"data"`
	}
	err := m.request(ctx, m.config.TrustBaseURL, http.MethodGet, "/api/verification/status", url.Values{"oauthId": {subject.ID}, "schemeId": {m.config.TrustSchemeID}}, Credential{}, nil, &out)
	if upstreamNotFound(err) {
		s.Verification = &Verification{Status: "not_found"}
		s.Message = "信任中心未找到当前账号记录。"
		return s
	}
	if err != nil {
		return summaryError(s, err)
	}
	if !out.Success || out.Data == nil || out.Data.Verified == nil || (*out.Data.Verified != (out.Data.Status == "approved")) {
		return summaryError(s, failure("invalid_response", "认证状态响应无效。", 502))
	}
	switch out.Data.Status {
	case "approved", "pending", "rejected", "not_submitted":
	default:
		return summaryError(s, failure("invalid_response", "认证状态值不受支持。", 502))
	}
	s.Verification = &Verification{Status: out.Data.Status}
	s.Message = "已查询当前账号的指定认证方案，仅展示状态，不获取材料。"
	return s
}

func upstreamNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.UpstreamStatus == 404
}

type weauthSite struct {
	ID      uint64 `json:"id"`
	Name    string `json:"name"`
	Enabled *bool  `json:"enabled"`
}

func siteResource(site weauthSite) (Resource, error) {
	if site.ID == 0 || site.Name == "" || site.Enabled == nil {
		return Resource{}, failure("invalid_response", "站点响应缺少必要字段。", 502)
	}
	status := "disabled"
	if *site.Enabled {
		status = "enabled"
	}
	return Resource{ID: strconv.FormatUint(site.ID, 10), Name: site.Name, Type: "weauth.site", Status: status}, nil
}

func (m *Manager) Resources(ctx context.Context, app string, c Connection) ([]Resource, error) {
	if app != "weauth" && app != "database" && app != "storage" {
		return nil, failure("unsupported", "此应用暂不支持资源列表，请进入原控制台。", 501)
	}
	cred, err := m.prepare(ctx, app, c)
	if err != nil {
		return nil, err
	}
	resources := []Resource{}
	switch app {
	case "weauth":
		var sites []weauthSite
		if err := m.request(ctx, c.BaseURL, http.MethodGet, "/api/admin/sites", nil, cred, nil, &sites); err != nil {
			return nil, err
		}
		for _, site := range sites {
			resource, err := siteResource(site)
			if err != nil {
				return nil, err
			}
			resources = append(resources, resource)
		}
	case "database":
		// Deliberately omit connection_info: upstream includes plaintext database
		// passwords even in its list response. They must never enter our result.
		var out struct {
			Databases *[]struct {
				ID     string `json:"id"`
				Name   string `json:"name"`
				Type   string `json:"db_type"`
				Status string `json:"status"`
			} `json:"databases"`
		}
		if err := m.request(ctx, c.BaseURL, http.MethodGet, "/api/databases", nil, cred, nil, &out); err != nil {
			return nil, err
		}
		if out.Databases == nil {
			return nil, failure("invalid_response", "数据库列表响应缺少必要字段。", 502)
		}
		for _, db := range *out.Databases {
			if db.ID == "" || db.Name == "" || (db.Type != "mysql" && db.Type != "postgresql") || (db.Status != "active" && db.Status != "readonly" && db.Status != "deleted") {
				return nil, failure("invalid_response", "数据库列表响应无效。", 502)
			}
			resources = append(resources, Resource{ID: db.ID, Name: db.Name, Type: "database." + db.Type, Status: db.Status})
		}
	case "storage":
		var out struct {
			Buckets *[]struct {
				Name        string `json:"name"`
				DisplayName string `json:"display_name"`
				AccessType  string `json:"access_type"`
			} `json:"buckets"`
		}
		if err := m.request(ctx, c.BaseURL, http.MethodGet, "/api/s3/buckets", nil, cred, nil, &out); err != nil {
			return nil, err
		}
		if out.Buckets == nil {
			return nil, failure("invalid_response", "存储桶列表响应缺少必要字段。", 502)
		}
		for _, bucket := range *out.Buckets {
			if bucket.Name == "" {
				return nil, failure("invalid_response", "存储桶列表响应无效。", 502)
			}
			name := bucket.DisplayName
			if name == "" {
				name = bucket.Name
			}
			resources = append(resources, Resource{ID: bucket.Name, Name: name, Type: "storage.bucket", Status: bucket.AccessType})
		}
	}
	return resources, nil
}

var domainPattern = regexp.MustCompile(`^(\*\.)?[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?$`)

func (m *Manager) Create(ctx context.Context, app string, c Connection, input CreateResource) (Resource, error) {
	if app != "weauth" {
		return Resource{}, failure("unsupported", "此应用暂不支持在 Euler 创建资源。", 501)
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" || utf8.RuneCountInString(input.Name) > 100 || len(input.Domains) > 32 {
		return Resource{}, failure("invalid_input", "站点名称必填且不超过 100 字，域名最多 32 个。", 400)
	}
	for i, domain := range input.Domains {
		domain = strings.TrimSpace(domain)
		if len(domain) > 253 || !domainPattern.MatchString(domain) || strings.Contains(domain, "..") {
			return Resource{}, failure("invalid_input", "请使用有效域名，不要包含协议、端口或路径。", 400)
		}
		input.Domains[i] = domain
	}
	cred, err := m.prepare(ctx, app, c)
	if err != nil {
		return Resource{}, err
	}
	var site weauthSite
	if err := m.request(ctx, c.BaseURL, http.MethodPost, "/api/admin/sites", nil, cred, input, &site); err != nil {
		return Resource{}, err
	}
	return siteResource(site)
}

func (m *Manager) Delete(ctx context.Context, app string, c Connection, id string) error {
	if app != "weauth" {
		return failure("unsupported", "此应用暂不支持在 Euler 删除资源。", 501)
	}
	n, err := strconv.ParseUint(id, 10, 32)
	if err != nil || n == 0 || strconv.FormatUint(n, 10) != id {
		return failure("invalid_input", "站点 ID 无效。", 400)
	}
	cred, err := m.prepare(ctx, app, c)
	if err != nil {
		return err
	}
	return m.request(ctx, c.BaseURL, http.MethodDelete, "/api/admin/sites/"+id, nil, cred, nil, nil)
}
