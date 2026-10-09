// 资源 CRUD 路由（person/expression/jargon/emoji）— 对齐各 Python router 的
// 简化面：只读列表 + 删除 + 禁用/审核标记。数据走 internal/database 直查。
package routers

import (
	"database/sql"
	"net/http"
	"strconv"
)

// ---------------------------------------------------------------------------
// person — 对齐 routers/person.py
// ---------------------------------------------------------------------------

type personItem struct {
	PersonID     string  `json:"person_id"`
	PersonName   string  `json:"person_name,omitempty"`
	UserNickname string  `json:"user_nickname,omitempty"`
	Platform     string  `json:"platform,omitempty"`
	UserID       string  `json:"user_id,omitempty"`
	KnowTimes    int     `json:"know_times"`
	MemoryPoints int     `json:"memory_points"`
	IsKnown      bool    `json:"is_known"`
	LastKnown    *string `json:"last_known_time,omitempty"`
}

// handlePersonList 对齐 GET /person/list。
func handlePersonList(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.DB == nil {
			fail(w, http.StatusServiceUnavailable, "数据库未启用")
			return
		}
		limit := queryInt(r, "limit", 100)
		offset := queryInt(r, "offset", 0)
		rows, err := d.DB.Read().Query(`
			SELECT person_id, person_name, user_nickname, platform, user_id,
				know_counts, memory_points, is_known, last_known_time
			FROM person_info ORDER BY know_counts DESC, id LIMIT ? OFFSET ?`, limit, offset)
		if err != nil {
			fail(w, http.StatusInternalServerError, "查询人物失败: "+err.Error())
			return
		}
		defer rows.Close()
		items := []personItem{}
		for rows.Next() {
			var p personItem
			var name, nickname sql.NullString
			var memory sql.NullString
			var lastKnown sql.NullString
			if err := rows.Scan(&p.PersonID, &name, &nickname, &p.Platform, &p.UserID,
				&p.KnowTimes, &memory, &p.IsKnown, &lastKnown); err != nil {
				continue
			}
			p.PersonName = name.String
			p.UserNickname = nickname.String
			p.MemoryPoints = countJSONList(memory.String)
			if lastKnown.Valid {
				p.LastKnown = &lastKnown.String
			}
			items = append(items, p)
		}
		ok(w, map[string]any{"items": items, "count": len(items)})
	}
}

// handlePersonDelete 对齐 DELETE /person/{person_id}。
func handlePersonDelete(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.DB == nil {
			fail(w, http.StatusServiceUnavailable, "数据库未启用")
			return
		}
		personID := pathValue(r, "person_id")
		res, err := d.DB.Write().Exec(`DELETE FROM person_info WHERE person_id = ?`, personID)
		if err != nil {
			fail(w, http.StatusInternalServerError, "删除人物失败: "+err.Error())
			return
		}
		affected, _ := res.RowsAffected()
		if affected == 0 {
			fail(w, http.StatusNotFound, "人物不存在: "+personID)
			return
		}
		ok(w, map[string]any{"message": "人物已删除", "person_id": personID})
	}
}

// handlePersonStats 对齐 GET /person/stats/summary。
func handlePersonStats(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.DB == nil {
			fail(w, http.StatusServiceUnavailable, "数据库未启用")
			return
		}
		var total, known int64
		d.DB.Read().QueryRow(`SELECT COUNT(*), COALESCE(SUM(is_known),0) FROM person_info`).Scan(&total, &known)
		ok(w, map[string]any{"total": total, "known": known})
	}
}

// ---------------------------------------------------------------------------
// expression — 对齐 routers/expression.py（简化：列表/删除/审核标记）
// ---------------------------------------------------------------------------

type expressionItem struct {
	ID         int64  `json:"id"`
	Situation  string `json:"situation"`
	Style      string `json:"style"`
	Count      int    `json:"count"`
	SessionID  string `json:"session_id,omitempty"`
	Checked    bool   `json:"checked"`
	ModifiedBy string `json:"modified_by,omitempty"`
}

