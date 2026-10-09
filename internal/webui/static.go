// 静态托管 — 磁盘目录（web/dashboard/dist）+ SPA fallback（对齐 app.py 的
// serve_spa 与 Diana 的 spaHandler 模式）：无文件后缀的路径回 index.html，
// 静态文件命中直接返回；api/ws 前缀 404 不做 SPA 回退。
package webui

import (
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// registerStatic 注册静态文件服务（DistDir 为空或不存在时跳过）。
func (s *Server) registerStatic() {
	dist := s.opts.DistDir
	if dist == "" {
		return
	}
	if st, err := os.Stat(dist); err != nil || !st.IsDir() {
		return
	}
	root := filepath.Clean(dist)
	fs := http.StripPrefix("/", http.FileServer(http.Dir(root)))

	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.NotFound(w, r)
			return
		}
		// API/WS 前缀不做 SPA 回退（对齐 full_path.startswith("api/") 分支）。
		p := path.Clean("/" + r.URL.Path)
		if p == "/api" || strings.HasPrefix(p, "/api/") {
			http.NotFound(w, r)
			return
		}
		if p == "/" || p == "" {
			serveIndex(w, r, root)
			return
		}
		// 防目录穿越：解析后的路径必须仍在 root 内。
		full := filepath.Join(root, filepath.FromSlash(p))
		if !strings.HasPrefix(filepath.Clean(full), root+string(os.PathSeparator)) &&
			filepath.Clean(full) != root {
			http.NotFound(w, r)
			return
		}
		if st, err := os.Stat(full); err == nil && !st.IsDir() {
			fs.ServeHTTP(w, r)
			return
		}
		// SPA fallback：无文件后缀的路径回 index.html。
		if path.Ext(p) == "" {
			serveIndex(w, r, root)
			return
		}
		http.NotFound(w, r)
	})
}

// serveIndex 输出 index.html。
func serveIndex(w http.ResponseWriter, r *http.Request, root string) {
	data, err := os.ReadFile(filepath.Join(root, "index.html"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow, noarchive")
	_, _ = w.Write(data)
}
