// LogService — 日志环形缓冲 + slog handler + 实时广播。
//
// 对齐 logs_ws.py：历史日志快照（load_recent_logs）与实时推送（broadcast_log）；
// Go 版以 slog handler 充当日志钩子，条目字段对齐前端日志查看器：
// {id, timestamp, level, module, message}。
package routers

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

// LogEntry 是一条日志的视图（字段对齐 _format_log_entry）。
type LogEntry struct {
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	Level     string `json:"level"`
	Module    string `json:"module"`
	Message   string `json:"message"`
}

// LogService 维护最近 N 条日志的环形缓冲，并向 Hub 的 logs:main 主题广播。
type LogService struct {
	mu      sync.Mutex
	ring    []LogEntry
	next    int
	count   int
	total   int64
	seq     int64
	level   *slog.LevelVar
	hub     Broadcaster
	module  string
	nowFunc func() time.Time
}

// NewLogService 创建容量为 capacity 的日志环形缓冲（capacity<=0 取 500）。
// level 为可变日志级别（nil 时内部自建一个，SetLevelByName 仍可用）；
// hub 为 nil 时只进缓存不广播。
func NewLogService(capacity int, level *slog.LevelVar, hub Broadcaster) *LogService {
	if capacity <= 0 {
		capacity = 500
	}
	if level == nil {
		level = new(slog.LevelVar)
		level.Set(slog.LevelInfo)
	}
	return &LogService{
		ring:    make([]LogEntry, capacity),
		level:   level,
		hub:     hub,
		module:  "webui.logs",
		nowFunc: time.Now,
	}
}

// SetHub 注入/更换广播器。
func (s *LogService) SetHub(hub Broadcaster) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hub = hub
}

// Enabled implements slog.Handler。
func (s *LogService) Enabled(_ context.Context, l slog.Level) bool {
	if s.level == nil {
		return true
	}
	return l >= s.level.Level()
}

// Handle implements slog.Handler：缓存 + 广播（字段对齐 _format_log_entry）。
//
// 属性拼接统一收口在这里：prefixedHandler 先把预置 attrs（WithAttrs/WithGroup）
// 追加进 message，这里再拼 record attrs（每次调用传的 "err", err 等）。
// 否则 record 级 attrs 会被丢弃，线上只能看到光秃秃的警告消息。
func (s *LogService) Handle(_ context.Context, rec slog.Record) error {
	var msg strings.Builder
	msg.WriteString(rec.Message)
	rec.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&msg, " %s=%v", a.Key, a.Value)
		return true
	})
	entry := LogEntry{
		Timestamp: s.nowFunc().Format("2006-01-02 15:04:05.000"),
		Level:     levelName(rec.Level),
		Module:    moduleOf(rec),
		Message:   msg.String(),
	}
	s.mu.Lock()
	s.seq++
	entry.ID = fmt.Sprintf("%d_%d", s.nowFunc().UnixNano(), s.seq)
	s.ring[s.next] = entry
	s.next = (s.next + 1) % len(s.ring)
	if s.count < len(s.ring) {
		s.count++
	}
	s.total++
	hub := s.hub
	s.mu.Unlock()
	if hub != nil {
		hub.Broadcast("logs", "main", "entry", map[string]any{"entry": entry})
	}
	return nil
}

// WithAttrs implements slog.Handler（预置属性，Handle 统一拼接）。
func (s *LogService) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return s
	}
	var b strings.Builder
	for _, a := range attrs {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value)
	}
	return &prefixedHandler{inner: s, preset: b.String()}
}

// WithGroup implements slog.Handler。
func (s *LogService) WithGroup(name string) slog.Handler {
	if name == "" {
		return s
	}
	return &prefixedHandler{inner: s, preset: " [" + name + "]"}
}

// prefixedHandler 携带 logger.With 预置的属性；record attrs 由
// inner.Handle 统一拼接，这里不再重复处理（避免双重拼接）。
type prefixedHandler struct {
	inner  *LogService
	preset string
}

func (h *prefixedHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *prefixedHandler) Handle(ctx context.Context, rec slog.Record) error {
	if h.preset != "" {
		clone := rec.Clone()
		clone.Message += h.preset
		return h.inner.Handle(ctx, clone)
	}
	return h.inner.Handle(ctx, rec)
}

func (h *prefixedHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	var b strings.Builder
	b.WriteString(h.preset)
	for _, a := range attrs {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value)
	}
	return &prefixedHandler{inner: h.inner, preset: b.String()}
}

func (h *prefixedHandler) WithGroup(name string) slog.Handler {
	if name != "" {
		return &prefixedHandler{inner: h.inner, preset: h.preset + " [" + name + "]"}
	}
	return h
}

// Recent 返回最近 limit 条日志（旧→新；level 非空时按级别过滤）。
func (s *LogService) Recent(limit int, level string) []LogEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]LogEntry, 0, s.count)
	level = strings.ToUpper(strings.TrimSpace(level))
	for i := 0; i < s.count; i++ {
		idx := (s.next - s.count + i + len(s.ring)) % len(s.ring)
		e := s.ring[idx]
		if level != "" && e.Level != level {
			continue
		}
		out = append(out, e)
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	sort.SliceStable(out, func(i, j int) bool {
		// Recent 依 ring 顺序已按时间排列，无需再排；保险起见保持稳定序。
		return false
	})
	return out
}

// Total 返回累计日志条数（环形覆盖前的总数）。
func (s *LogService) Total() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.total
}

// LevelName 返回当前日志级别名（DEBUG/INFO/WARN/ERROR）。
func (s *LogService) LevelName() string {
	if s.level == nil {
		return "INFO"
	}
	return levelName(s.level.Level())
}

// SetLevelByName 设置日志级别；非法名称返回 false。
func (s *LogService) SetLevelByName(name string) bool {
	var level slog.Level
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case "DEBUG":
		level = slog.LevelDebug
	case "INFO":
		level = slog.LevelInfo
	case "WARN", "WARNING":
		level = slog.LevelWarn
	case "ERROR":
		level = slog.LevelError
	default:
		return false
	}
	if s.level != nil {
		s.level.Set(level)
	}
	return true
}

// levelName 把 slog.Level 转为前端级别名（对齐 Python 日志级别命名）。
func levelName(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "ERROR"
	case l >= slog.LevelWarn:
		return "WARN"
	case l >= slog.LevelInfo:
		return "INFO"
	default:
		return "DEBUG"
	}
}

// moduleOf 从 slog record 提取模块名（logger 名经 slog.Logger.With 传递时
// 由宿主以 attrs 形式提供；这里回退到 handler 模块名）。
func moduleOf(rec slog.Record) string {
	name := ""
	rec.Attrs(func(a slog.Attr) bool {
		if a.Key == "module" || a.Key == "logger" {
			if v, ok := a.Value.Any().(string); ok {
				name = v
				return false
			}
		}
		return true
	})
	if name == "" {
		return "main"
	}
	return name
}

// NewShared 构造一个尚未绑定 hub 的日志服务（宿主先拿去当 slog handler，
// Server 装配时经 webui.Options.Logs 注入并 SetHub）。
func NewShared(capacity int, level *slog.LevelVar) *LogService {
	return NewLogService(capacity, level, nil)
}
