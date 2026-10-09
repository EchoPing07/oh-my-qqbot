// 记忆路由 — 对齐 routers/memory.py 的简化 admin 面：search / profiles。
package routers

import (
	"net/http"
	"strings"

	contracts "oh-my-qqbot/internal"
)

// handleMemorySearch 对齐记忆检索（query + chat_id + mode + top_k）。
func handleMemorySearch(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.Memory == nil {
			fail(w, http.StatusServiceUnavailable, "记忆内核未启用")
			return
		}
		var body struct {
			Query  string `json:"query"`
			ChatID string `json:"chat_id"`
			Mode   string `json:"mode"`
			TopK   int    `json:"top_k"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		if strings.TrimSpace(body.Query) == "" {
			fail(w, http.StatusBadRequest, "query 不能为空")
			return
		}
		mode := body.Mode
		if mode == "" {
			mode = "search"
		}
		topK := body.TopK
		if topK <= 0 {
			topK = 5
		}
		hits, err := d.Memory.SearchMemory(r.Context(), mode, body.Query, body.ChatID, topK)
		if err != nil {
			fail(w, http.StatusInternalServerError, "记忆检索失败: "+err.Error())
			return
		}
		if hits == nil {
			hits = []contracts.MemoryHit{}
		}
		ok(w, map[string]any{"hits": hits, "mode": mode, "count": len(hits)})
	}
}

// handleMemoryProfiles 对齐 GET /memory/profiles：画像列表（从 person_info 表
// 取已认识人物；画像详情经内核 GetPersonProfile）。
func handleMemoryProfiles(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.Memory == nil {
			fail(w, http.StatusServiceUnavailable, "记忆内核未启用")
			return
		}
		if d.DB == nil {
			fail(w, http.StatusServiceUnavailable, "数据库未启用")
			return
		}
		limit := queryInt(r, "limit", 50)
		rows, err := d.DB.Read().Query(`
			SELECT person_id, person_name, user_nickname, know_counts
			FROM person_info WHERE is_known = 1 ORDER BY know_counts DESC LIMIT ?`, limit)
		if err != nil {
			fail(w, http.StatusInternalServerError, "查询人物画像列表失败: "+err.Error())
			return
		}
		defer rows.Close()
		type profileSummary struct {
			PersonID     string `json:"person_id"`
			PersonName   string `json:"person_name,omitempty"`
			UserNickname string `json:"user_nickname,omitempty"`
			KnowTimes    int    `json:"know_times"`
		}
		items := []profileSummary{}
		for rows.Next() {
			var p profileSummary
			var name, nickname *string
			if err := rows.Scan(&p.PersonID, &name, &nickname, &p.KnowTimes); err != nil {
				continue
			}
			if name != nil {
				p.PersonName = *name
			}
			if nickname != nil {
				p.UserNickname = *nickname
			}
			items = append(items, p)
		}
		ok(w, map[string]any{"profiles": items, "count": len(items)})
	}
}

// handleMemoryProfile 对齐单个人物画像查询（内核聚合视图）。
func handleMemoryProfile(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.Memory == nil {
			fail(w, http.StatusServiceUnavailable, "记忆内核未启用")
			return
		}
		personID := pathValue(r, "person_id")
		profile, err := d.Memory.GetPersonProfile(r.Context(), personID)
		if err != nil {
			fail(w, http.StatusInternalServerError, "查询人物画像失败: "+err.Error())
			return
		}
		if profile == nil {
			fail(w, http.StatusNotFound, "人物画像不存在: "+personID)
			return
		}
		ok(w, map[string]any{"profile": profile})
	}
}