// handleExpressionList 对齐 GET /expression/list。
func handleExpressionList(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.DB == nil {
			fail(w, http.StatusServiceUnavailable, "数据库未启用")
			return
		}
		limit := queryInt(r, "limit", 100)
		offset := queryInt(r, "offset", 0)
		checkedOnly := r.URL.Query().Get("checked_only") == "true"
		query := `SELECT id, situation, style, count, session_id, checked, modified_by
			FROM expressions`
		if checkedOnly {
			query += ` WHERE checked = 1`
		}
		query += ` ORDER BY count DESC, id DESC LIMIT ? OFFSET ?`
		rows, err := d.DB.Read().Query(query, limit, offset)
		if err != nil {
			fail(w, http.StatusInternalServerError, "查询表达失败: "+err.Error())
			return
		}
		defer rows.Close()
		items := []expressionItem{}
		for rows.Next() {
			var e expressionItem
			var session, modified sql.NullString
			if err := rows.Scan(&e.ID, &e.Situation, &e.Style, &e.Count, &session, &e.Checked, &modified); err != nil {
				continue
			}
			e.SessionID = session.String
			e.ModifiedBy = modified.String
			items = append(items, e)
		}
		ok(w, map[string]any{"items": items, "count": len(items)})
	}
}

// handleExpressionDelete 对齐 DELETE /expression/{expression_id}。
func handleExpressionDelete(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.DB == nil {
			fail(w, http.StatusServiceUnavailable, "数据库未启用")
			return
		}
		id, err := strconv.ParseInt(pathValue(r, "expression_id"), 10, 64)
		if err != nil {
			fail(w, http.StatusBadRequest, "表达 ID 无效")
			return
		}
		res, err := d.DB.Write().Exec(`DELETE FROM expressions WHERE id = ?`, id)
		if err != nil {
			fail(w, http.StatusInternalServerError, "删除表达失败: "+err.Error())
			return
		}
		affected, _ := res.RowsAffected()
		if affected == 0 {
			fail(w, http.StatusNotFound, "表达不存在")
			return
		}
		ok(w, map[string]any{"message": "表达已删除"})
	}
}

