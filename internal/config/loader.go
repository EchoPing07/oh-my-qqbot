// 配置装载器 — 对齐 参考MaiBot/src/config/config.py 的
// load_config_from_file / write_config_to_file / generate_new_config_file /
// ConfigManager（热重载、回调注册、变更节对比）。
//
// 核心语义：
//   - 配置文件缺失 → 生成默认模板（对齐 generate_new_config_file）
//   - 用户 toml 缺字段 → 保留 Go 结构体默认值（对齐 pydantic default_factory 的缺省合成）
//   - 文件带 [inner] version 表；版本落后或缺字段时用完整默认结构回写（对齐 write_config_to_file）
//   - fsnotify 热重载 + 按（scope, section 前缀）分发的回调（对齐 register_reload_callback）
package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	toml "github.com/pelletier/go-toml/v2"
)

// Manager 对齐 config.ConfigManager。
type Manager struct {
	botConfigPath   string
	modelConfigPath string

	mu        sync.RWMutex
	global    *Config
	model     *ModelConfig
	reloadRev int

	cbMu       sync.Mutex
	callbacks  []*reloadCallbackRegistration

	watchMu     sync.Mutex
	watcher     *fsnotify.Watcher
	watchStop   chan struct{}
	watchClosed chan struct{}
}

// reloadCallbackRegistration 对齐 _ReloadCallbackRegistration。
type reloadCallbackRegistration struct {
	id       uint64
	callback ConfigReloadCallback
	sections []string // 点号分隔的 bot 配置节前缀；nil 表示全部
}

// ConfigReloadCallback 是热重载回调：changedScopes 为 "bot"/"model"，
// changedSections 为发生变化的 bot 顶层节名（nil 表示未知，保守通知）。
type ConfigReloadCallback func(changedScopes []string, changedSections []string)

var nextCallbackID uint64

// NewManager 创建配置管理器（路径对齐 ConfigManager.__init__ 的 config/ 目录布局）。
func NewManager(configDir string) *Manager {
	return &Manager{
		botConfigPath:   filepath.Join(configDir, "bot_config.toml"),
		modelConfigPath: filepath.Join(configDir, "model_config.toml"),
	}
}

// BotConfigPath / ModelConfigPath 返回配置文件路径。
func (m *Manager) BotConfigPath() string   { return m.botConfigPath }
func (m *Manager) ModelConfigPath() string { return m.modelConfigPath }

// Initialize 对齐 ConfigManager.initialize：加载双配置（缺文件则生成默认模板）。
func (m *Manager) Initialize() error {
	global, err := LoadBotConfig(m.botConfigPath)
	if err != nil {
		return fmt.Errorf("加载 bot_config 失败: %w", err)
	}
	model, err := LoadModelConfig(m.modelConfigPath)
	if err != nil {
		return fmt.Errorf("加载 model_config 失败: %w", err)
	}
	m.mu.Lock()
	m.global = global
	m.model = model
	m.mu.Unlock()
	return nil
}

// GetGlobalConfig 对齐 get_global_config。
func (m *Manager) GetGlobalConfig() *Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.global == nil {
		panic("config: bot 配置尚未初始化（对齐 config.global_not_initialized）")
	}
	return m.global
}

// GetModelConfig 对齐 get_model_config。
func (m *Manager) GetModelConfig() *ModelConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.model == nil {
		panic("config: model 配置尚未初始化（对齐 config.model_not_initialized）")
	}
	return m.model
}

// ReloadRevision 返回热重载代数（对齐 reload_revision）。
func (m *Manager) ReloadRevision() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.reloadRev
}

// ReloadNow 立即重载双配置并分发回调（供 WebUI /system/reload-config 与热重载共用）。
// scopes 为 "bot"/"model" 子集；空/全量时重载双文件。
func (m *Manager) ReloadNow(scopes ...string) error {
	return m.reloadNow(scopes)
}

// ---------------------------------------------------------------------------
// 加载与缺省合成
// ---------------------------------------------------------------------------

