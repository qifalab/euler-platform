// Package lottery implements Euler-owned events and random draws.
// Functional reference: miaojilab/emoera-lottery-system; see SOURCE.md.
package lottery

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"strings"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
)

type Module struct{ rt *appkit.Runtime }

func New(rt *appkit.Runtime) *Module { return &Module{rt: rt} }
func (*Module) ID() string           { return "lottery" }
func (m *Module) Migrate(ctx context.Context) error {
	_, e := m.rt.DB.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS lottery_rooms (
 id TEXT PRIMARY KEY,tenant_id TEXT NOT NULL,project_id TEXT NOT NULL,installation_id TEXT NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
 name TEXT NOT NULL,description TEXT NOT NULL,status TEXT NOT NULL CHECK(status IN ('open','closed')),
 prevent_duplicates INTEGER NOT NULL DEFAULT 1,token_hash TEXT NOT NULL UNIQUE,token_cipher BLOB NOT NULL,
 generation INTEGER NOT NULL DEFAULT 1,next_round INTEGER NOT NULL DEFAULT 1,created_at TEXT NOT NULL,actor_id TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS lottery_rooms_scope ON lottery_rooms(tenant_id,project_id,installation_id);
CREATE TABLE IF NOT EXISTS lottery_participants (
 id TEXT PRIMARY KEY,room_id TEXT NOT NULL REFERENCES lottery_rooms(id) ON DELETE CASCADE,name TEXT NOT NULL,name_key TEXT NOT NULL,department TEXT NOT NULL,
 created_at TEXT NOT NULL,deleted_at TEXT,source TEXT NOT NULL,client_hash TEXT NOT NULL DEFAULT '',UNIQUE(room_id,name_key));
CREATE INDEX IF NOT EXISTS lottery_participants_room ON lottery_participants(room_id,deleted_at);
CREATE TABLE IF NOT EXISTS lottery_draws (
 id TEXT PRIMARY KEY,room_id TEXT NOT NULL REFERENCES lottery_rooms(id) ON DELETE CASCADE,generation INTEGER NOT NULL,round_number INTEGER NOT NULL,
 request_id TEXT NOT NULL,request_hash TEXT NOT NULL,prize_name TEXT NOT NULL,prevent_duplicates INTEGER NOT NULL,created_at TEXT NOT NULL,actor_id TEXT NOT NULL,
 UNIQUE(room_id,request_id),UNIQUE(room_id,generation,round_number));
CREATE TABLE IF NOT EXISTS lottery_winners (
 draw_id TEXT NOT NULL REFERENCES lottery_draws(id) ON DELETE CASCADE,participant_id TEXT NOT NULL REFERENCES lottery_participants(id),
 name TEXT NOT NULL,department TEXT NOT NULL,position INTEGER NOT NULL,PRIMARY KEY(draw_id,participant_id));
CREATE INDEX IF NOT EXISTS lottery_winners_participant ON lottery_winners(participant_id);
`)
	return e
}

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}
type room struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Description       string `json:"description"`
	Status            string `json:"status"`
	PreventDuplicates bool   `json:"preventDuplicates"`
	Generation        int    `json:"generation"`
	NextRound         int    `json:"nextRound"`
	CreatedAt         string `json:"createdAt"`
	TotalUsers        int    `json:"totalUsers"`
	CurrentWinners    int    `json:"currentWinners"`
	TotalRounds       int    `json:"totalRounds"`
}

const roomColumns = "r.id,r.name,r.description,r.status,r.prevent_duplicates,r.generation,r.next_round,r.created_at"

type scanner interface{ Scan(...any) error }

func scanRoom(row scanner) (room, error) {
	var v room
	e := row.Scan(&v.ID, &v.Name, &v.Description, &v.Status, &v.PreventDuplicates, &v.Generation, &v.NextRound, &v.CreatedAt)
	return v, e
}
func scopedRoom(ctx context.Context, q queryer, s appkit.Scope, id string) (room, error) {
	return scanRoom(q.QueryRowContext(ctx, "SELECT "+roomColumns+" FROM lottery_rooms r WHERE r.id=? AND r.tenant_id=? AND r.project_id=? AND r.installation_id=?", id, s.TenantID, s.ProjectID, s.InstallationID))
}
func roomCounts(ctx context.Context, q queryer, v *room) error {
	if e := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM lottery_participants WHERE room_id=? AND deleted_at IS NULL", v.ID).Scan(&v.TotalUsers); e != nil {
		return e
	}
	if e := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM lottery_winners w JOIN lottery_draws d ON d.id=w.draw_id WHERE d.room_id=? AND d.generation=?", v.ID, v.Generation).Scan(&v.CurrentWinners); e != nil {
		return e
	}
	return q.QueryRowContext(ctx, "SELECT COUNT(*) FROM lottery_draws WHERE room_id=? AND generation=?", v.ID, v.Generation).Scan(&v.TotalRounds)
}
func (m *Module) Handler() http.Handler {
	x := http.NewServeMux()
	appkit.Handle(x, "GET /rooms", "read", m.listRooms)
	appkit.Handle(x, "POST /rooms", "write", m.createRoom)
	appkit.Handle(x, "GET /rooms/{id}", "read", m.getRoom)
	appkit.Handle(x, "PATCH /rooms/{id}", "write", m.updateRoom)
	appkit.Handle(x, "GET /rooms/{id}/invitation", "write", m.invitation)
	appkit.Handle(x, "POST /rooms/{id}/invitation/rotate", "manage", m.rotateInvitation)
	appkit.Handle(x, "GET /rooms/{id}/participants", "read", m.participants)
	appkit.Handle(x, "POST /rooms/{id}/participants", "write", m.addParticipant)
	appkit.Handle(x, "POST /rooms/{id}/participants/batch", "write", m.batchParticipants)
	appkit.Handle(x, "DELETE /rooms/{id}/participants/{participantID}", "write", m.deleteParticipant)
	appkit.Handle(x, "POST /rooms/{id}/draws", "write", m.draw)
	appkit.Handle(x, "GET /rooms/{id}/draws", "read", m.history)
	appkit.Handle(x, "GET /history", "read", m.history)
	appkit.Handle(x, "POST /rooms/{id}/reset", "manage", m.reset)
	return x
}
func hash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

type roomInput struct {
	Name              string `json:"name"`
	Description       string `json:"description"`
	Status            string `json:"status"`
	PreventDuplicates *bool  `json:"preventDuplicates"`
}

func validateRoom(in *roomInput) error {
	var e error
	in.Name, e = appkit.Name(in.Name, 100)
	if e != nil {
		return e
	}
	in.Description = strings.TrimSpace(in.Description)
	if len([]rune(in.Description)) > 1000 {
		return appkit.Invalid("活动说明最多 1000 字")
	}
	if in.Status == "" {
		in.Status = "open"
	}
	if in.Status != "open" && in.Status != "closed" {
		return appkit.Invalid("房间状态无效")
	}
	return nil
}
func (m *Module) listRooms(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	rows, e := m.rt.DB.QueryContext(r.Context(), "SELECT "+roomColumns+" FROM lottery_rooms r WHERE r.tenant_id=? AND r.project_id=? AND r.installation_id=? ORDER BY r.created_at DESC", s.TenantID, s.ProjectID, s.InstallationID)
	if e != nil {
		return e
	}
	items := []room{}
	for rows.Next() {
		v, e := scanRoom(rows)
		if e != nil {
			rows.Close()
			return e
		}
		items = append(items, v)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for i := range items {
		if e = roomCounts(r.Context(), m.rt.DB, &items[i]); e != nil {
			return e
		}
	}
	appkit.JSON(w, 200, map[string]any{"rooms": items})
	return nil
}
func (m *Module) createRoom(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	var in roomInput
	if e := appkit.Decode(w, r, &in); e != nil {
		return e
	}
	if e := validateRoom(&in); e != nil {
		return e
	}
	v := room{ID: appkit.NewID("room_"), Name: in.Name, Description: in.Description, Status: in.Status, PreventDuplicates: true, Generation: 1, NextRound: 1, CreatedAt: appkit.Now()}
	if in.PreventDuplicates != nil {
		v.PreventDuplicates = *in.PreventDuplicates
	}
	token := appkit.NewID("join_")
	cipher, e := m.rt.Encrypt(token, "lottery/invitation/"+v.ID)
	if e != nil {
		return e
	}
	e = m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		_, e := tx.ExecContext(r.Context(), "INSERT INTO lottery_rooms VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)", v.ID, s.TenantID, s.ProjectID, s.InstallationID, v.Name, v.Description, v.Status, v.PreventDuplicates, hash(token), cipher, v.Generation, v.NextRound, v.CreatedAt, s.ActorID)
		if e != nil {
			return e
		}
		return m.rt.Audit(r.Context(), tx, s, "room.created", v.ID, "创建抽奖活动")
	})
	if e != nil {
		return e
	}
	appkit.JSON(w, 201, v)
	return nil
}
func (m *Module) getRoom(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	v, e := scopedRoom(r.Context(), m.rt.DB, s, r.PathValue("id"))
	if e != nil {
		return e
	}
	if e = roomCounts(r.Context(), m.rt.DB, &v); e != nil {
		return e
	}
	appkit.JSON(w, 200, v)
	return nil
}
func (m *Module) updateRoom(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	var in roomInput
	if e := appkit.Decode(w, r, &in); e != nil {
		return e
	}
	if e := validateRoom(&in); e != nil {
		return e
	}
	var v room
	e := m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		var e error
		v, e = scopedRoom(r.Context(), tx, s, r.PathValue("id"))
		if e != nil {
			return e
		}
		v.Name = in.Name
		v.Description = in.Description
		v.Status = in.Status
		if in.PreventDuplicates != nil {
			v.PreventDuplicates = *in.PreventDuplicates
		}
		_, e = tx.ExecContext(r.Context(), "UPDATE lottery_rooms SET name=?,description=?,status=?,prevent_duplicates=? WHERE id=?", v.Name, v.Description, v.Status, v.PreventDuplicates, v.ID)
		if e != nil {
			return e
		}
		return m.rt.Audit(r.Context(), tx, s, "room.updated", v.ID, "更新活动设置")
	})
	if e != nil {
		return e
	}
	appkit.JSON(w, 200, v)
	return nil
}
func (m *Module) invitation(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	v, e := scopedRoom(r.Context(), m.rt.DB, s, r.PathValue("id"))
	if e != nil {
		return e
	}
	var cipher []byte
	if e = m.rt.DB.QueryRowContext(r.Context(), "SELECT token_cipher FROM lottery_rooms WHERE id=?", v.ID).Scan(&cipher); e != nil {
		return e
	}
	token, e := m.rt.Decrypt(cipher, "lottery/invitation/"+v.ID)
	if e != nil {
		return e
	}
	base := strings.TrimRight(m.rt.PublicURL, "/") + "/public/lottery/join/" + token
	appkit.JSON(w, 200, map[string]string{"signupURL": base, "qrURL": base + "/qr.png"})
	return nil
}
func (m *Module) rotateInvitation(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	e := m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		v, e := scopedRoom(r.Context(), tx, s, r.PathValue("id"))
		if e != nil {
			return e
		}
		token := appkit.NewID("join_")
		cipher, e := m.rt.Encrypt(token, "lottery/invitation/"+v.ID)
		if e != nil {
			return e
		}
		_, e = tx.ExecContext(r.Context(), "UPDATE lottery_rooms SET token_hash=?,token_cipher=? WHERE id=?", hash(token), cipher, v.ID)
		if e != nil {
			return e
		}
		return m.rt.Audit(r.Context(), tx, s, "invitation.rotated", v.ID, "撤销旧报名链接并生成新链接")
	})
	if e != nil {
		return e
	}
	return m.invitation(w, r, s)
}

type participant struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Department   string `json:"department"`
	CreatedAt    string `json:"createdAt"`
	Participated bool   `json:"participated"`
}

func paging(r *http.Request) (int, int) {
	parse := func(s string, d, max int) int {
		n, e := strconv.Atoi(s)
		if e != nil || n < 1 {
			return d
		}
		if n > max {
			return max
		}
		return n
	}
	return parse(r.URL.Query().Get("page"), 1, 1000000), parse(r.URL.Query().Get("pageSize"), 20, 100)
}
func (m *Module) participants(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	v, e := scopedRoom(r.Context(), m.rt.DB, s, r.PathValue("id"))
	if e != nil {
		return e
	}
	page, size := paging(r)
	search := strings.TrimSpace(r.URL.Query().Get("search"))
	if len(search) > 200 {
		return appkit.Invalid("搜索文本过长")
	}
	where := "p.room_id=? AND p.deleted_at IS NULL AND (instr(lower(p.name),lower(?))>0 OR instr(lower(p.department),lower(?))>0)"
	args := []any{v.ID, search, search}
	var total, available int
	if e = m.rt.DB.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM lottery_participants p WHERE "+where, args...).Scan(&total); e != nil {
		return e
	}
	if e = m.rt.DB.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM lottery_participants p WHERE p.room_id=? AND p.deleted_at IS NULL AND NOT EXISTS(SELECT 1 FROM lottery_winners w JOIN lottery_draws d ON d.id=w.draw_id WHERE w.participant_id=p.id AND d.generation=?)", v.ID, v.Generation).Scan(&available); e != nil {
		return e
	}
	queryArgs := append([]any{v.Generation}, args...)
	queryArgs = append(queryArgs, size, (page-1)*size)
	rows, e := m.rt.DB.QueryContext(r.Context(), "SELECT p.id,p.name,p.department,p.created_at,EXISTS(SELECT 1 FROM lottery_winners w JOIN lottery_draws d ON d.id=w.draw_id WHERE w.participant_id=p.id AND d.generation=?) FROM lottery_participants p WHERE "+where+" ORDER BY p.created_at,p.id LIMIT ? OFFSET ?", queryArgs...)
	if e != nil {
		return e
	}
	defer rows.Close()
	items := []participant{}
	for rows.Next() {
		var p participant
		if e = rows.Scan(&p.ID, &p.Name, &p.Department, &p.CreatedAt, &p.Participated); e != nil {
			return e
		}
		items = append(items, p)
	}
	if e = rows.Err(); e != nil {
		return e
	}
	appkit.JSON(w, 200, map[string]any{"participants": items, "total": total, "available": available, "page": page, "pageSize": size})
	return nil
}

type participantInput struct {
	Name       string `json:"name"`
	Department string `json:"department"`
}

func validateParticipant(in *participantInput, public bool) error {
	var e error
	max := 100
	if public {
		max = 20
	}
	in.Name, e = appkit.Name(in.Name, max)
	if e != nil {
		return e
	}
	if public && len([]rune(in.Name)) < 2 {
		return appkit.Invalid("姓名至少需要 2 个字符")
	}
	in.Department = strings.TrimSpace(in.Department)
	if len([]rune(in.Department)) > 50 {
		return appkit.Invalid("部门最多 50 字")
	}
	return nil
}
func insertParticipant(ctx context.Context, tx *sql.Tx, roomID string, in participantInput, source, clientHash string) (participant, error) {
	var count int
	if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM lottery_participants WHERE room_id=?", roomID).Scan(&count); e != nil {
		return participant{}, e
	}
	if count >= 10000 {
		return participant{}, appkit.Conflict("此房间已达到 10000 人上限")
	}
	var existing string
	e := tx.QueryRowContext(ctx, "SELECT id FROM lottery_participants WHERE room_id=? AND name_key=?", roomID, strings.ToLower(in.Name)).Scan(&existing)
	if e == nil {
		return participant{}, appkit.Conflict("此姓名已报名，或已从名单移除；请使用可区分的姓名")
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return participant{}, e
	}
	p := participant{ID: appkit.NewID("person_"), Name: in.Name, Department: in.Department, CreatedAt: appkit.Now()}
	_, e = tx.ExecContext(ctx, "INSERT INTO lottery_participants(id,room_id,name,name_key,department,created_at,source,client_hash) VALUES(?,?,?,?,?,?,?,?)", p.ID, roomID, p.Name, strings.ToLower(p.Name), p.Department, p.CreatedAt, source, clientHash)
	return p, e
}
func (m *Module) addParticipant(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	var in participantInput
	if e := appkit.Decode(w, r, &in); e != nil {
		return e
	}
	if e := validateParticipant(&in, false); e != nil {
		return e
	}
	var p participant
	e := m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		v, e := scopedRoom(r.Context(), tx, s, r.PathValue("id"))
		if e != nil {
			return e
		}
		p, e = insertParticipant(r.Context(), tx, v.ID, in, "manual", "")
		if e != nil {
			return e
		}
		return m.rt.Audit(r.Context(), tx, s, "participant.added", p.ID, "手动添加参与者")
	})
	if e != nil {
		return e
	}
	appkit.JSON(w, 201, p)
	return nil
}
func (m *Module) batchParticipants(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	var in struct {
		Count     int `json:"count"`
		StartFrom int `json:"startFrom"`
	}
	if e := appkit.Decode(w, r, &in); e != nil {
		return e
	}
	if in.StartFrom == 0 {
		in.StartFrom = 1
	}
	if in.Count < 1 || in.Count > 1000 || in.StartFrom < 1 || in.StartFrom > 10000000 {
		return appkit.Invalid("单次生成 1–1000 人，起始号码为 1–10000000")
	}
	added, skipped := 0, 0
	e := m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		v, e := scopedRoom(r.Context(), tx, s, r.PathValue("id"))
		if e != nil {
			return e
		}
		for i := 0; i < in.Count; i++ {
			inUser := participantInput{Name: fmt.Sprintf("用户%d", in.StartFrom+i)}
			var count int
			if e = tx.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM lottery_participants WHERE room_id=? AND name_key=?", v.ID, strings.ToLower(inUser.Name)).Scan(&count); e != nil {
				return e
			}
			if count > 0 {
				skipped++
				continue
			}
			if _, e = insertParticipant(r.Context(), tx, v.ID, inUser, "batch", ""); e != nil {
				return e
			}
			added++
		}
		return m.rt.Audit(r.Context(), tx, s, "participants.generated", v.ID, "批量生成序号参与者")
	})
	if e != nil {
		return e
	}
	appkit.JSON(w, 201, map[string]int{"addedCount": added, "skippedCount": skipped})
	return nil
}
func (m *Module) deleteParticipant(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	e := m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		v, e := scopedRoom(r.Context(), tx, s, r.PathValue("id"))
		if e != nil {
			return e
		}
		res, e := tx.ExecContext(r.Context(), "UPDATE lottery_participants SET deleted_at=? WHERE id=? AND room_id=? AND deleted_at IS NULL", appkit.Now(), r.PathValue("participantID"), v.ID)
		if e != nil {
			return e
		}
		n, e := res.RowsAffected()
		if e != nil {
			return e
		}
		if n == 0 {
			return appkit.NotFound()
		}
		return m.rt.Audit(r.Context(), tx, s, "participant.removed", r.PathValue("participantID"), "移除参与者，保留已有中奖记录")
	})
	if e != nil {
		return e
	}
	appkit.JSON(w, 200, map[string]bool{"success": true})
	return nil
}

type winner struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Department string `json:"department"`
}
type drawResult struct {
	ID                string   `json:"id"`
	RoomID            string   `json:"roomId"`
	RoomName          string   `json:"roomName"`
	RoundNumber       int      `json:"roundNumber"`
	PrizeName         string   `json:"prizeName"`
	PreventDuplicates bool     `json:"preventDuplicates"`
	CreatedAt         string   `json:"createdAt"`
	Winners           []winner `json:"winners"`
}

func readWinners(ctx context.Context, q queryer, id string) ([]winner, error) {
	rows, e := q.QueryContext(ctx, "SELECT participant_id,name,department FROM lottery_winners WHERE draw_id=? ORDER BY position", id)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []winner{}
	for rows.Next() {
		var w winner
		if e = rows.Scan(&w.ID, &w.Name, &w.Department); e != nil {
			return nil, e
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
func (m *Module) draw(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	var in struct {
		Count             int    `json:"count"`
		PrizeName         string `json:"prizeName"`
		PreventDuplicates *bool  `json:"preventDuplicates"`
		RequestID         string `json:"requestId"`
	}
	if e := appkit.Decode(w, r, &in); e != nil {
		return e
	}
	in.PrizeName = strings.TrimSpace(in.PrizeName)
	if in.Count < 1 || in.Count > 1000 || len([]rune(in.PrizeName)) > 100 || len(in.RequestID) < 8 || len(in.RequestID) > 100 {
		return appkit.Invalid("抽取人数须为 1–1000；须提供 8–100 字符幂等请求 ID，奖品名最多 100 字")
	}
	encoded, _ := json.Marshal(in)
	requestHash := hash(string(encoded))
	var result drawResult
	e := m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		v, e := scopedRoom(r.Context(), tx, s, r.PathValue("id"))
		if e != nil {
			return e
		}
		var oldHash string
		var generation int
		e = tx.QueryRowContext(r.Context(), "SELECT id,round_number,prize_name,prevent_duplicates,created_at,request_hash,generation FROM lottery_draws WHERE room_id=? AND request_id=?", v.ID, in.RequestID).Scan(&result.ID, &result.RoundNumber, &result.PrizeName, &result.PreventDuplicates, &result.CreatedAt, &oldHash, &generation)
		if e == nil {
			if oldHash != requestHash || generation != v.Generation {
				return appkit.Conflict("请求 ID 已使用，请为新的抽奖生成新 ID")
			}
			result.RoomID = v.ID
			result.RoomName = v.Name
			result.Winners, e = readWinners(r.Context(), tx, result.ID)
			return e
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if v.Status != "open" {
			return appkit.Conflict("房间已关闭，请先重新开启")
		}
		prevent := v.PreventDuplicates
		if in.PreventDuplicates != nil {
			prevent = *in.PreventDuplicates
		}
		query := "SELECT p.id,p.name,p.department FROM lottery_participants p WHERE p.room_id=? AND p.deleted_at IS NULL"
		args := []any{v.ID}
		if prevent {
			query += " AND NOT EXISTS(SELECT 1 FROM lottery_winners w JOIN lottery_draws d ON d.id=w.draw_id WHERE w.participant_id=p.id AND d.generation=?)"
			args = append(args, v.Generation)
		}
		query += " ORDER BY p.id"
		rows, e := tx.QueryContext(r.Context(), query, args...)
		if e != nil {
			return e
		}
		eligible := []winner{}
		for rows.Next() {
			var p winner
			if e = rows.Scan(&p.ID, &p.Name, &p.Department); e != nil {
				rows.Close()
				return e
			}
			eligible = append(eligible, p)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		if len(eligible) < in.Count {
			return appkit.Conflict(fmt.Sprintf("当前仅有 %d 人符合抽奖条件", len(eligible)))
		}
		// Partial Fisher–Yates with crypto/rand is unbiased and samples without replacement.
		for i := 0; i < in.Count; i++ {
			n, e := rand.Int(rand.Reader, big.NewInt(int64(len(eligible)-i)))
			if e != nil {
				return e
			}
			j := i + int(n.Int64())
			eligible[i], eligible[j] = eligible[j], eligible[i]
		}
		result = drawResult{ID: appkit.NewID("draw_"), RoomID: v.ID, RoomName: v.Name, RoundNumber: v.NextRound, PrizeName: in.PrizeName, PreventDuplicates: prevent, CreatedAt: appkit.Now(), Winners: eligible[:in.Count]}
		_, e = tx.ExecContext(r.Context(), "INSERT INTO lottery_draws VALUES(?,?,?,?,?,?,?,?,?,?)", result.ID, v.ID, v.Generation, result.RoundNumber, in.RequestID, requestHash, result.PrizeName, prevent, result.CreatedAt, s.ActorID)
		if e != nil {
			return e
		}
		for i, p := range result.Winners {
			if _, e = tx.ExecContext(r.Context(), "INSERT INTO lottery_winners VALUES(?,?,?,?,?)", result.ID, p.ID, p.Name, p.Department, i); e != nil {
				return e
			}
		}
		if _, e = tx.ExecContext(r.Context(), "UPDATE lottery_rooms SET next_round=next_round+1,prevent_duplicates=? WHERE id=?", prevent, v.ID); e != nil {
			return e
		}
		return m.rt.Audit(r.Context(), tx, s, "draw.completed", result.ID, "完成一次随机抽奖")
	})
	if e != nil {
		return e
	}
	appkit.JSON(w, 200, result)
	return nil
}
func (m *Module) history(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	roomID := r.PathValue("id")
	if roomID == "" {
		roomID = r.URL.Query().Get("roomId")
	}
	if roomID != "" {
		if _, e := scopedRoom(r.Context(), m.rt.DB, s, roomID); e != nil {
			return e
		}
	}
	page, size := paging(r)
	search := strings.TrimSpace(r.URL.Query().Get("search"))
	if len(search) > 200 {
		return appkit.Invalid("搜索文本过长")
	}
	where := "r.tenant_id=? AND r.project_id=? AND r.installation_id=? AND d.generation=r.generation"
	args := []any{s.TenantID, s.ProjectID, s.InstallationID}
	if roomID != "" {
		where += " AND r.id=?"
		args = append(args, roomID)
	}
	if search != "" {
		where += " AND (instr(lower(r.name),lower(?))>0 OR instr(lower(d.prize_name),lower(?))>0 OR EXISTS(SELECT 1 FROM lottery_winners w WHERE w.draw_id=d.id AND (instr(lower(w.name),lower(?))>0 OR instr(lower(w.department),lower(?))>0)))"
		args = append(args, search, search, search, search)
	}
	var total int
	if e := m.rt.DB.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM lottery_draws d JOIN lottery_rooms r ON r.id=d.room_id WHERE "+where, args...).Scan(&total); e != nil {
		return e
	}
	args = append(args, size, (page-1)*size)
	rows, e := m.rt.DB.QueryContext(r.Context(), "SELECT d.id,r.id,r.name,d.round_number,d.prize_name,d.prevent_duplicates,d.created_at FROM lottery_draws d JOIN lottery_rooms r ON r.id=d.room_id WHERE "+where+" ORDER BY d.created_at DESC,d.id LIMIT ? OFFSET ?", args...)
	if e != nil {
		return e
	}
	items := []drawResult{}
	for rows.Next() {
		var d drawResult
		if e = rows.Scan(&d.ID, &d.RoomID, &d.RoomName, &d.RoundNumber, &d.PrizeName, &d.PreventDuplicates, &d.CreatedAt); e != nil {
			rows.Close()
			return e
		}
		items = append(items, d)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for i := range items {
		items[i].Winners, e = readWinners(r.Context(), m.rt.DB, items[i].ID)
		if e != nil {
			return e
		}
	}
	appkit.JSON(w, 200, map[string]any{"draws": items, "total": total, "page": page, "pageSize": size})
	return nil
}
func (m *Module) reset(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	var in struct {
		Confirm string `json:"confirm"`
	}
	if e := appkit.Decode(w, r, &in); e != nil {
		return e
	}
	if in.Confirm != r.PathValue("id") {
		return appkit.Invalid("请确认当前房间 ID")
	}
	e := m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		v, e := scopedRoom(r.Context(), tx, s, r.PathValue("id"))
		if e != nil {
			return e
		}
		_, e = tx.ExecContext(r.Context(), "UPDATE lottery_rooms SET generation=generation+1,next_round=1 WHERE id=?", v.ID)
		if e != nil {
			return e
		}
		return m.rt.Audit(r.Context(), tx, s, "room.reset", v.ID, "开启新抽奖周期，保留参与者与旧周期审计记录")
	})
	if e != nil {
		return e
	}
	appkit.JSON(w, 200, map[string]bool{"success": true})
	return nil
}