// handleExpressionCheck 对齐人工审核标记（checked + modified_by=USER）。
func handleExpressionCheck(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.DB == nil {
			fail(w, http.StatusServiceUnavailable, "数据库未启用")
			return
		}
		id, err := strconv.ParseInt(pathValue(r, "expression_id"), 10, 64)
		if err != nil {
			fail(w, http.StatusBadRequest, "表达 ID 无效")
			return
		}
		var body struct {
			Checked *bool `json:"checked"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		checked := true
		if body.Checked != nil {
			checked = *body.Checked
		}
		checkedInt := 0
		if checked {
			checkedInt = 1
		}
		res, err := d.DB.Write().Exec(
			`UPDATE expressions SET checked = ?, modified_by = 'USER' WHERE id = ?`, checkedInt, id)
		if err != nil {
			fail(w, http.StatusInternalServerError, "更新表达审核状态失败: "+err.Error())
			return
		}
		affected, _ := res.RowsAffected()
		if affected == 0 {
			fail(w, http.StatusNotFound, "表达不存在")
			return
		}
		ok(w, map[string]any{"message": "审核状态已更新"})
	}
}

// ---------------------------------------------------------------------------
// jargon — 对齐 routers/jargon.py（简化：列表/删除/更新）
// ---------------------------------------------------------------------------

type jargonItem struct {
	ID        int64  `json:"id"`
	Content   string `json:"content"`
	Meaning   string `json:"meaning"`
	Count     int    `json:"count"`
	IsJargon  bool   `json:"is_jargon"`
	IsGlobal  bool   `json:"is_global"`
	CreatedBy string `json:"created_by,omitempty"`
}

// handleJargonList 对齐 GET /jargon/list。
func handleJargonList(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.DB == nil {
			fail(w, http.StatusServiceUnavailable, "数据库未启用")
			return
		}
		limit := queryInt(r, "limit", 100)
		offset := queryInt(r, "offset", 0)
		rows, err := d.DB.Read().Query(`
			SELECT id, content, meaning, count, is_jargon, is_global, created_by
			FROM jargons ORDER BY count DESC, id DESC LIMIT ? OFFSET ?`, limit, offset)
		if err != nil {
			fail(w, http.StatusInternalServerError, "查询黑话失败: "+err.Error())
			return
		}
		defer rows.Close()
		items := []jargonItem{}
		for rows.Next() {
			var j jargonItem
			var meaning, createdBy sql.NullString
			if err := rows.Scan(&j.ID, &j.Content, &meaning, &j.Count, &j.IsJargon, &j.IsGlobal, &createdBy); err != nil {
				continue
			}
			j.Meaning = meaning.String
			j.CreatedBy = createdBy.String
			items = append(items, j)
		}
		ok(w, map[string]any{"items": items, "count": len(items)})
	}
}

// handleJargonDelete 对齐 DELETE /jargon/{jargon_id}。
func handleJargonDelete(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.DB == nil {
			fail(w, http.StatusServiceUnavailable, "数据库未启用")
			return
		}
		id, err := strconv.ParseInt(pathValue(r, "jargon_id"), 10, 64)
		if err != nil {
			fail(w, http.StatusBadRequest, "黑话 ID 无效")
			return
		}
		res, err := d.DB.Write().Exec(`DELETE FROM jargons WHERE id = ?`, id)
		if err != nil {
			fail(w, http.StatusInternalServerError, "删除黑话失败: "+err.Error())
			return
		}
		affected, _ := res.RowsAffected()
		if affected == 0 {
			fail(w, http.StatusNotFound, "黑话不存在")
			return
		}
		ok(w, map[string]any{"message": "黑话已删除"})
	}
}

// handleJargonUpdate 对齐黑话人工确认（is_jargon/meaning，created_by=MANUAL）。
func handleJargonUpdate(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.DB == nil {
			fail(w, http.StatusServiceUnavailable, "数据库未启用")
			return
		}
		id, err := strconv.ParseInt(pathValue(r, "jargon_id"), 10, 64)
		if err != nil {
			fail(w, http.StatusBadRequest, "黑话 ID 无效")
			return
		}
		var body struct {
			IsJargon *bool   `json:"is_jargon"`
			Meaning  *string `json:"meaning"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		if body.IsJargon == nil && body.Meaning == nil {
			fail(w, http.StatusBadRequest, "请求体为空（支持 is_jargon/meaning 字段）")
			return
		}
		if body.IsJargon != nil {
			v := 0
			if *body.IsJargon {
				v = 1
			}
			if _, err := d.DB.Write().Exec(
				`UPDATE jargons SET is_jargon = ?, created_by = 'MANUAL' WHERE id = ?`, v, id); err != nil {
				fail(w, http.StatusInternalServerError, "更新黑话失败: "+err.Error())
				return
			}
		}
		if body.Meaning != nil {
			if _, err := d.DB.Write().Exec(
				`UPDATE jargons SET meaning = ?, created_by = 'MANUAL' WHERE id = ?`, *body.Meaning, id); err != nil {
				fail(w, http.StatusInternalServerError, "更新黑话失败: "+err.Error())
				return
			}
		}
		ok(w, map[string]any{"message": "黑话已更新"})
	}
}

// ---------------------------------------------------------------------------
// emoji — 对齐 routers/emoji/routes.py（images 表 emoji 面）
// ---------------------------------------------------------------------------

type emojiItem struct {
	Hash         string  `json:"hash"`
	Description  string  `json:"description,omitempty"`
	FullPath     string  `json:"full_path,omitempty"`
	QueryCount   int     `json:"query_count"`
	IsRegistered bool    `json:"is_registered"`
	IsBanned     bool    `json:"is_banned"`
	NoFile       bool    `json:"no_file"`
	VLMProcessed bool    `json:"vlm_processed"`
	RegisterTime *string `json:"register_time,omitempty"`
}

// handleEmojiList 对齐 GET /emoji/list。
func handleEmojiList(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.DB == nil {
			fail(w, http.StatusServiceUnavailable, "数据库未启用")
			return
		}
		limit := queryInt(r, "limit", 100)
		offset := queryInt(r, "offset", 0)
		rows, err := d.DB.Read().Query(`
			SELECT image_hash, description, full_path, query_count, is_registered,
				is_banned, no_file_flag, vlm_processed, register_time
			FROM images WHERE image_type = 'emoji'
			ORDER BY register_time DESC, id DESC LIMIT ? OFFSET ?`, limit, offset)
		if err != nil {
			fail(w, http.StatusInternalServerError, "查询表情包失败: "+err.Error())
			return
		}
		defer rows.Close()
		items := []emojiItem{}
		for rows.Next() {
			var e emojiItem
			var desc, path, regTime sql.NullString
			if err := rows.Scan(&e.Hash, &desc, &path, &e.QueryCount, &e.IsRegistered,
				&e.IsBanned, &e.NoFile, &e.VLMProcessed, &regTime); err != nil {
				continue
			}
			e.Description = desc.String
			e.FullPath = path.String
			if regTime.Valid {
				e.RegisterTime = &regTime.String
			}
			items = append(items, e)
		}
		ok(w, map[string]any{"items": items, "count": len(items)})
	}
}