// LoadBotConfig 从磁盘读取 bot_config.toml；缺失时生成默认模板。
// 对齐 load_config_from_file(Config, ...)：缺字段用默认值合成，
// 版本旧于 ConfigVersion 时用完整结构回写。
func LoadBotConfig(path string) (*Config, error) {
	loaded, err := loadConfigFile(path, ConfigVersion, defaultBotTemplate)
	if err != nil {
		return nil, err
	}
	cfg := DefaultConfig()
	if len(loaded.data) > 0 {
		if err := unmarshalSection(loaded.data, cfg); err != nil {
			return nil, fmt.Errorf("解析 %s 失败: %w", path, err)
		}
	}
	if loaded.needsRewrite {
		if err := writeConfigToPath(path, cfg, ConfigVersion); err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

// bareModelConfigBase 返回 pydantic 语义的空壳默认值：
// models/api_providers 为空列表，model_task_config 各任务为 TaskConfig 字段默认值
// （对齐 ModelConfig 的 default_factory 与 TaskConfig 的字段默认；
// 生成模板时才使用 default_model_config.py 的模板值）。
func bareModelConfigBase() *ModelConfig {
	def := defaultTaskConfig()
	mtc := ModelTaskConfig{}
	for _, tc := range mtc.TaskConfigs() {
		*tc = def
	}
	return &ModelConfig{ModelTaskConfig: mtc}
}

// LoadModelConfig 从磁盘读取 model_config.toml；缺失时生成默认模板。
// 对齐 load_config_from_file(ModelConfig, ...)，并执行 model_post_init 校验。
func LoadModelConfig(path string) (*ModelConfig, error) {
	loaded, err := loadConfigFile(path, ModelConfigVersion, defaultModelTemplate)
	if err != nil {
		return nil, err
	}
	cfg := bareModelConfigBase()
	if len(loaded.data) > 0 {
		if err := unmarshalSection(loaded.data, cfg); err != nil {
			return nil, fmt.Errorf("解析 %s 失败: %w", path, err)
		}
	}
	synthesizeModelConfigDefaults(cfg)
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("校验 %s 失败: %w", path, err)
	}
	if loaded.needsRewrite {
		if err := writeConfigToPath(path, cfg, ModelConfigVersion); err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

type loadedFile struct {
	data         map[string]any
	needsRewrite bool
}

// defaultTemplateFunc 生成默认配置结构（对齐 generate_new_config_file）。
type defaultTemplateFunc func() any

func defaultBotTemplate() any    { return DefaultConfig() }
func defaultModelTemplate() any  { return DefaultModelConfig() }

// loadConfigFile 读取文件并判断是否需要回写（缺失/版本落后/缺 inner）。
func loadConfigFile(path, wantVersion string, tmpl defaultTemplateFunc) (*loadedFile, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		slog.Warn("配置文件缺失，正在生成默认配置", "path", path)
		if err := writeConfigToPath(path, tmpl(), wantVersion); err != nil {
			return nil, err
		}
		// 生成的文件即为最新版本，直接读回（保持单一读取路径）。
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置失败: %w", err)
	}
	var parsed map[string]any
	if err := toml.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("解析 TOML 失败: %w", err)
	}
	needsRewrite := false
	innerRaw, ok := parsed["inner"]
	if !ok {
		// 对齐 config.missing_inner_version：宽容处理为需要回写补版本。
		slog.Warn("配置缺少 [inner] 版本表，将回写补齐", "path", path)
		needsRewrite = true
	} else {
		inner, isTable := innerRaw.(map[string]any)
		if !isTable {
			return nil, fmt.Errorf("配置 [inner] 表无效")
		}
		ver, _ := inner["version"].(string)
		if ver == "" {
			return nil, fmt.Errorf("配置 [inner].version 无效")
		}
		if CompareVersions(ver, wantVersion) {
			slog.Info("检测到配置版本升级，将回写新结构", "file", filepath.Base(path), "old", ver, "new", wantVersion)
			needsRewrite = true
		}
	}
	delete(parsed, "inner")
	return &loadedFile{data: parsed, needsRewrite: needsRewrite}, nil
}

// unmarshalSection 将剩余 TOML 数据合并进默认结构体（缺省字段保留默认值）。
func unmarshalSection(data map[string]any, dst any) error {
	buf, err := toml.Marshal(data)
	if err != nil {
		return err
	}
	return toml.Unmarshal(buf, dst)
}

// synthesizeModelConfigDefaults 补齐 TOML 未写出的模型配置缺省字段
// （对齐 pydantic 字段默认值：send_temperature=true、max_tokens/temperature 等）。
func synthesizeModelConfigDefaults(cfg *ModelConfig) {
	for i := range cfg.APIProviders {
		defaultAPIProviderFields(&cfg.APIProviders[i])
	}
	for i := range cfg.Models {
		m := &cfg.Models[i]
		if m.ExtraParams == nil {
			m.ExtraParams = map[string]any{}
		}
		if m.PricePeriods == nil {
			m.PricePeriods = []ModelPricePeriod{}
		}
		// send_temperature 缺省 true（对齐 pydantic 字段默认值）。
		if m.SendTemperature == nil {
			m.SendTemperature = BoolPtr(true)
		}
	}
	for _, tc := range cfg.ModelTaskConfig.TaskConfigs() {
		if tc.ModelList == nil {
			tc.ModelList = []string{}
		}
		if tc.MaxTokens == 0 {
			tc.MaxTokens = 4096
		}
		if tc.SelectionStrategy == "" {
			tc.SelectionStrategy = "balance"
		}
		if tc.HardTimeout == 0 {
			tc.HardTimeout = 240.0
		}
	}
}

// ---------------------------------------------------------------------------
// 写回（原子写）
// ---------------------------------------------------------------------------

// writeConfigToPath 对齐 write_config_to_file：先写 [inner] 版本表 + 完整默认结构，
// 经临时文件 + rename 原子落盘。inner 表位于文件头（对齐 tomlkit add 顺序）。
func writeConfigToPath(path string, cfg any, version string) error {
	// 将配置结构体序列化后再逐节合并，保证 [inner] 位于文件头。
	buf, err := toml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}
	var sections map[string]any
	if err := toml.Unmarshal(buf, &sections); err != nil {
		return fmt.Errorf("再解析配置失败: %w", err)
	}
	delete(sections, "inner")
	// go-toml 的 map 序列化按键名排序，无法把 inner 放在最前；
	// 采用手工拼接：inner 头 + 各节体（对齐 Python 写出形态 [inner] 在顶部）。
	header, err := toml.Marshal(map[string]any{"inner": map[string]any{"version": version}})
	if err != nil {
		return err
	}
	body, err := toml.Marshal(sections)
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}
	out := append([]byte(header), body...)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// SaveBotConfig 把内存中的 bot 配置写回磁盘（原子写回）。
