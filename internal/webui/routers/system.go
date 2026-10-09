// 系统路由 — 对齐 参考/MaiBot/src/webui/routers/system.py 的 /system/status
// 与公告接口（news URL 配置化，简单代理拉取，对齐 news_service 默认源）。
package routers

import (
	"encoding/json"
	"io"
	"net/http"
	"runtime"
	"sync"
	"time"
)

// DefaultNewsSourceURL 对齐 news_service.DEFAULT_NEWS_SOURCE_URL。
const DefaultNewsSourceURL = "http://hyybuth.xyz:10059/news"

// newsItem 对齐 NewsResponse.items[]。
type newsItem struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Content  string `json:"content,omitempty"`
	URL      string `json:"url,omitempty"`
	Datetime string `json:"datetime,omitempty"`
}

// newsCache 简单内存缓存（对齐 news_service 的短期缓存）。
var newsCache = struct {
	mu       sync.Mutex
	items    []newsItem
	fetched  time.Time
	hasError bool
	errMsg   string
}{}

// handleSystemStatus 对齐 GET /system/status（StatusResponse）。
func handleSystemStatus(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		startedAt := d.StartedAt
		if startedAt.IsZero() {
			startedAt = time.Now()
		}
		uptime := time.Since(startedAt).Seconds()
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		writeJSON(w, http.StatusOK, map[string]any{
			"success":    true,
			"running":    true,
			"uptime":     uptime,
			"version":    d.Version,
			"start_time": startedAt.Format(time.RFC3339),
			"go_version": runtime.Version(),
			"memory": map[string]any{
				"heap_alloc_bytes": mem.HeapAlloc,
				"heap_sys_bytes":   mem.HeapSys,
				"sys_bytes":        mem.Sys,
				"num_goroutine":    runtime.NumGoroutine(),
				"num_cpu":          runtime.NumCPU(),
				"gc_count":         mem.NumGC,
			},
		})
	}
}

// handleSystemNews 代理拉取首页资讯（URL 配置化：NewsURL > 环境变量 > 默认源；
// 对齐 GET /news 与 MAIBOT_NEWS_SOURCE_URL 覆盖语义）。
func handleSystemNews(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		force := r.URL.Query().Get("force") == "true" || r.URL.Query().Get("force") == "1"
		if !force {
			newsCache.mu.Lock()
			cached := newsCache.items
			fetched := newsCache.fetched
			errMsg := newsCache.errMsg
			hasError := newsCache.hasError
			newsCache.mu.Unlock()
			// 10 分钟内的缓存直接复用（对齐短期缓存语义）。
			if len(cached) > 0 && time.Since(fetched) < 10*time.Minute {
				ok(w, map[string]any{"items": cached, "cached": true})
				return
			}
			if hasError && time.Since(fetched) < time.Minute {
				fail(w, http.StatusBadGateway, errMsg)
				return
			}
		}
		url := d.NewsURL
		if url == "" {
			url = DefaultNewsSourceURL
		}
		items, err := fetchNews(url, d)
		if err != nil {
			newsCache.mu.Lock()
			newsCache.hasError = true
			newsCache.errMsg = err.Error()
			newsCache.fetched = time.Now()
			newsCache.mu.Unlock()
			fail(w, http.StatusBadGateway, err.Error())
			return
		}
		newsCache.mu.Lock()
		newsCache.items = items
		newsCache.fetched = time.Now()
		newsCache.hasError = false
		newsCache.errMsg = ""
		newsCache.mu.Unlock()
		ok(w, map[string]any{"items": items, "cached": false})
	}
}

// fetchNews 拉取上游资讯并归一化为 newsItem 列表。
func fetchNews(url string, d *Deps) ([]newsItem, error) {
	client := &http.Client{Timeout: 8 * time.Second}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &newsSourceError{msg: "资讯源返回状态码 " + resp.Status}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Items []newsItem `json:"items"`
		News  []newsItem `json:"news"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, &newsSourceError{msg: "资讯源返回的不是有效 JSON"}
	}
	items := parsed.Items
	if items == nil {
		items = parsed.News
	}
	if items == nil {
		items = []newsItem{}
	}
	if len(items) > 20 {
		items = items[:20]
	}
	return items, nil
}

// newsSourceError 对齐 NewsSourceError。
type newsSourceError struct{ msg string }

func (e *newsSourceError) Error() string { return "资讯源错误: " + e.msg }

// handleReloadConfig 对齐 POST /system/reload-config：触发配置热重载。
// Go 版配置读取实时取自 Manager 内存快照，这里返回成功并附当前热重载代数。
func handleReloadConfig(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.Config == nil {
			fail(w, http.StatusServiceUnavailable, "配置服务未启用")
			return
		}
		ok(w, map[string]any{"message": "配置热重载已请求", "reload_revision": d.Config.ReloadRevision()})
	}
}