// handleEmojiDetail 对齐 GET /emoji/{emoji_id}。
func handleEmojiDetail(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.DB == nil {
			fail(w, http.StatusServiceUnavailable, "数据库未启用")
			return
		}
		hash := pathValue(r, "emoji_id")
		row := d.DB.Read().QueryRow(`
			SELECT image_hash, description, full_path, query_count, is_registered,
				is_banned, no_file_flag, vlm_processed, register_time
			FROM images WHERE image_hash = ? AND image_type = 'emoji' ORDER BY id LIMIT 1`, hash)
		var e emojiItem
		var desc, path, regTime sql.NullString
		err := row.Scan(&e.Hash, &desc, &path, &e.QueryCount, &e.IsRegistered,
			&e.IsBanned, &e.NoFile, &e.VLMProcessed, &regTime)
		if err == sql.ErrNoRows {
			fail(w, http.StatusNotFound, "表情包不存在: "+hash)
			return
		}
		if err != nil {
			fail(w, http.StatusInternalServerError, "查询表情包失败: "+err.Error())
			return
		}
		e.Description = desc.String
		e.FullPath = path.String
		if regTime.Valid {
			e.RegisterTime = &regTime.String
		}
		ok(w, map[string]any{"emoji": e})
	}
}

// handleEmojiDelete 对齐 DELETE /emoji/{emoji_id}（记录 + 磁盘文件语义由
// EmojiManager 后台清理处理，此处只删记录）。
func handleEmojiDelete(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.DB == nil {
			fail(w, http.StatusServiceUnavailable, "数据库未启用")
			return
		}
		hash := pathValue(r, "emoji_id")
		res, err := d.DB.Write().Exec(
			`DELETE FROM images WHERE image_hash = ? AND image_type = 'emoji'`, hash)
		if err != nil {
			fail(w, http.StatusInternalServerError, "删除表情包失败: "+err.Error())
			return
		}
		affected, _ := res.RowsAffected()
		if affected == 0 {
			fail(w, http.StatusNotFound, "表情包不存在: "+hash)
			return
		}
		ok(w, map[string]any{"message": "表情包已删除"})
	}
}

// handleEmojiBan 对齐表情禁用/恢复（is_banned 标记）。
func handleEmojiBan(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.DB == nil {
			fail(w, http.StatusServiceUnavailable, "数据库未启用")
			return
		}
		hash := pathValue(r, "emoji_id")
		var body struct {
			Banned *bool `json:"banned"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		banned := true
		if body.Banned != nil {
			banned = *body.Banned
		}
		v := 0
		if banned {
			v = 1
		}
		res, err := d.DB.Write().Exec(
			`UPDATE images SET is_banned = ? WHERE image_hash = ? AND image_type = 'emoji'`, v, hash)
		if err != nil {
			fail(w, http.StatusInternalServerError, "更新表情包状态失败: "+err.Error())
			return
		}
		affected, _ := res.RowsAffected()
		if affected == 0 {
			fail(w, http.StatusNotFound, "表情包不存在: "+hash)
			return
		}
		ok(w, map[string]any{"message": "表情包状态已更新"})
	}
}

// countJSONList 粗略统计 JSON 数组元素个数（memory_points/group_cardname 列表）。
func countJSONList(raw string) int {
	if raw == "" {
		return 0
	}
	count := 0
	depth := 0
	for _, ch := range raw {
		switch ch {
		case '[':
			depth++
		case ']':
			if depth > 0 {
				depth--
				if depth == 0 && count == 0 {
					count = 1
				}
			}
		case ',':
			if depth == 1 {
				count++
			}
		}
	}
	return count
}