func (m *Manager) SaveBotConfig(cfg *Config) error {
	if err := writeConfigToPath(m.botConfigPath, cfg, ConfigVersion); err != nil {
		return err
	}
	m.mu.Lock()
	m.global = cfg
	m.mu.Unlock()
	return nil
}

// SaveModelConfig 把内存中的 model 配置写回磁盘（原子写回）。
func (m *Manager) SaveModelConfig(cfg *ModelConfig) error {
	synthesizeModelConfigDefaults(cfg)
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := writeConfigToPath(m.modelConfigPath, cfg, ModelConfigVersion); err != nil {
		return err
	}
	m.mu.Lock()
	m.model = cfg
	m.mu.Unlock()
	return nil
}

// ---------------------------------------------------------------------------
// 热重载 — 对齐 ConfigManager 热重载/watcher 语义
// ---------------------------------------------------------------------------

// RegisterReloadCallback 对齐 register_reload_callback：sections 支持点号分隔的
// bot 配置节前缀（如 "chat.reply_timing"），按前缀匹配；nil 表示全部通知。
func (m *Manager) RegisterReloadCallback(cb ConfigReloadCallback, sections ...string) *ReloadCallbackHandle {
	m.cbMu.Lock()
	defer m.cbMu.Unlock()
	nextCallbackID++
	normalized := normalizeSections(sections)
	reg := &reloadCallbackRegistration{id: nextCallbackID, callback: cb, sections: normalized}
	m.callbacks = append(m.callbacks, reg)
	return &ReloadCallbackHandle{m: m, id: reg.id}
}

// ReloadCallbackHandle 用于注销回调（对齐 unregister_reload_callback）。
type ReloadCallbackHandle struct {
	m  *Manager
	id uint64
}

