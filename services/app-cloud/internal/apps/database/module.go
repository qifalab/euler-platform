// Package database rebuilds the ecloud-database business surface inside Euler.
// Original product: https://github.com/ctipscn/ecloud-database (Apache-2.0).
package database

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"hash/fnv"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
)

type Module struct {
	rt      *appkit.Runtime
	engines map[string]engineConfig
	initErr error
	ledger  ledger
	locks   [64]sync.Mutex
	once    sync.Once
	cancel  context.CancelFunc
	done    chan struct{}
}
type Resource struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	Database   string `json:"database"`
	Username   string `json:"username"`
	Status     string `json:"status"`
	SizeBytes  int64  `json:"sizeBytes"`
	CreatedAt  string `json:"createdAt"`
	UpdatedAt  string `json:"updatedAt"`
	LastError  string `json:"lastError,omitempty"`
	credential []byte
}

func New(rt *appkit.Runtime) *Module {
	engines, e := readEngines()
	m := &Module{rt: rt, engines: engines, initErr: e}
	m.ledger = ledger{rt: rt, prefix: "database", base: Quota{MySQLMB: int64(envInt("EULER_DATABASE_DEFAULT_MYSQL_MB", 100)), MySQLCount: int64(envInt("EULER_DATABASE_DEFAULT_MYSQL_COUNT", 1)), PostgresMB: int64(envInt("EULER_DATABASE_DEFAULT_POSTGRES_MB", 100)), PostgresCount: int64(envInt("EULER_DATABASE_DEFAULT_POSTGRES_COUNT", 1))}}
	return m
}
func (m *Module) ID() string { return "database" }
func (m *Module) Migrate(ctx context.Context) error {
	if m.initErr != nil {
		return m.initErr
	}
	_, e := m.rt.DB.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS database_instances(id TEXT PRIMARY KEY,tenant_id TEXT NOT NULL,project_id TEXT NOT NULL,installation_id TEXT NOT NULL,name TEXT NOT NULL,type TEXT NOT NULL,db_name TEXT NOT NULL UNIQUE,db_user TEXT NOT NULL UNIQUE,credential BLOB NOT NULL,status TEXT NOT NULL,size_bytes INTEGER NOT NULL DEFAULT 0,last_error TEXT NOT NULL DEFAULT '',created_at TEXT NOT NULL,updated_at TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS database_instances_scope ON database_instances(tenant_id,project_id,status);
CREATE TABLE IF NOT EXISTS database_operations(id TEXT PRIMARY KEY,tenant_id TEXT NOT NULL,project_id TEXT NOT NULL,actor_id TEXT NOT NULL,resource_id TEXT NOT NULL,action TEXT NOT NULL,state TEXT NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL);`)
	if e != nil {
		return e
	}
	if e = m.ledger.migrate(ctx); e != nil {
		return e
	}
	if os.Getenv("EULER_DATABASE_DISABLE_SCHEDULER") != "true" {
		m.once.Do(func() {
			ctx, m.cancel = context.WithCancel(ctx)
			m.done = make(chan struct{})
			go func() {
				defer close(m.done)
				ticker := time.NewTicker(time.Duration(max(1, envInt("EULER_DATABASE_QUOTA_INTERVAL_MINUTES", 5))) * time.Minute)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						m.maintainAll(ctx)
					}
				}
			}()
		})
	}
	return nil
}
func (m *Module) Handler() http.Handler {
	mux := http.NewServeMux()
	appkit.Handle(mux, "GET /", "read", m.overview)
	appkit.Handle(mux, "GET /quota", "read", m.overview)
	appkit.Handle(mux, "GET /databases", "read", m.list)
	appkit.Handle(mux, "POST /databases", "write", m.create)
	appkit.Handle(mux, "GET /databases/{id}", "read", m.detail)
	appkit.Handle(mux, "GET /databases/{id}/credentials", "secrets", m.credentials)
	appkit.Handle(mux, "POST /databases/{id}/password", "secrets", m.rotate)
	appkit.Handle(mux, "DELETE /databases/{id}", "manage", m.remove)
	appkit.Handle(mux, "POST /admin/check-quota", "admin", m.checkQuota)
	appkit.Handle(mux, "POST /admin/fix-pg-permissions", "admin", m.repair)
	appkit.Handle(mux, "GET /admin/accounts", "admin", m.overview)
	appkit.Handle(mux, "GET /admin/operations", "admin", m.operations)
	m.ledger.register(mux)
	return mux
}
func (m *Module) PublicHandler() http.Handler { return http.NotFoundHandler() }
func (m *Module) lock(s appkit.Scope) func() {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s.TenantID + "/" + s.ProjectID))
	v := &m.locks[h.Sum32()%uint32(len(m.locks))]
	v.Lock()
	return v.Unlock
}
func (m *Module) aad(s appkit.Scope, id string) string {
	return "database:" + s.TenantID + ":" + s.ProjectID + ":" + id
}
func generated(prefix string) string {
	b := make([]byte, 12)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return prefix + hex.EncodeToString(b)
}

const columns = "id,name,type,db_name,db_user,status,size_bytes,created_at,updated_at,last_error,credential"

type scanner interface{ Scan(...any) error }

func scanResource(row scanner) (Resource, error) {
	var v Resource
	e := row.Scan(&v.ID, &v.Name, &v.Type, &v.Database, &v.Username, &v.Status, &v.SizeBytes, &v.CreatedAt, &v.UpdatedAt, &v.LastError, &v.credential)
	return v, e
}
func (m *Module) get(ctx context.Context, s appkit.Scope, id string) (Resource, error) {
	return scanResource(m.rt.DB.QueryRowContext(ctx, "SELECT "+columns+" FROM database_instances WHERE tenant_id=? AND project_id=? AND id=? AND status<>'deleted'", s.TenantID, s.ProjectID, id))
}
func (m *Module) resources(ctx context.Context, s appkit.Scope) ([]Resource, error) {
	rows, e := m.rt.DB.QueryContext(ctx, "SELECT "+columns+" FROM database_instances WHERE tenant_id=? AND project_id=? AND status<>'deleted' ORDER BY created_at DESC", s.TenantID, s.ProjectID)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Resource{}
	for rows.Next() {
		v, e := scanResource(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (m *Module) list(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	items, e := m.resources(r.Context(), s)
	if e != nil {
		return e
	}
	appkit.JSON(w, 200, map[string]any{"items": items})
	return nil
}
func (m *Module) detail(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	v, e := m.get(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	appkit.JSON(w, 200, v)
	return nil
}
func (m *Module) overview(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	items, e := m.resources(r.Context(), s)
	if e != nil {
		return e
	}
	q, e := m.ledger.quota(r.Context(), m.rt.DB, s)
	if e != nil {
		return e
	}
	usage := Quota{}
	for _, v := range items {
		if v.Type == "mysql" {
			usage.MySQLCount++
			usage.MySQLMB += v.SizeBytes
		} else {
			usage.PostgresCount++
			usage.PostgresMB += v.SizeBytes
		}
	}
	engines := map[string]bool{"mysql": m.engines["mysql"].Engine != nil, "postgresql": m.engines["postgresql"].Engine != nil}
	appkit.JSON(w, 200, map[string]any{"quota": q, "mysqlUsedBytes": usage.MySQLMB, "mysqlCount": usage.MySQLCount, "postgresUsedBytes": usage.PostgresMB, "postgresCount": usage.PostgresCount, "engines": engines, "items": items})
	return nil
}
func (m *Module) intent(ctx context.Context, s appkit.Scope, id, action string) (string, error) {
	op := appkit.NewID("op_")
	e := m.rt.Transaction(ctx, func(tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, "INSERT INTO database_operations VALUES(?,?,?,?,?,?,?,?,?)", op, s.TenantID, s.ProjectID, s.ActorID, id, action, "pending", appkit.Now(), appkit.Now())
		if e != nil {
			return e
		}
		return m.rt.Audit(ctx, tx, s, action+".started", id, "已授权数据库引擎操作")
	})
	return op, e
}

// Completion survives browser cancellation and permission changes after the
// external operation began, so real engine mutations always retain evidence.
func (m *Module) complete(s appkit.Scope, op, id, action, status string, engineErr error, credential []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	state := "completed"
	message := ""
	if engineErr != nil {
		state = "uncertain"
		status = "error"
		message = "引擎操作未确认完成，请运营人员核验后处理"
	}
	return m.rt.Transaction(ctx, func(tx *sql.Tx) error {
		var e error
		if credential != nil {
			_, e = tx.ExecContext(ctx, "UPDATE database_instances SET status=?,last_error=?,credential=?,updated_at=? WHERE id=? AND tenant_id=? AND project_id=?", status, message, credential, appkit.Now(), id, s.TenantID, s.ProjectID)
		} else {
			_, e = tx.ExecContext(ctx, "UPDATE database_instances SET status=?,last_error=?,updated_at=? WHERE id=? AND tenant_id=? AND project_id=?", status, message, appkit.Now(), id, s.TenantID, s.ProjectID)
		}
		if e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, "UPDATE database_operations SET state=?,updated_at=? WHERE id=?", state, appkit.Now(), op); e != nil {
			return e
		}
		return m.rt.Audit(ctx, tx, s, action+"."+state, id, "数据库引擎操作结果已记录")
	})
}
func (m *Module) create(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	var in struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	if e := appkit.Decode(w, r, &in); e != nil {
		return e
	}
	name, e := appkit.Name(in.Name, 100)
	if e != nil {
		return e
	}
	engine := m.engines[in.Type].Engine
	if engine == nil {
		return appkit.Unavailable("此数据库引擎尚未配置")
	}
	unlock := m.lock(s)
	defer unlock()
	id := appkit.NewID("db_")
	password := generated("p_")
	cipher, e := m.rt.Encrypt(password, m.aad(s, id))
	if e != nil {
		return e
	}
	v := Resource{ID: id, Name: name, Type: in.Type, Database: generated("e_"), Username: generated("u_"), Status: "provisioning", CreatedAt: appkit.Now(), UpdatedAt: appkit.Now()}
	op := appkit.NewID("op_")
	e = m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		q, e := m.ledger.quota(r.Context(), tx, s)
		if e != nil {
			return e
		}
		var count, size int64
		if e = tx.QueryRowContext(r.Context(), "SELECT COUNT(*),COALESCE(SUM(size_bytes),0) FROM database_instances WHERE tenant_id=? AND project_id=? AND type=? AND status<>'deleted'", s.TenantID, s.ProjectID, in.Type).Scan(&count, &size); e != nil {
			return e
		}
		maxCount, maxMB := q.MySQLCount, q.MySQLMB
		if in.Type == "postgresql" {
			maxCount, maxMB = q.PostgresCount, q.PostgresMB
		}
		if count >= maxCount || size >= maxMB*1024*1024 {
			return appkit.Conflict("当前项目的数据库数量或容量已达配额")
		}
		_, e = tx.ExecContext(r.Context(), "INSERT INTO database_instances VALUES(?,?,?,?,?,?,?,?,?,?,0,'',?,?)", v.ID, s.TenantID, s.ProjectID, s.InstallationID, v.Name, v.Type, v.Database, v.Username, cipher, v.Status, v.CreatedAt, v.UpdatedAt)
		if e != nil {
			return e
		}
		_, e = tx.ExecContext(r.Context(), "INSERT INTO database_operations VALUES(?,?,?,?,?,?,?,?,?)", op, s.TenantID, s.ProjectID, s.ActorID, id, "create", "pending", appkit.Now(), appkit.Now())
		if e != nil {
			return e
		}
		return m.rt.Audit(r.Context(), tx, s, "create.started", id, "预留项目配额并开始创建数据库")
	})
	if e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 60*time.Second)
	defer cancel()
	engineErr := engine.Create(ctx, v.Database, v.Username, password)
	if e = m.complete(s, op, id, "create", "active", engineErr, nil); e != nil {
		return e
	}
	if engineErr != nil {
		return appkit.Unavailable("数据库创建结果未确认，已保留操作记录和资源标识")
	}
	v.Status = "active"
	appkit.JSON(w, 201, v)
	return nil
}
func (m *Module) credentials(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	v, e := m.get(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	if v.Status != "active" && v.Status != "readonly" {
		return appkit.Conflict("数据库尚未处于可连接状态")
	}
	password, e := m.rt.Decrypt(v.credential, m.aad(s, v.ID))
	if e != nil {
		return e
	}
	cfg := m.engines[v.Type]
	if cfg.Engine == nil {
		return appkit.Unavailable("引擎未配置")
	}
	if e = m.rt.Audit(r.Context(), nil, s, "credentials.viewed", v.ID, "查看数据库连接凭据"); e != nil {
		return e
	}
	tools := []map[string]string{}
	if cfg.ToolURL != "" {
		tools = append(tools, map[string]string{"name": "phpMyAdmin", "url": cfg.ToolURL})
	}
	if base := os.Getenv("EULER_DATABASE_ADMINER_URL"); base != "" {
		u, _ := url.Parse(base)
		q := u.Query()
		q.Set("username", v.Username)
		q.Set("db", v.Database)
		host := cfg.Host + ":" + strconv.Itoa(cfg.Port)
		if v.Type == "mysql" {
			q.Set("mysql", "")
			q.Set("server", host)
		} else {
			q.Set("pgsql", host)
		}
		u.RawQuery = q.Encode()
		tools = append(tools, map[string]string{"name": "Adminer", "url": u.String()})
	}
	appkit.JSON(w, 200, map[string]any{"host": cfg.Host, "port": cfg.Port, "database": v.Database, "username": v.Username, "password": password, "tools": tools})
	return nil
}
func (m *Module) rotate(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	unlock := m.lock(s)
	defer unlock()
	v, e := m.get(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	if v.Status != "active" && v.Status != "readonly" {
		return appkit.Conflict("当前状态不能轮换密码")
	}
	engine := m.engines[v.Type].Engine
	if engine == nil {
		return appkit.Unavailable("引擎未配置")
	}
	password := generated("p_")
	cipher, e := m.rt.Encrypt(password, m.aad(s, v.ID))
	if e != nil {
		return e
	}
	op, e := m.intent(r.Context(), s, v.ID, "password.rotate")
	if e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
	defer cancel()
	engineErr := engine.Rotate(ctx, v.Database, v.Username, password)
	if e = m.complete(s, op, v.ID, "password.rotate", v.Status, engineErr, cipher); e != nil {
		return e
	}
	if engineErr != nil {
		return appkit.Unavailable("密码轮换结果未确认，请运营人员核验")
	}
	appkit.JSON(w, 200, map[string]bool{"rotated": true})
	return nil
}
func (m *Module) remove(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	unlock := m.lock(s)
	defer unlock()
	v, e := m.get(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	engine := m.engines[v.Type].Engine
	if engine == nil {
		return appkit.Unavailable("引擎未配置")
	}
	op, e := m.intent(r.Context(), s, v.ID, "delete")
	if e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 60*time.Second)
	defer cancel()
	engineErr := engine.Drop(ctx, v.Database, v.Username)
	if e = m.complete(s, op, v.ID, "delete", "deleted", engineErr, nil); e != nil {
		return e
	}
	if engineErr != nil {
		return appkit.Unavailable("删除结果未确认，记录和配额预留已保留")
	}
	appkit.JSON(w, 200, map[string]bool{"deleted": true})
	return nil
}
func (m *Module) maintain(ctx context.Context, s appkit.Scope, repair bool) error {
	unlock := m.lock(s)
	defer unlock()
	items, e := m.resources(ctx, s)
	if e != nil {
		return e
	}
	totals := map[string]int64{}
	counts := map[string]int64{}
	for i := range items {
		v := &items[i]
		counts[v.Type]++
		if v.Status != "active" && v.Status != "readonly" {
			continue
		}
		engine := m.engines[v.Type].Engine
		if engine == nil {
			return appkit.Unavailable("配额检查所需引擎未配置")
		}
		size, e := engine.Size(ctx, v.Database)
		if e != nil {
			return appkit.Unavailable("无法读取真实数据库用量，未改变配额状态")
		}
		v.SizeBytes = size
		totals[v.Type] += size
	}
	q, e := m.ledger.quota(ctx, m.rt.DB, s)
	if e != nil {
		return e
	}
	for _, v := range items {
		if v.Status != "active" && v.Status != "readonly" {
			continue
		}
		limit, countLimit := q.MySQLMB, q.MySQLCount
		if v.Type == "postgresql" {
			limit = q.PostgresMB
			countLimit = q.PostgresCount
		}
		ro := totals[v.Type] > limit*1024*1024 || counts[v.Type] > countLimit
		target := "active"
		if ro {
			target = "readonly"
		}
		if v.Status != target || (repair && v.Type == "postgresql") {
			op, e := m.intent(ctx, s, v.ID, "quota.enforce")
			if e != nil {
				return e
			}
			engineErr := m.engines[v.Type].Engine.ReadOnly(ctx, v.Database, v.Username, ro)
			if e = m.complete(s, op, v.ID, "quota.enforce", target, engineErr, nil); e != nil {
				return e
			}
			if engineErr != nil {
				return appkit.Unavailable("引擎权限修改未完成，已记录待核验状态")
			}
		}
		if _, e = m.rt.DB.ExecContext(ctx, "UPDATE database_instances SET size_bytes=?,updated_at=? WHERE id=? AND tenant_id=? AND project_id=?", v.SizeBytes, appkit.Now(), v.ID, s.TenantID, s.ProjectID); e != nil {
			return e
		}
	}
	return nil
}
func (m *Module) checkQuota(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 90*time.Second)
	defer cancel()
	if e := m.maintain(ctx, s, false); e != nil {
		return e
	}
	return m.overview(w, r, s)
}
func (m *Module) repair(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 90*time.Second)
	defer cancel()
	if e := m.maintain(ctx, s, true); e != nil {
		return e
	}
	appkit.JSON(w, 200, map[string]bool{"repaired": true})
	return nil
}
func (m *Module) maintainAll(ctx context.Context) {
	rows, e := m.rt.DB.QueryContext(ctx, "SELECT DISTINCT tenant_id,project_id,installation_id FROM database_instances WHERE status IN ('active','readonly')")
	if e != nil {
		return
	}
	var scopes []appkit.Scope
	for rows.Next() {
		s := appkit.Scope{ActorID: "system", ApplicationID: "database"}
		if rows.Scan(&s.TenantID, &s.ProjectID, &s.InstallationID) == nil {
			scopes = append(scopes, s)
		}
	}
	rows.Close()
	for _, s := range scopes {
		if m.rt.ApplicationEnabled == nil {
			continue
		}
		enabled, err := m.rt.ApplicationEnabled(ctx, s.TenantID, s.ProjectID, "database")
		if err != nil || !enabled {
			continue
		}
		job, cancel := context.WithTimeout(ctx, 90*time.Second)
		_ = m.maintain(job, s, false)
		cancel()
	}
}
func (m *Module) operations(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	rows, e := m.rt.DB.QueryContext(r.Context(), "SELECT id,resource_id,action,state,created_at,updated_at FROM database_operations WHERE tenant_id=? AND project_id=? ORDER BY created_at DESC LIMIT 100", s.TenantID, s.ProjectID)
	if e != nil {
		return e
	}
	defer rows.Close()
	out := []map[string]string{}
	for rows.Next() {
		var id, resource, action, state, created, updated string
		if e = rows.Scan(&id, &resource, &action, &state, &created, &updated); e != nil {
			return e
		}
		out = append(out, map[string]string{"id": id, "resourceId": resource, "action": action, "state": state, "createdAt": created, "updatedAt": updated})
	}
	if e = rows.Err(); e != nil {
		return e
	}
	appkit.JSON(w, 200, map[string]any{"items": out})
	return nil
}
