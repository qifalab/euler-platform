package statistics

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	_ "time/tzdata" // Keep IANA day boundaries available in minimal deployment images.
	"unicode"
)

const storedTimeLayout = "2006-01-02T15:04:05.000000000Z"
const maxAnalysisRows = 200000
const maxDailyRows = 100000

var eventName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)

type analyticsSettings struct {
	RetentionDays int      `json:"retentionDays"`
	EventNames    []string `json:"eventNames"`
}

func (m *Module) migrateAnalytics(ctx context.Context) error {
	_, e := m.rt.DB.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS statistics_settings (
 site_id TEXT PRIMARY KEY REFERENCES statistics_sites(id) ON DELETE CASCADE,
 retention_days INTEGER NOT NULL DEFAULT 365, event_names TEXT NOT NULL DEFAULT '[]');
 CREATE TABLE IF NOT EXISTS statistics_events (
 id INTEGER PRIMARY KEY AUTOINCREMENT, site_id TEXT NOT NULL REFERENCES statistics_sites(id) ON DELETE CASCADE,
 visitor_hash TEXT NOT NULL, ip_hash TEXT NOT NULL, name TEXT NOT NULL, url TEXT NOT NULL, visited_at TEXT NOT NULL);
 CREATE INDEX IF NOT EXISTS statistics_events_site_time ON statistics_events(site_id,visited_at);
 CREATE INDEX IF NOT EXISTS statistics_events_rate ON statistics_events(site_id,ip_hash,visited_at);
 CREATE TABLE IF NOT EXISTS statistics_migrations(name TEXT PRIMARY KEY);`)
	if e != nil {
		return e
	}
	return m.normalizeTimestamps(ctx)
}
func (m *Module) getSettings(ctx context.Context, id string) (analyticsSettings, error) {
	v := analyticsSettings{RetentionDays: 365, EventNames: []string{}}
	var names string
	e := m.rt.DB.QueryRowContext(ctx, "SELECT retention_days,event_names FROM statistics_settings WHERE site_id=?", id).Scan(&v.RetentionDays, &names)
	if e == sql.ErrNoRows {
		return v, nil
	}
	if e != nil {
		return v, e
	}
	e = json.Unmarshal([]byte(names), &v.EventNames)
	return v, e
}
func (m *Module) settings(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	v, e := m.getSite(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	out, e := m.getSettings(r.Context(), v.ID)
	if e != nil {
		return e
	}
	appkit.JSON(w, 200, out)
	return nil
}
func (m *Module) saveSettings(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	v, e := m.getSite(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	var in analyticsSettings
	if e = appkit.DecodeLimit(w, r, &in, 4096); e != nil {
		return e
	}
	if in.RetentionDays < 7 || in.RetentionDays > 730 || len(in.EventNames) > 32 {
		return appkit.Invalid("留存须为 7–730 天，事件名最多 32 个")
	}
	seen := map[string]bool{}
	for _, n := range in.EventNames {
		if !eventName.MatchString(n) || seen[n] {
			return appkit.Invalid("事件名须为不重复的小写字母、数字或下划线，以字母开头，最长 40 字符")
		}
		seen[n] = true
	}
	if in.EventNames == nil {
		in.EventNames = []string{}
	}
	names, _ := json.Marshal(in.EventNames)
	e = m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		if _, e := tx.ExecContext(r.Context(), "INSERT INTO statistics_settings(site_id,retention_days,event_names) VALUES(?,?,?) ON CONFLICT(site_id) DO UPDATE SET retention_days=excluded.retention_days,event_names=excluded.event_names", v.ID, in.RetentionDays, string(names)); e != nil {
			return e
		}
		if _, e := prune(r.Context(), tx, v.ID, in.RetentionDays, 10000); e != nil {
			return e
		}
		return m.rt.Audit(r.Context(), tx, s, "analytics.settings.updated", v.ID, "更新统计留存与允许的事件名")
	})
	if e != nil {
		return e
	}
	appkit.JSON(w, 200, in)
	return nil
}

// Bounded physical deletion avoids holding the write lock for a large backlog.
// Reports always apply the retention cutoff, including rows awaiting deletion.
func prune(ctx context.Context, tx *sql.Tx, id string, days, limit int) (int64, error) {
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format(storedTimeLayout)
	var total int64
	for _, table := range []string{"statistics_views", "statistics_events"} {
		result, e := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE id IN (SELECT id FROM "+table+" WHERE site_id=? AND visited_at<? LIMIT ?)", id, cutoff, limit)
		if e != nil {
			return 0, e
		}
		n, e := result.RowsAffected()
		if e != nil {
			return 0, e
		}
		total += n
	}
	return total, nil
}
func (m *Module) cleanup(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	v, e := m.getSite(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	settings, e := m.getSettings(r.Context(), v.ID)
	if e != nil {
		return e
	}
	var deleted int64
	e = m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		var err error
		deleted, err = prune(r.Context(), tx, v.ID, settings.RetentionDays, 10000)
		if err != nil {
			return err
		}
		return m.rt.Audit(r.Context(), tx, s, "analytics.retention.cleaned", v.ID, fmt.Sprintf("清理 %d 条过期统计记录", deleted))
	})
	if e != nil {
		return e
	}
	appkit.JSON(w, 200, map[string]any{"deleted": deleted, "batchLimitPerTable": 10000})
	return nil
}
func (m *Module) acceptCollection(ctx context.Context, tx *sql.Tx, id, ipHash string) error {
	var enabled int
	if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM statistics_sites s JOIN installations i ON i.id=s.installation_id AND i.tenant_id=s.tenant_id AND i.project_id=s.project_id WHERE s.id=? AND s.enabled=1 AND i.status='enabled' AND i.application_id='statistics'", id).Scan(&enabled); e != nil {
		return e
	}
	if enabled == 0 {
		return appkit.NotFound()
	}
	now := time.Now().UTC()
	minute := now.Add(-time.Minute).Format(storedTimeLayout)
	day := now.Format("2006-01-02") + "T00:00:00.000000000Z"
	var rate, daily int
	for _, table := range []string{"statistics_views", "statistics_events"} {
		var n int
		if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE site_id=? AND ip_hash=? AND visited_at>?", id, ipHash, minute).Scan(&n); e != nil {
			return e
		}
		rate += n
		if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE site_id=? AND visited_at>=?", id, day).Scan(&n); e != nil {
			return e
		}
		daily += n
	}
	if rate >= 120 || daily >= maxDailyRows {
		return &appkit.Error{Status: 429, Code: "rate_limited", Message: "达到统计采集限额（每 IP 每分钟 120 条，每站点每天 100000 条）"}
	}
	days := 365
	e := tx.QueryRowContext(ctx, "SELECT retention_days FROM statistics_settings WHERE site_id=?", id).Scan(&days)
	if e != nil && e != sql.ErrNoRows {
		return e
	}
	_, e = prune(ctx, tx, id, days, 1000)
	return e
}
func (m *Module) collectEvent(w http.ResponseWriter, r *http.Request, s site) error {
	if e := origin(w, r, s, true); e != nil {
		return e
	}
	// No arbitrary attributes, identity, client timestamp or property map.
	// Registered names bound cardinality; server time prevents backdated injection.
	var in struct {
		VisitorID string `json:"visitor_id"`
		URL       string `json:"url"`
		Name      string `json:"name"`
	}
	if e := appkit.DecodeLimit(w, r, &in, 8192); e != nil {
		return e
	}
	if len(in.VisitorID) < 8 || len(in.VisitorID) > 128 || !eventName.MatchString(in.Name) {
		return appkit.Invalid("访客标识或事件名无效")
	}
	clean, e := cleanURL(in.URL)
	if e != nil {
		return e
	}
	u, _ := url.Parse(clean)
	o, _ := url.Parse(r.Header.Get("Origin"))
	if !matches(s, u.Host) || !strings.EqualFold(o.Host, u.Host) || o.Scheme != u.Scheme {
		return appkit.Forbidden("页面地址与来源不一致")
	}
	ip, _, e := net.SplitHostPort(r.RemoteAddr)
	if e != nil {
		ip = r.RemoteAddr
	}
	ipHash := m.digest(s.ID+"/ip", ip)
	e = m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		var names string
		e := tx.QueryRowContext(r.Context(), "SELECT event_names FROM statistics_settings WHERE site_id=?", s.ID).Scan(&names)
		if e == sql.ErrNoRows {
			return appkit.Invalid("请先在控制台登记事件名")
		}
		if e != nil {
			return e
		}
		var allowed []string
		if e = json.Unmarshal([]byte(names), &allowed); e != nil {
			return e
		}
		found := false
		for _, n := range allowed {
			if n == in.Name {
				found = true
				break
			}
		}
		if !found {
			return appkit.Invalid("事件名未登记")
		}
		if e = m.acceptCollection(r.Context(), tx, s.ID, ipHash); e != nil {
			return e
		}
		_, e = tx.ExecContext(r.Context(), "INSERT INTO statistics_events(site_id,visitor_hash,ip_hash,name,url,visited_at) VALUES(?,?,?,?,?,?)", s.ID, m.digest(s.ID+"/visitor", in.VisitorID), ipHash, in.Name, clean, time.Now().UTC().Format(storedTimeLayout))
		return e
	})
	if e != nil {
		return e
	}
	appkit.JSON(w, 202, map[string]bool{"success": true})
	return nil
}

type period struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Timezone string `json:"timezone"`
	Start    string `json:"startInclusive"`
	End      string `json:"endExclusive"`
	loc      *time.Location
}

func (m *Module) readPeriod(r *http.Request, id string, required bool) (period, error) {
	settings, e := m.getSettings(r.Context(), id)
	if e != nil {
		return period{}, e
	}
	now := time.Now().UTC()
	cutoff := now.AddDate(0, 0, -settings.RetentionDays)
	zone := r.URL.Query().Get("timezone")
	if zone == "" {
		zone = "UTC"
	}
	if len(zone) > 80 {
		return period{}, appkit.Invalid("时区无效")
	}
	loc, e := time.LoadLocation(zone)
	if e != nil {
		return period{}, appkit.Invalid("请使用 IANA 时区，例如 Asia/Shanghai")
	}
	p := period{From: r.URL.Query().Get("from"), To: r.URL.Query().Get("to"), Timezone: zone, loc: loc}
	var start, end time.Time
	if p.From == "" && p.To == "" && !required {
		start = cutoff
		end = now.Add(time.Second)
		p.From = start.In(loc).Format(time.DateOnly)
		p.To = now.In(loc).Format(time.DateOnly)
	} else {
		if p.From == "" && p.To == "" {
			p.From = now.In(loc).AddDate(0, 0, -29).Format(time.DateOnly)
			p.To = now.In(loc).Format(time.DateOnly)
		}
		start, e = time.ParseInLocation(time.DateOnly, p.From, loc)
		if e != nil {
			return p, appkit.Invalid("开始日期须为 YYYY-MM-DD")
		}
		end, e = time.ParseInLocation(time.DateOnly, p.To, loc)
		if e != nil {
			return p, appkit.Invalid("结束日期须为 YYYY-MM-DD")
		}
		end = end.AddDate(0, 0, 1)
		startCalendar, _ := time.Parse(time.DateOnly, p.From)
		endCalendar, _ := time.Parse(time.DateOnly, p.To)
		if !start.Before(end) || endCalendar.Sub(startCalendar) >= 366*24*time.Hour {
			return p, appkit.Invalid("日期范围须为 1–366 天")
		}
		if start.Before(cutoff) {
			start = cutoff
		}
		if !start.Before(end) {
			return p, appkit.Invalid("所选日期已超出数据留存范围")
		}
	}
	p.Start = start.UTC().Format(storedTimeLayout)
	p.End = end.UTC().Format(storedTimeLayout)
	e = m.rt.Transaction(r.Context(), func(tx *sql.Tx) error { _, e := prune(r.Context(), tx, id, settings.RetentionDays, 1000); return e })
	return p, e
}

type countGroup struct {
	Name           string `json:"name"`
	Count          int    `json:"count"`
	UniqueVisitors int    `json:"uniqueVisitors"`
}
type dailyPoint struct {
	Date           string `json:"date"`
	PageViews      int    `json:"pageViews"`
	UniqueVisitors int    `json:"uniqueVisitors"`
	Events         int    `json:"events"`
}
type analysis struct {
	Period         period       `json:"period"`
	Trend          []dailyPoint `json:"trend"`
	Sources        []countGroup `json:"sources"`
	Devices        []countGroup `json:"devices"`
	Browsers       []countGroup `json:"browsers"`
	Pages          []countGroup `json:"pages"`
	Events         []countGroup `json:"events"`
	PageViews      int          `json:"pageViews"`
	UniqueVisitors int          `json:"uniqueVisitors"`
	EventCount     int          `json:"eventCount"`
	Limit          int          `json:"analysisRowLimit"`
}
type accumulator struct {
	count    int
	visitors map[string]bool
}

func addGroup(groups map[string]*accumulator, name, visitor string) {
	v := groups[name]
	if v == nil {
		v = &accumulator{visitors: map[string]bool{}}
		groups[name] = v
	}
	v.count++
	v.visitors[visitor] = true
}
func groupsList(groups map[string]*accumulator) []countGroup {
	out := []countGroup{}
	for n, g := range groups {
		out = append(out, countGroup{n, g.count, len(g.visitors)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count == out[j].Count {
			return out[i].Name < out[j].Name
		}
		return out[i].Count > out[j].Count
	})
	return out
}
func classify(ua string) (string, string) {
	s := strings.ToLower(ua)
	device := "桌面"
	switch {
	case strings.Contains(s, "bot") || strings.Contains(s, "spider") || strings.Contains(s, "crawler"):
		device = "机器人"
	case strings.Contains(s, "ipad") || strings.Contains(s, "tablet"):
		device = "平板"
	case strings.Contains(s, "mobile") || strings.Contains(s, "android"):
		device = "手机"
	case s == "":
		device = "未知"
	}
	browser := "其他"
	switch {
	case strings.Contains(s, "edg/"):
		browser = "Edge"
	case strings.Contains(s, "opr/"):
		browser = "Opera"
	case strings.Contains(s, "firefox/") || strings.Contains(s, "fxios/"):
		browser = "Firefox"
	case strings.Contains(s, "chrome/") || strings.Contains(s, "crios/"):
		browser = "Chrome"
	case strings.Contains(s, "safari/"):
		browser = "Safari"
	case s == "":
		browser = "未知"
	}
	return device, browser
}
func (m *Module) analyze(ctx context.Context, id string, p period) (analysis, error) {
	out := analysis{Period: p, Trend: []dailyPoint{}, Limit: maxAnalysisRows}
	pages := map[string]*accumulator{}
	sources := map[string]*accumulator{}
	devices := map[string]*accumulator{}
	browsers := map[string]*accumulator{}
	events := map[string]*accumulator{}
	days := map[string]*accumulator{}
	eventDays := map[string]int{}
	visitors := map[string]bool{}
	rows, e := m.rt.DB.QueryContext(ctx, "SELECT visitor_hash,referrer,user_agent,visited_at,url FROM statistics_views WHERE site_id=? AND visited_at>=? AND visited_at<? ORDER BY visited_at,id LIMIT ?", id, p.Start, p.End, maxAnalysisRows+1)
	if e != nil {
		return out, e
	}
	for rows.Next() {
		var visitor, ref, ua, at, pageURL string
		if e = rows.Scan(&visitor, &ref, &ua, &at, &pageURL); e != nil {
			rows.Close()
			return out, e
		}
		out.PageViews++
		if out.PageViews > maxAnalysisRows {
			rows.Close()
			return out, appkit.Invalid("分析记录超过 200000 条，请缩小日期范围")
		}
		t, e := time.Parse(time.RFC3339Nano, at)
		if e != nil {
			rows.Close()
			return out, e
		}
		addGroup(days, t.In(p.loc).Format(time.DateOnly), visitor)
		visitors[visitor] = true
		source := "直接访问"
		if u, e := url.Parse(ref); e == nil && u.Host != "" {
			source = strings.ToLower(u.Host)
		}
		addGroup(sources, source, visitor)
		addGroup(pages, pageURL, visitor)
		device, browser := classify(ua)
		addGroup(devices, device, visitor)
		addGroup(browsers, browser, visitor)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return out, e
	}
	rows, e = m.rt.DB.QueryContext(ctx, "SELECT visitor_hash,name,visited_at FROM statistics_events WHERE site_id=? AND visited_at>=? AND visited_at<? ORDER BY visited_at,id LIMIT ?", id, p.Start, p.End, maxAnalysisRows-out.PageViews+1)
	if e != nil {
		return out, e
	}
	for rows.Next() {
		var visitor, name, at string
		if e = rows.Scan(&visitor, &name, &at); e != nil {
			rows.Close()
			return out, e
		}
		out.EventCount++
		if out.PageViews+out.EventCount > maxAnalysisRows {
			rows.Close()
			return out, appkit.Invalid("分析记录超过 200000 条，请缩小日期范围")
		}
		t, e := time.Parse(time.RFC3339Nano, at)
		if e != nil {
			rows.Close()
			return out, e
		}
		eventDays[t.In(p.loc).Format(time.DateOnly)]++
		addGroup(events, name, visitor)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return out, e
	}
	start, e := time.ParseInLocation(time.DateOnly, p.From, p.loc)
	if e != nil {
		return out, e
	}
	end, e := time.ParseInLocation(time.DateOnly, p.To, p.loc)
	if e != nil {
		return out, e
	}
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		date := d.Format(time.DateOnly)
		v := dailyPoint{Date: date, Events: eventDays[date]}
		if g := days[date]; g != nil {
			v.PageViews = g.count
			v.UniqueVisitors = len(g.visitors)
		}
		out.Trend = append(out.Trend, v)
	}
	out.UniqueVisitors = len(visitors)
	out.Sources = groupsList(sources)
	out.Pages = groupsList(pages)
	out.Devices = groupsList(devices)
	out.Browsers = groupsList(browsers)
	out.Events = groupsList(events)
	return out, nil
}
func (m *Module) analytics(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	v, e := m.getSite(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	p, e := m.readPeriod(r, v.ID, true)
	if e != nil {
		return e
	}
	out, e := m.analyze(r.Context(), v.ID, p)
	if e != nil {
		return e
	}
	appkit.JSON(w, 200, out)
	return nil
}
func csvSafe(value string) string {
	trim := strings.TrimLeftFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) || r == '\uFEFF' })
	if strings.IndexFunc(value, unicode.IsControl) >= 0 || (len(trim) > 0 && strings.ContainsRune("=+-@", rune(trim[0]))) {
		return "'" + value
	}
	return value
}
func (m *Module) exportCSV(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	v, e := m.getSite(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	p, e := m.readPeriod(r, v.ID, true)
	if e != nil {
		return e
	}
	out, e := m.analyze(r.Context(), v.ID, p)
	if e != nil {
		return e
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="statistics.csv"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	writer := csv.NewWriter(w)
	if e = writer.Write([]string{"kind", "site", "timezone", "from", "to", "name", "count", "unique_visitors"}); e != nil {
		return e
	}
	write := func(kind, name string, count, uv int) error {
		return writer.Write([]string{kind, csvSafe(v.Name), csvSafe(p.Timezone), p.From, p.To, csvSafe(name), fmt.Sprint(count), fmt.Sprint(uv)})
	}
	for _, d := range out.Trend {
		if e = write("daily_page_views", d.Date, d.PageViews, d.UniqueVisitors); e != nil {
			return e
		}
		if e = write("daily_events", d.Date, d.Events, 0); e != nil {
			return e
		}
	}
	for _, entry := range []struct {
		kind   string
		values []countGroup
	}{{"page", out.Pages}, {"source", out.Sources}, {"device", out.Devices}, {"browser", out.Browsers}, {"event", out.Events}} {
		for _, g := range entry.values {
			if e = write(entry.kind, g.Name, g.Count, g.UniqueVisitors); e != nil {
				return e
			}
		}
	}
	writer.Flush()
	return writer.Error()
}

type funnelStep struct {
	Name       string  `json:"name"`
	Visitors   int     `json:"visitors"`
	Conversion float64 `json:"conversion"`
}

func (m *Module) funnel(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	v, e := m.getSite(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	p, e := m.readPeriod(r, v.ID, true)
	if e != nil {
		return e
	}
	var in struct {
		Steps         []string `json:"steps"`
		WindowMinutes int      `json:"windowMinutes"`
	}
	if e = appkit.DecodeLimit(w, r, &in, 4096); e != nil {
		return e
	}
	if len(in.Steps) < 2 || len(in.Steps) > 8 || in.WindowMinutes < 1 || in.WindowMinutes > 10080 {
		return appkit.Invalid("漏斗须包含 2–8 个事件，转化窗口为 1–10080 分钟")
	}
	for _, n := range in.Steps {
		if !eventName.MatchString(n) {
			return appkit.Invalid("事件名无效")
		}
	}
	// Descending stage updates prevent one event satisfying repeated steps.
	// Latest viable prefix starts maximize the remaining conversion window.
	type progress struct {
		starts  []time.Time
		reached []bool
	}
	visitors := map[string]*progress{}
	counts := make([]int, len(in.Steps))
	window := time.Duration(in.WindowMinutes) * time.Minute
	rows, e := m.rt.DB.QueryContext(r.Context(), "SELECT visitor_hash,name,visited_at FROM statistics_events WHERE site_id=? AND visited_at>=? AND visited_at<? ORDER BY visited_at,id LIMIT ?", v.ID, p.Start, p.End, maxAnalysisRows+1)
	if e != nil {
		return e
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
		if n > maxAnalysisRows {
			return appkit.Invalid("漏斗记录超过 200000 条，请缩小日期范围")
		}
		var visitor, name, at string
		if e = rows.Scan(&visitor, &name, &at); e != nil {
			return e
		}
		t, e := time.Parse(time.RFC3339Nano, at)
		if e != nil {
			return e
		}
		state := visitors[visitor]
		if state == nil {
			state = &progress{starts: make([]time.Time, len(in.Steps)), reached: make([]bool, len(in.Steps))}
			visitors[visitor] = state
		}
		for i := len(in.Steps) - 1; i >= 0; i-- {
			if name != in.Steps[i] {
				continue
			}
			start := t
			if i > 0 {
				start = state.starts[i-1]
				if start.IsZero() || t.Sub(start) > window || t.Before(start) {
					continue
				}
			}
			state.starts[i] = start
			if !state.reached[i] {
				state.reached[i] = true
				counts[i]++
			}
		}
	}
	if e = rows.Err(); e != nil {
		return e
	}
	steps := []funnelStep{}
	for i, name := range in.Steps {
		ratio := 0.0
		if counts[0] > 0 {
			ratio = float64(counts[i]) / float64(counts[0])
		}
		steps = append(steps, funnelStep{Name: name, Visitors: counts[i], Conversion: ratio})
	}
	appkit.JSON(w, 200, map[string]any{"period": p, "steps": steps, "windowMinutes": in.WindowMinutes, "definition": "同站点、同访客，按服务器采集顺序完成事件；所有步骤位于所选日期内，末步距首步不超过转化窗口；每位访客每步最多计一次。同一时间戳按采集 ID 排序。"})
	return nil
}

// Maintain is called by Euler's lifecycle-owned maintenance runner, including
// for paused sites. Gather IDs before transactions (SQLite uses one writer).
func (m *Module) Maintain(ctx context.Context) error {
	rows, e := m.rt.DB.QueryContext(ctx, "SELECT s.id,COALESCE(c.retention_days,365) FROM statistics_sites s LEFT JOIN statistics_settings c ON c.site_id=s.id ORDER BY s.id")
	if e != nil {
		return e
	}
	type target struct {
		id   string
		days int
	}
	targets := []target{}
	for rows.Next() {
		var v target
		if e = rows.Scan(&v.id, &v.days); e != nil {
			rows.Close()
			return e
		}
		targets = append(targets, v)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for _, v := range targets {
		if e = ctx.Err(); e != nil {
			return e
		}
		if e = m.rt.Transaction(ctx, func(tx *sql.Tx) error { _, e := prune(ctx, tx, v.id, v.days, 10000); return e }); e != nil {
			return e
		}
	}
	return nil
}

// Legacy RFC3339Nano omits trailing zeros, so direct lexical comparison is
// ambiguous at exact boundaries. Normalize once, then keep every new timestamp
// fixed-width. This preserves nanoseconds and lets SQLite use the time indexes.
func (m *Module) normalizeTimestamps(ctx context.Context) error {
	return m.rt.Transaction(ctx, func(tx *sql.Tx) error {
		var exists int
		if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM statistics_migrations WHERE name='fixed-utc-nanoseconds-v1'").Scan(&exists); e != nil {
			return e
		}
		if exists > 0 {
			return nil
		}
		for _, table := range []string{"statistics_views", "statistics_events"} {
			_, e := tx.ExecContext(ctx, "UPDATE "+table+" SET visited_at=substr(visited_at,1,19)||'.'||CASE WHEN substr(visited_at,20,1)='.' THEN substr(substr(visited_at,21,length(visited_at)-21)||'000000000',1,9) ELSE '000000000' END||'Z'")
			if e != nil {
				return e
			}
		}
		_, e := tx.ExecContext(ctx, "INSERT INTO statistics_migrations(name) VALUES('fixed-utc-nanoseconds-v1')")
		return e
	})
}