// Unregister 注销该回调。
func (h *ReloadCallbackHandle) Unregister() {
	h.m.cbMu.Lock()
	defer h.m.cbMu.Unlock()
	out := h.m.callbacks[:0]
	for _, reg := range h.m.callbacks {
		if reg.id != h.id {
			out = append(out, reg)
		}
	}
	h.m.callbacks = out
}

func normalizeSections(sections []string) []string {
	if len(sections) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, s := range sections {
		parts := strings.Split(strings.ToLower(strings.TrimSpace(s)), ".")
		var clean []string
		for _, p := range parts {
			if p != "" {
				clean = append(clean, p)
			}
		}
		if len(clean) == 0 {
			continue
		}
		dotted := strings.Join(clean, ".")
		if !seen[dotted] {
			seen[dotted] = true
			out = append(out, dotted)
		}
	}
	return out
}

// sectionMatches 对齐 _section_matches：点号分段后任一方为另一方前缀即命中。
func sectionMatches(declared, changed string) bool {
	dp := strings.Split(declared, ".")
	cp := strings.Split(changed, ".")
	n := len(dp)
	if len(cp) < n {
		n = len(cp)
	}
	for i := 0; i < n; i++ {
		if dp[i] != cp[i] {
			return false
		}
	}
	return true
}

// reloadNow 立即重载双配置并分发回调（热重载与测试共用）。
func (m *Manager) reloadNow(changedScopes []string) error {
	var newGlobal *Config
	var newModel *ModelConfig
	var err error
	scopeSet := map[string]bool{}
	for _, s := range changedScopes {
		scopeSet[s] = true
	}
	if scopeSet["bot"] || len(scopeSet) == 0 {
		if newGlobal, err = LoadBotConfig(m.botConfigPath); err != nil {
			return err
		}
	}
	if scopeSet["model"] || len(scopeSet) == 0 {
		if newModel, err = LoadModelConfig(m.modelConfigPath); err != nil {
			return err
		}
	}

	m.mu.Lock()
	var changedSections []string
	sectionsKnown := false
	if newGlobal != nil {
		if m.global != nil {
			changedSections, sectionsKnown = diffBotConfigSections(m.global, newGlobal)
		}
		m.global = newGlobal
	}
	if newModel != nil {
		m.model = newModel
	}
	m.reloadRev++
	m.mu.Unlock()

	// 分发回调（对齐 _is_registration_interested 的命中规则）。
	m.cbMu.Lock()
	regs := make([]*reloadCallbackRegistration, len(m.callbacks))
	copy(regs, m.callbacks)
	m.cbMu.Unlock()
	for _, reg := range regs {
		if !registrationInterested(reg, scopeSet, sectionsKnown, changedSections) {
			continue
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("配置热重载回调 panic", "panic", r)
				}
			}()
			var sectionsArg []string
			if sectionsKnown {
				sectionsArg = changedSections
			}
			reg.callback(changedScopes, sectionsArg)
		}()
	}
	return nil
}

// registrationInterested 对齐 _is_registration_interested：
// 注册节为 nil → 全部通知；只关心 bot 节的回调不接收纯 model 重载；
// 变更节未知时保守通知。
func registrationInterested(reg *reloadCallbackRegistration, scopes map[string]bool, sectionsKnown bool, changed []string) bool {
	if reg.sections == nil {
		return true
	}
	if !scopes["bot"] {
		return false
	}
	if !sectionsKnown {
		return true
	}
	for _, declared := range reg.sections {
		for _, changedSec := range changed {
			if sectionMatches(declared, changedSec) {
				return true
			}
		}
	}
	return false
}

// diffBotConfigSections 对齐 _diff_bot_config_sections：对比新旧 bot 配置的顶层节。
// 使用 TOML 序列化做值比较；失败时返回 known=false 由调用方保守处理。
func diffBotConfigSections(oldCfg, newCfg *Config) ([]string, bool) {
	oldBuf, err1 := toml.Marshal(oldCfg)
	newBuf, err2 := toml.Marshal(newCfg)
	if err1 != nil || err2 != nil {
		return nil, false
	}
	var oldMap, newMap map[string]any
	if err := toml.Unmarshal(oldBuf, &oldMap); err != nil {
		return nil, false
	}
	if err := toml.Unmarshal(newBuf, &newMap); err != nil {
		return nil, false
	}
	names := make([]string, 0, len(newMap))
	for k := range newMap {
		names = append(names, k)
	}
	sort.Strings(names)
	var changed []string
	for _, k := range names {
		if !tomlValueEqual(oldMap[k], newMap[k]) {
			changed = append(changed, k)
		}
	}
	return changed, true
}

