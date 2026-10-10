// user_emoji 路由 — 本地用户表情包管理（简化面：列出/删除/内容回源）。
// news 路由无前端消费方，已随轻量化原则移除。
package routers

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// ---------------------------------------------------------------------------
// user_emojis — 对齐 routers/user_emoji.py（本地用户表情包管理，简化面：
// 列出/删除；上传走 POST multipart 在此实现）
// ---------------------------------------------------------------------------

// userEmojiRoot 返回用户表情包根目录（data/webui_user_emojis）。
func userEmojiRoot(d *Deps) string {
	return filepath.Join(d.DataDir, "webui_user_emojis")
}

// userEmojiItem 对齐列表项。
type userEmojiItem struct {
	ID          string `json:"id"`
	UserID      string `json:"user_id"`
	FileName    string `json:"file_name"`
	SizeBytes   int64  `json:"size_bytes"`
	ContentType string `json:"content_type"`
	URL         string `json:"content_url"`
}

// handleUserEmojiList 对齐 GET /user-emojis（user_id 查询参数必填）。
func handleUserEmojiList(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := sanitizeUserID(r.URL.Query().Get("user_id"))
		if userID == "" {
			fail(w, http.StatusBadRequest, "缺少 user_id 参数")
			return
		}
		dir := filepath.Join(userEmojiRoot(d), userID)
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				ok(w, map[string]any{"emojis": []userEmojiItem{}, "count": 0})
				return
			}
			fail(w, http.StatusInternalServerError, "读取用户表情目录失败: "+err.Error())
			return
		}
		items := []userEmojiItem{}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue
			}
			id := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
			items = append(items, userEmojiItem{
				ID: id, UserID: userID, FileName: entry.Name(),
				SizeBytes: info.Size(), ContentType: guessImageContentType(entry.Name()),
				URL: "/api/webui/user-emojis/" + id + "/content?user_id=" + userID,
			})
		}
		ok(w, map[string]any{"emojis": items, "count": len(items)})
	}
}

// userEmojiExtensions 是用户表情文件扩展名白名单（列表与内容回源共用）。
var userEmojiExtensions = []string{".png", ".gif", ".jpg", ".jpeg", ".webp"}

// handleUserEmojiContent 对齐 GET /user-emojis/{emoji_id}/content — S8 修复：
// 列表项的 content_url 此前指向未注册路由。实现：requireAuth + ID 白名单
// （sanitizeUserID 阻断路径穿越）+ 扩展名白名单枚举回源 + 目录边界复核。
func handleUserEmojiContent(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := sanitizeUserID(r.URL.Query().Get("user_id"))
		emojiID := sanitizeUserID(pathValue(r, "emoji_id"))
		if userID == "" || emojiID == "" {
			fail(w, http.StatusBadRequest, "user_id 与 emoji_id 不能为空")
			return
		}
		root := userEmojiRoot(d)
		dir := filepath.Join(root, userID)
		for _, ext := range userEmojiExtensions {
			path := filepath.Join(dir, emojiID+ext)
			info, err := os.Stat(path)
			if err != nil || info.IsDir() {
				continue
			}
			// 双保险：最终路径必须仍在用户表情根目录内。
			if rel, relErr := filepath.Rel(root, path); relErr != nil ||
				rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				continue
			}
			f, err := os.Open(path)
			if err != nil {
				continue
			}
			defer f.Close()
			w.Header().Set("Content-Type", guessImageContentType(path))
			http.ServeContent(w, r, path, info.ModTime(), f)
			return
		}
		fail(w, http.StatusNotFound, "表情不存在: "+emojiID)
	}
}

// handleUserEmojiDelete 对齐 DELETE /user-emojis/{emoji_id}。
func handleUserEmojiDelete(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := sanitizeUserID(r.URL.Query().Get("user_id"))
		emojiID := sanitizeUserID(pathValue(r, "emoji_id"))
		if userID == "" || emojiID == "" {
			fail(w, http.StatusBadRequest, "user_id 与 emoji_id 不能为空")
			return
		}
		dir := filepath.Join(userEmojiRoot(d), userID)
		removed := false
		for _, ext := range userEmojiExtensions {
			if err := os.Remove(filepath.Join(dir, emojiID+ext)); err == nil {
				removed = true
				break
			}
		}
		if !removed {
			fail(w, http.StatusNotFound, "表情不存在: "+emojiID)
			return
		}
		ok(w, map[string]any{"message": "表情已删除", "id": emojiID})
	}
}

// sanitizeUserID 白名单化 ID（十六进制/短横线），阻断路径穿越。
func sanitizeUserID(raw string) string {
	var b strings.Builder
	for _, ch := range raw {
		if ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_' {
			b.WriteRune(ch)
		}
	}
	return b.String()
}

// guessImageContentType 按扩展名猜测 MIME。
func guessImageContentType(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	default:
		return "application/octet-stream"
	}
}
