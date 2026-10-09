// news / user_emoji 路由 — 简化实现。
package routers

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// ---------------------------------------------------------------------------
// news — 对齐 routers/news.py（GET /news 直接复用资讯代理）
// ---------------------------------------------------------------------------

// handleNews 对齐 GET /news（与 /system/news 同源；保留独立路径对齐 Python 形态）。
func handleNews(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		handleSystemNews(d)(w, r)
	}
}

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
		for _, ext := range []string{".png", ".gif", ".jpg", ".jpeg", ".webp"} {
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

// newUserEmojiID 生成 32 位十六进制 ID（对齐 uuid4().hex 形态；上传接口启用时使用）。
func newUserEmojiID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

var _ = newUserEmojiID // 预留：POST /user-emojis 上传路径使用

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
