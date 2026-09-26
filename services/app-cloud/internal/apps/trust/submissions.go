package trust

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
	"net/http"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func (m *Module) remember(ctx context.Context, q appkit.Execer, s appkit.Scope) error {
	p, e := m.seal(actor{ID: s.ActorID, Name: s.ActorName, Email: s.Email}, "actor:"+s.TenantID+":"+s.ProjectID+":"+s.ActorID)
	if e != nil {
		return e
	}
	now := appkit.Now()
	_, e = q.ExecContext(ctx, "INSERT INTO trust_actors VALUES(?,?,?,?,?,?) ON CONFLICT(tenant_id,project_id,actor_id) DO UPDATE SET profile=excluded.profile,last_seen=excluded.last_seen", s.TenantID, s.ProjectID, s.ActorID, p, now, now)
	return e
}
func (m *Module) bootstrap(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	if e := m.remember(r.Context(), m.rt.DB, s); e != nil {
		return e
	}
	schemes, e := listSchemes(r.Context(), m.rt.DB, s, s.Can("admin") || s.Can("review"))
	if e != nil {
		return e
	}
	subs, e := m.listSubmissions(r.Context(), s, s.ActorID, "", "", "")
	if e != nil {
		return e
	}
	appkit.JSON(w, 200, map[string]any{"schemes": schemes, "submissions": subs, "permissions": s.Permissions})
	return nil
}
func (m *Module) actor(ctx context.Context, q queryer, s appkit.Scope, id string) (actor, error) {
	var a actor
	var b []byte
	e := q.QueryRowContext(ctx, "SELECT profile,created_at,last_seen FROM trust_actors WHERE tenant_id=? AND project_id=? AND actor_id=?", s.TenantID, s.ProjectID, id).Scan(&b, &a.CreatedAt, &a.LastSeen)
	if e != nil {
		return a, e
	}
	created, last := a.CreatedAt, a.LastSeen
	e = m.open(b, "actor:"+s.TenantID+":"+s.ProjectID+":"+id, &a)
	a.CreatedAt = created
	a.LastSeen = last
	return a, e
}
func (m *Module) loadSubmission(ctx context.Context, q queryer, s appkit.Scope, id string, detail bool) (Submission, error) {
	var v Submission
	var body, reason []byte
	var fields string
	e := q.QueryRowContext(ctx, `SELECT v.id,v.scheme_id,c.name,v.actor_id,v.status,v.payload,v.fields,v.reason,v.version,v.created_at,v.updated_at,v.reviewed_at FROM trust_submissions v JOIN trust_schemes c ON c.id=v.scheme_id AND c.tenant_id=v.tenant_id AND c.project_id=v.project_id WHERE v.tenant_id=? AND v.project_id=? AND v.id=?`, s.TenantID, s.ProjectID, id).Scan(&v.ID, &v.SchemeID, &v.SchemeName, &v.ActorID, &v.Status, &body, &fields, &reason, &v.Version, &v.CreatedAt, &v.UpdatedAt, &v.ReviewedAt)
	if e != nil {
		return v, notFound(e)
	}
	if e = m.open(reason, "reason:"+v.ID, &v.Reason); e != nil {
		return v, e
	}
	a, e := m.actor(ctx, q, s, v.ActorID)
	if e != nil {
		return v, e
	}
	v.ActorName = a.Name
	v.Email = a.Email
	if detail {
		if e = m.open(body, "submission:"+v.ID, &v.Data); e != nil {
			return v, e
		}
		if e = json.Unmarshal([]byte(fields), &v.Fields); e != nil {
			return v, e
		}
		rows, e := q.QueryContext(ctx, "SELECT id,action,status,actor_id,reason,version,created_at FROM trust_history WHERE tenant_id=? AND project_id=? AND submission_id=? ORDER BY version,created_at,id", s.TenantID, s.ProjectID, id)
		if e != nil {
			return v, e
		}
		v.History = []History{}
		for rows.Next() {
			var h History
			var b []byte
			if e = rows.Scan(&h.ID, &h.Action, &h.Status, &h.ActorID, &b, &h.Version, &h.CreatedAt); e != nil {
				rows.Close()
				return v, e
			}
			if e = m.open(b, "history:"+h.ID, &h.Reason); e != nil {
				rows.Close()
				return v, e
			}
			v.History = append(v.History, h)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return v, e
		}
		v.Materials = []Material{}
		for _, f := range v.Fields {
			if f.Type == "file" || f.Type == "image" {
				var mid string
				_ = json.Unmarshal(v.Data[f.Name], &mid)
				if mid != "" {
					mat, _, e := m.getMaterial(ctx, q, s, mid, false)
					if e != nil {
						return v, e
					}
					v.Materials = append(v.Materials, mat)
				}
			}
		}
	}
	return v, nil
}
func (m *Module) listSubmissions(ctx context.Context, s appkit.Scope, actorID, status, schemeID, search string) ([]Submission, error) {
	query := "SELECT id FROM trust_submissions WHERE tenant_id=? AND project_id=?"
	args := scopeArgs(s)
	for _, f := range []struct{ k, v string }{{"actor_id", actorID}, {"status", status}, {"scheme_id", schemeID}} {
		if f.v != "" {
			query += " AND " + f.k + "=?"
			args = append(args, f.v)
		}
	}
	query += " ORDER BY updated_at DESC,id"
	rows, e := m.rt.DB.QueryContext(ctx, query, args...)
	if e != nil {
		return nil, e
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return nil, e
		}
		ids = append(ids, id)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	items := []Submission{}
	search = strings.ToLower(strings.TrimSpace(search))
	for _, id := range ids {
		v, e := m.loadSubmission(ctx, m.rt.DB, s, id, false)
		if e != nil {
			return nil, e
		}
		if search == "" || strings.Contains(strings.ToLower(v.ActorName+" "+v.Email+" "+v.ActorID), search) {
			items = append(items, v)
		}
	}
	return items, nil
}
func page(r *http.Request) (int, int) {
	p, _ := strconv.Atoi(r.URL.Query().Get("page"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if p < 0 {
		p = 0
	}
	if p > 1000000 {
		p = 1000000
	}
	if limit < 1 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	return p, limit
}
func paginated[T any](w http.ResponseWriter, r *http.Request, items []T) {
	p, n := page(r)
	start := p * n
	if start > len(items) {
		start = len(items)
	}
	end := start + n
	if end > len(items) {
		end = len(items)
	}
	appkit.JSON(w, 200, map[string]any{"items": items[start:end], "total": len(items), "page": p, "limit": n})
}
func (m *Module) ownSubmissions(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	items, e := m.listSubmissions(r.Context(), s, s.ActorID, r.URL.Query().Get("status"), r.URL.Query().Get("schemeId"), "")
	if e == nil {
		paginated(w, r, items)
	}
	return e
}
func (m *Module) reviewQueue(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	q := r.URL.Query()
	items, e := m.listSubmissions(r.Context(), s, q.Get("actorId"), q.Get("status"), q.Get("schemeId"), q.Get("search"))
	if e == nil {
		paginated(w, r, items)
	}
	return e
}
func (m *Module) ownDetail(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	var owner string
	if e := m.rt.DB.QueryRowContext(r.Context(), "SELECT actor_id FROM trust_submissions WHERE tenant_id=? AND project_id=? AND id=?", s.TenantID, s.ProjectID, r.PathValue("id")).Scan(&owner); e != nil {
		return notFound(e)
	}
	if owner != s.ActorID {
		return appkit.NotFound()
	}
	v, e := m.loadSubmission(r.Context(), m.rt.DB, s, r.PathValue("id"), true)
	if e != nil {
		return e
	}
	if v.ActorID != s.ActorID {
		return appkit.NotFound()
	}
	appkit.JSON(w, 200, v)
	return nil
}
func (m *Module) reviewDetail(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	v, e := m.loadSubmission(r.Context(), m.rt.DB, s, r.PathValue("id"), true)
	if e == nil {
		e = m.rt.Audit(r.Context(), nil, s, "material.read", v.ID, "审核人员查看申请材料")
	}
	if e == nil {
		appkit.JSON(w, 200, v)
	}
	return e
}
func (m *Module) validateData(ctx context.Context, q queryer, s appkit.Scope, scheme Scheme, data map[string]json.RawMessage, submissionID string) (map[string]json.RawMessage, error) {
	clean := map[string]json.RawMessage{}
	names := map[string]bool{}
	for _, f := range scheme.Fields {
		names[f.Name] = true
		raw := data[f.Name]
		var val string
		var num float64
		empty := len(raw) == 0 || string(raw) == "null" || string(raw) == `""`
		if empty {
			if f.Required {
				return nil, appkit.Invalid(f.Label + " 为必填项")
			}
			continue
		}
		if f.Type == "number" {
			if json.Unmarshal(raw, &num) != nil {
				return nil, appkit.Invalid(f.Label + " 必须是数字")
			}
			if f.Validations.Min != nil && num < *f.Validations.Min || f.Validations.Max != nil && num > *f.Validations.Max {
				return nil, appkit.Invalid(f.Label + " 超出允许范围")
			}
			clean[f.Name] = raw
			continue
		}
		if json.Unmarshal(raw, &val) != nil || len(val) > 80000 {
			return nil, appkit.Invalid(f.Label + " 内容无效")
		}
		if strings.TrimSpace(val) == "" && f.Required {
			return nil, appkit.Invalid(f.Label + " 为必填项")
		}
		if f.Type == "file" || f.Type == "image" {
			var owner, schemeID, fieldName, linked string
			e := q.QueryRowContext(ctx, "SELECT actor_id,scheme_id,field_name,submission_id FROM trust_materials WHERE id=? AND tenant_id=? AND project_id=?", val, s.TenantID, s.ProjectID).Scan(&owner, &schemeID, &fieldName, &linked)
			if e != nil || owner != s.ActorID || schemeID != scheme.ID || fieldName != f.Name || (linked != "" && linked != submissionID) {
				return nil, appkit.Invalid("材料不属于当前申请人或字段")
			}
			clean[f.Name] = raw
			continue
		}
		length := len([]rune(val))
		max := 20000
		if f.Type != "longText" {
			max = 2000
		}
		if length > max || f.Validations.MinLength != nil && length < *f.Validations.MinLength || f.Validations.MaxLength != nil && length > *f.Validations.MaxLength {
			return nil, appkit.Invalid(f.Label + " 长度不符合要求")
		}
		if f.Validations.Pattern != "" {
			ok, e := regexp.MatchString(f.Validations.Pattern, val)
			if e != nil || !ok {
				return nil, appkit.Invalid(f.Label + " 格式不符合要求")
			}
		}
		switch f.Type {
		case "email":
			a, e := mail.ParseAddress(val)
			if e != nil || a.Address != val {
				return nil, appkit.Invalid("邮箱格式无效")
			}
		case "date":
			if _, e := time.Parse("2006-01-02", val); e != nil {
				return nil, appkit.Invalid("日期格式无效")
			}
		case "select":
			found := false
			for _, o := range f.Options {
				if val == o {
					found = true
				}
			}
			if !found {
				return nil, appkit.Invalid("请选择有效选项")
			}
		}
		clean[f.Name] = raw
	}
	for name := range data {
		if !names[name] {
			return nil, appkit.Invalid("提交包含方案之外的字段")
		}
	}
	return clean, nil
}
func (m *Module) history(ctx context.Context, tx *sql.Tx, s appkit.Scope, id, action, status, reason string, version int) error {
	hid := appkit.NewID("th_")
	b, e := m.seal(reason, "history:"+hid)
	if e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, "INSERT INTO trust_history VALUES(?,?,?,?,?,?,?,?,?,?)", hid, id, s.TenantID, s.ProjectID, s.ActorID, action, status, b, version, appkit.Now())
	return e
}
func (m *Module) submit(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	var body struct {
		SchemeID      string                     `json:"schemeId"`
		SchemeVersion int                        `json:"schemeVersion"`
		Version       int                        `json:"version"`
		Data          map[string]json.RawMessage `json:"data"`
	}
	if e := appkit.Decode(w, r, &body); e != nil {
		return e
	}
	id := ""
	e := m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		scheme, e := getScheme(r.Context(), tx, s, body.SchemeID)
		if e != nil {
			return e
		}
		if scheme.Status != "active" {
			return appkit.Conflict("方案已停用")
		}
		if body.SchemeVersion != scheme.Version {
			return appkit.Conflict("方案字段已变化，请重新载入")
		}
		status := ""
		version := 0
		e = tx.QueryRowContext(r.Context(), "SELECT id,status,version FROM trust_submissions WHERE tenant_id=? AND project_id=? AND scheme_id=? AND actor_id=?", s.TenantID, s.ProjectID, scheme.ID, s.ActorID).Scan(&id, &status, &version)
		if e != nil && e != sql.ErrNoRows {
			return e
		}
		if status == "approved" {
			return appkit.Conflict("已通过的认证无需重复提交")
		}
		if body.Version != version {
			return appkit.Conflict("申请已被更新，请刷新后重试")
		}
		fresh := id == ""
		if fresh {
			id = appkit.NewID("tsu_")
		}
		clean, e := m.validateData(r.Context(), tx, s, scheme, body.Data, id)
		if e != nil {
			return e
		}
		enc, e := m.seal(clean, "submission:"+id)
		if e != nil {
			return e
		}
		reason, e := m.seal("", "reason:"+id)
		if e != nil {
			return e
		}
		fields, _ := json.Marshal(scheme.Fields)
		now := appkit.Now()
		version++
		if fresh {
			_, e = tx.ExecContext(r.Context(), "INSERT INTO trust_submissions VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)", id, s.TenantID, s.ProjectID, scheme.ID, s.ActorID, "pending", enc, string(fields), reason, version, now, now, "")
		} else {
			_, e = tx.ExecContext(r.Context(), "UPDATE trust_submissions SET payload=?,fields=?,status='pending',reason=?,version=?,updated_at=?,reviewed_at='' WHERE id=? AND tenant_id=? AND project_id=?", enc, string(fields), reason, version, now, id, s.TenantID, s.ProjectID)
		}
		if e != nil {
			return e
		}
		for _, f := range scheme.Fields {
			if f.Type == "file" || f.Type == "image" {
				var mid string
				_ = json.Unmarshal(clean[f.Name], &mid)
				if mid != "" {
					if _, e = tx.ExecContext(r.Context(), "UPDATE trust_materials SET submission_id=? WHERE id=? AND tenant_id=? AND project_id=? AND actor_id=?", id, mid, s.TenantID, s.ProjectID, s.ActorID); e != nil {
						return e
					}
				}
			}
		}
		if e = m.remember(r.Context(), tx, s); e != nil {
			return e
		}
		action := "submit"
		if !fresh {
			action = "resubmit"
		}
		if e = m.history(r.Context(), tx, s, id, action, "pending", "", version); e != nil {
			return e
		}
		if e = m.enqueue(r.Context(), tx, s, id, scheme.Name, action, "pending"); e != nil {
			return e
		}
		return m.rt.Audit(r.Context(), tx, s, action, id, "提交认证申请")
	})
	if e != nil {
		return e
	}
	m.deliverFor(r.Context(), s, id)
	v, e := m.loadSubmission(r.Context(), m.rt.DB, s, id, true)
	if e == nil {
		appkit.JSON(w, 201, v)
	}
	return e
}
func (m *Module) review(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	var b struct {
		Status  string `json:"status"`
		Reason  string `json:"reason"`
		Version int    `json:"version"`
	}
	if e := appkit.Decode(w, r, &b); e != nil {
		return e
	}
	b.Reason = strings.TrimSpace(b.Reason)
	if b.Status != "approved" && b.Status != "rejected" {
		return appkit.Invalid("审核状态无效")
	}
	if b.Status == "rejected" && b.Reason == "" || len(b.Reason) > 10000 {
		return appkit.Invalid("拒绝需填写原因，最多 10000 字节")
	}
	if b.Status == "approved" {
		b.Reason = ""
	}
	id := r.PathValue("id")
	e := m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		v, e := m.loadSubmission(r.Context(), tx, s, id, false)
		if e != nil {
			return e
		}
		if v.Version != b.Version {
			return appkit.Conflict("申请已被重提或审核，请刷新材料后重试")
		}
		reason, e := m.seal(b.Reason, "reason:"+id)
		if e != nil {
			return e
		}
		now := appkit.Now()
		_, e = tx.ExecContext(r.Context(), "UPDATE trust_submissions SET status=?,reason=?,version=version+1,updated_at=?,reviewed_at=? WHERE id=? AND tenant_id=? AND project_id=?", b.Status, reason, now, now, id, s.TenantID, s.ProjectID)
		if e != nil {
			return e
		}
		action := "approve"
		if b.Status == "rejected" {
			action = "reject"
		}
		if e = m.history(r.Context(), tx, s, id, action, b.Status, b.Reason, v.Version+1); e != nil {
			return e
		}
		if e = m.enqueue(r.Context(), tx, s, id, v.SchemeName, action, b.Status); e != nil {
			return e
		}
		return m.rt.Audit(r.Context(), tx, s, "review."+action, id, "更新认证审核结果")
	})
	if e != nil {
		return e
	}
	m.deliverFor(r.Context(), s, id)
	v, e := m.loadSubmission(r.Context(), m.rt.DB, s, id, true)
	if e == nil {
		appkit.JSON(w, 200, v)
	}
	return e
}
func (m *Module) stats(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	out := map[string]int{}
	for name, q := range map[string]string{"users": "SELECT count(*) FROM trust_actors WHERE tenant_id=? AND project_id=?", "schemes": "SELECT count(*) FROM trust_schemes WHERE tenant_id=? AND project_id=? AND status='active'", "total": "SELECT count(*) FROM trust_submissions WHERE tenant_id=? AND project_id=?", "pending": "SELECT count(*) FROM trust_submissions WHERE tenant_id=? AND project_id=? AND status='pending'", "approved": "SELECT count(*) FROM trust_submissions WHERE tenant_id=? AND project_id=? AND status='approved'", "rejected": "SELECT count(*) FROM trust_submissions WHERE tenant_id=? AND project_id=? AND status='rejected'"} {
		var n int
		if e := m.rt.DB.QueryRowContext(r.Context(), q, s.TenantID, s.ProjectID).Scan(&n); e != nil {
			return e
		}
		out[name] = n
	}
	appkit.JSON(w, 200, out)
	return nil
}
func (m *Module) users(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	rows, e := m.rt.DB.QueryContext(r.Context(), "SELECT actor_id FROM trust_actors WHERE tenant_id=? AND project_id=? ORDER BY created_at DESC,actor_id", s.TenantID, s.ProjectID)
	if e != nil {
		return e
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return e
		}
		ids = append(ids, id)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	search := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("search")))
	items := []actor{}
	for _, id := range ids {
		a, e := m.actor(r.Context(), m.rt.DB, s, id)
		if e != nil {
			return e
		}
		if search == "" || strings.Contains(strings.ToLower(a.ID+" "+a.Name+" "+a.Email), search) {
			items = append(items, a)
		}
	}
	paginated(w, r, items)
	return nil
}