// tomlValueEqual 递归比较反序列化后的 TOML 值。
func tomlValueEqual(a, b any) bool {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, v := range av {
			bvv, ok := bv[k]
			if !ok || !tomlValueEqual(v, bvv) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !tomlValueEqual(av[i], bv[i]) {
				return false
			}
		}
		return true
	default:
		return a == b
	}
}

// StartWatching 启动 fsnotify 热重载（对齐 ConfigManager 的 FileWatcher 接线）。
// 带最小重载间隔（1s，对齐 _hot_reload_min_interval_s）防抖。
//
// watch 的是配置目录而非单个文件（B3）：Linux 下 tmp+rename 原子写会让
// 逐文件 watch 指向旧 inode，热重载静默失效；watch 目录 + 按事件文件名
// 过滤（scopeForPath）可覆盖 rename 形态的保存。目录不存在时回退为
// 逐文件 watch（兜底，行为同旧实现）。
func (m *Manager) StartWatching() error {
	m.watchMu.Lock()
	defer m.watchMu.Unlock()
	if m.watcher != nil {
		return nil
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	if err := w.Add(filepath.Dir(m.botConfigPath)); err != nil {
		// 回退：目录不存在等场景逐文件监听。
		for _, p := range []string{m.botConfigPath, m.modelConfigPath} {
			if err := w.Add(p); err != nil {
				w.Close()
				return fmt.Errorf("监听配置目录/文件失败: %w", err)
			}
		}
	}
	m.watcher = w
	m.watchStop = make(chan struct{})
	m.watchClosed = make(chan struct{})
	go m.watchLoop()
	return nil
}

// StopWatching 停止热重载监听。
func (m *Manager) StopWatching() {
	m.watchMu.Lock()
	defer m.watchMu.Unlock()
	if m.watcher == nil {
		return
	}
	close(m.watchStop)
	<-m.watchClosed
	m.watcher.Close()
	m.watcher = nil
}

func (m *Manager) watchLoop() {
	defer close(m.watchClosed)
	const minInterval = 1 * time.Second
	var lastReload time.Time
	var pendingScope []string
	var timer *time.Timer
	var flushC <-chan time.Time
	for {
		select {
		case <-m.watchStop:
			return
		case event, ok := <-m.watcher.Events:
			if !ok {
				return
			}
			// 目录 watch 下 event.Name 是具体变更文件；编辑器保存常是
			// rename/create（tmp+rename 原子写）。任何写事件 + 文件名命中
			// 才触发（scopeForPath 过滤目录内其他文件与 .tmp 中间产物）。
			if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
				continue
			}
			scope := scopeForPath(event.Name)
			if scope == "" {
				continue
			}
			// 合并同一防抖窗口内的多个 scope。
			found := false
			for _, s := range pendingScope {
				if s == scope {
					found = true
				}
			}
			if !found {
				pendingScope = append(pendingScope, scope)
			}
			if timer == nil {
				timer = time.NewTimer(minInterval)
				flushC = timer.C
			}
		case <-flushC:
			timer = nil
			flushC = nil
			scopes := pendingScope
			pendingScope = nil
			if since := time.Since(lastReload); since < minInterval {
				time.Sleep(minInterval - since)
			}
			if err := m.reloadNow(scopes); err != nil {
				slog.Error("配置热重载失败，保留旧配置", "err", err)
			} else {
				lastReload = time.Now()
				slog.Info("配置已热重载", "scopes", strings.Join(scopes, ","))
			}
		}
	}
}

// scopeForPath 对齐 _resolve_changed_scopes 的文件名→scope 映射。
func scopeForPath(path string) string {
	switch filepath.Base(path) {
	case "bot_config.toml":
		return "bot"
	case "model_config.toml":
		return "model"
	}
	return ""
}
