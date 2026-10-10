// Package prompt 实现 MaiBot 的 prompt 模板加载与渲染：
// 对齐 参考MaiBot/src/prompt/prompt_manager.py 与 src/common/prompt_i18n.py。
//
// 语义要点：
//   - .prompt 文件位于 <root>/prompts/<locale>/，data/custom_prompts/<locale>/ 覆盖
//   - {var} 占位符由注册的渲染上下文（字符串或函数）填充
//   - 模板中的 {{ }} 转义为字面 { }；渲染期间用私有占位符 0xFDE9/0xFDEA 保护
//   - 支持嵌套 prompt 引用（字段名命中其他 prompt 名时递归渲染）
package prompt

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// SuffixPrompt 对齐 prompt_manager.SUFFIX_PROMPT。
const SuffixPrompt = ".prompt"

// 私有占位符 — 对齐 prompt_manager._LEFT_BRACE / _RIGHT_BRACE (chr(0xFDE9)/chr(0xFDEA))。
const (
	LeftBraceRepl  = "\uFDE9"
	RightBraceRepl = "\uFDEA"
)

// DefaultLocale 对齐 i18n.loaders.DEFAULT_LOCALE。
const DefaultLocale = "zh-CN"

// Manager 对齐 prompt_manager.PromptManager。
type Manager struct {
	mu sync.RWMutex

	prompts map[string]*Template

	// contextConstructors 对齐 _context_construct_functions：全局上下文构造函数。
	contextConstructors map[string]RenderFunc

	// reloadCallbacks 在模板缓存失效（文件变更）后调用。
	reloadObservers []func(changed []string)

	locale          string
	promptsRoot     string
	customPromptsRoot string
}

// RenderFunc 接收 prompt 名称，返回填充内容（对齐 Callable[[str], str]）。
type RenderFunc func(promptName string) (string, error)

// NewManager 创建 PromptManager。
// locale 为空时取 DefaultLocale；promptsRoot 是仓库的 prompts/ 目录；
// customPromptsRoot 是 data/custom_prompts/（为空时取 promptsRoot 同级的 data/custom_prompts）。
func NewManager(promptsRoot, customPromptsRoot, locale string) *Manager {
	if locale == "" {
		locale = DefaultLocale
	}
	root := promptsRoot
	if root == "" {
		root = "prompts"
	}
	custom := customPromptsRoot
	if custom == "" {
		custom = filepath.Join(filepath.Dir(root), "data", "custom_prompts")
	}
	return &Manager{
		prompts:             map[string]*Template{},
		contextConstructors: map[string]RenderFunc{},
		locale:              locale,
		promptsRoot:         root,
		customPromptsRoot:   custom,
	}
}

// Template 对齐 prompt_manager.Prompt。
type Template struct {
	Name     string
	template string

	mu       sync.Mutex
	contexts map[string]RenderFunc
	cloned   bool
}

// AddContext 对齐 Prompt.add_context：注册渲染上下文（固定字符串或函数）。
func (t *Template) AddContext(name string, fnOrStr any) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.contexts == nil {
		t.contexts = map[string]RenderFunc{}
	}
	if _, exists := t.contexts[name]; exists {
		return fmt.Errorf("Context function name '%s' 已存在于 Prompt '%s' 中", name, t.Name)
	}
	switch v := fnOrStr.(type) {
	case string:
		s := v
		t.contexts[name] = func(string) (string, error) { return s, nil }
	case RenderFunc:
		t.contexts[name] = v
	case func(string) (string, error):
		t.contexts[name] = v
	default:
		return fmt.Errorf("AddContext: 不支持的上下文类型 %T", fnOrStr)
	}
	return nil
}

// contextsSnapshot 返回模板上下文的快照（线程安全读取）。
func (t *Template) contextsSnapshot() map[string]RenderFunc {
	t.mu.Lock()
	defer t.mu.Unlock()
	return contextsSnapshot(t.contexts)
}

// Clone 对齐 Prompt.clone。
func (t *Template) Clone() *Template {
	t.mu.Lock()
	defer t.mu.Unlock()
	c := &Template{
		Name:     t.Name,
		template: t.template,
		contexts: make(map[string]RenderFunc, len(t.contexts)),
		cloned:   true,
	}
	for k, v := range t.contexts {
		c.contexts[k] = v
	}
	return c
}

// validate 对齐 Prompt.__post_init__：{{ }} 保护后不允许裸 {}。
func validateTemplate(name, template string) error {
	if name == "" {
		return fmt.Errorf("prompt_name 不能为空")
	}
	if template == "" {
		return fmt.Errorf("template 不能为空")
	}
	tmp := strings.ReplaceAll(template, "{{", LeftBraceRepl)
	tmp = strings.ReplaceAll(tmp, "}}", RightBraceRepl)
	if strings.Contains(tmp, "{}") {
		return fmt.Errorf("模板 '%s' 中不允许使用未命名的占位符 '{}'", name)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 文件发现与覆盖解析 — 对齐 prompt_i18n.py
// ---------------------------------------------------------------------------

// safeSegmentPattern 对齐 SAFE_SEGMENT_PATTERN。
var safeSegmentPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// NormalizePromptName 对齐 normalize_prompt_name：去 .prompt 后缀并校验安全字符。
func NormalizePromptName(name string) (string, error) {
	candidate := strings.TrimSpace(name)
	candidate = strings.TrimSuffix(candidate, SuffixPrompt)
	if candidate == "" || candidate == "." || candidate == ".." || !safeSegmentPattern.MatchString(candidate) {
		return "", fmt.Errorf("无效的 prompt 名称: %q", name)
	}
	return candidate, nil
}

// iterLocaleCandidates 对齐 _iter_locale_candidates：请求语言优先，回退默认语言。
func iterLocaleCandidates(requested string) []string {
	if requested == DefaultLocale {
		return []string{DefaultLocale}
	}
	return []string{requested, DefaultLocale}
}

// resolvePromptPath 对齐 resolve_prompt_path（无 category 的平铺布局）：
// 依次找 custom/<locale>/、custom 根目录（legacy）、prompts/<locale>/。
func (m *Manager) resolvePromptPath(name, locale string) (string, bool, error) {
	for _, loc := range iterLocaleCandidates(locale) {
		// 自定义目录按语言层覆盖。
		candidate := filepath.Join(m.customPromptsRoot, loc, name+SuffixPrompt)
		if fileExists(candidate) {
			return candidate, true, nil
		}
		// legacy：custom_prompts 根目录平铺。
		legacy := filepath.Join(m.customPromptsRoot, name+SuffixPrompt)
		if fileExists(legacy) {
			return legacy, true, nil
		}
		// 内置模板层。
		candidate = filepath.Join(m.promptsRoot, loc, name+SuffixPrompt)
		if fileExists(candidate) {
			return candidate, false, nil
		}
	}
	return "", false, fmt.Errorf("prompt 模板未找到: locale=%s name=%s", locale, name)
}

// ListPromptNames 对齐 list_prompt_templates：列出默认语言层的全部模板名。
func (m *Manager) ListPromptNames() []string {
	entries, err := os.ReadDir(filepath.Join(m.promptsRoot, DefaultLocale))
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), SuffixPrompt) {
			names = append(names, strings.TrimSuffix(e.Name(), SuffixPrompt))
		}
	}
	sort.Strings(names)
	return names
}

// ---------------------------------------------------------------------------
// 加载
// ---------------------------------------------------------------------------

// LoadPrompts 对齐 PromptManager.load_prompts：
// 加载全部内置模板，custom_prompts 同名覆盖；仅存在于 custom 的模板也加载。
func (m *Manager) LoadPrompts() error {
	loaded := map[string]*Template{}
	// 1) 内置模板层（含 custom 覆盖解析）。
	for _, name := range m.ListPromptNames() {
		path, customized, err := m.resolvePromptPath(name, m.locale)
		if err != nil {
			return fmt.Errorf("解析模板 %s 失败: %w", name, err)
		}
		tpl, err := readTemplate(name, path, customized)
		if err != nil {
			return err
		}
		loaded[name] = tpl
	}
	// 2) 仅存在于 custom 目录的模板（对齐「已经加载过了，跳过」之外的分支）。
	for _, loc := range append([]string{m.locale}, localeDirs(m.customPromptsRoot)...) {
		if loc == "" {
			continue
		}
		dir := filepath.Join(m.customPromptsRoot, loc)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), SuffixPrompt) {
				continue
			}
			name := strings.TrimSuffix(e.Name(), SuffixPrompt)
			if _, ok := loaded[name]; ok {
				continue
			}
			tpl, err := readTemplate(name, filepath.Join(dir, e.Name()), true)
			if err != nil {
				return err
			}
			loaded[name] = tpl
		}
	}
	m.mu.Lock()
	m.prompts = loaded
	m.mu.Unlock()
	return nil
}

func localeDirs(customRoot string) []string {
	entries, err := os.ReadDir(customRoot)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

func readTemplate(name, path string, customized bool) (*Template, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取模板 %s 失败: %w", path, err)
	}
	tpl := &Template{Name: name, template: string(raw)}
	if err := validateTemplate(name, tpl.template); err != nil {
		return nil, err
	}
	return tpl, nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// ---------------------------------------------------------------------------
// 获取与渲染
// ---------------------------------------------------------------------------

// GetPrompt 对齐 PromptManager.get_prompt：返回克隆，克隆可注入上下文后渲染。
func (m *Manager) GetPrompt(name string) (*Template, error) {
	m.mu.RLock()
	tpl, ok := m.prompts[name]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("Prompt name '%s' 不存在", name)
	}
	return tpl.Clone(), nil
}

// AddContextConstructor 对齐 add_context_construct_function：注册全局构造函数。
func (m *Manager) AddContextConstructor(name string, fn RenderFunc) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.contextConstructors[name]; exists {
		return fmt.Errorf("Construct function name '%s' 已存在", name)
	}
	if _, exists := m.prompts[name]; exists {
		return fmt.Errorf("Prompt name '%s' 已存在", name)
	}
	m.contextConstructors[name] = fn
	return nil
}

// RegisterReloadObserver 注册模板重载观察者（文件缓存变更后通知）。
func (m *Manager) RegisterReloadObserver(fn func(changed []string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reloadObservers = append(m.reloadObservers, fn)
}

// Reload 重新加载全部模板并通知观察者变更的模板名
//（对齐 _reload_if_prompt_cache_changed 的差异日志路径）。
func (m *Manager) Reload() {
	old := map[string]string{}
	m.mu.RLock()
	for k, v := range m.prompts {
		old[k] = v.template
	}
	m.mu.RUnlock()
	if err := m.LoadPrompts(); err != nil {
		// B7：加载失败不能静默——保留旧模板继续服务，但必须留下排查线索。
		slog.Warn("prompt 重载失败，沿用旧模板", "err", err)
		return
	}
	m.mu.RLock()
	var changed []string
	seen := map[string]bool{}
	for k, v := range m.prompts {
		seen[k] = true
		if old[k] != v.template {
			changed = append(changed, k)
		}
	}
	for k := range old {
		if !seen[k] {
			changed = append(changed, k)
		}
	}
	observers := append([]func([]string){}, m.reloadObservers...)
	m.mu.RUnlock()
	for _, fn := range observers {
		fn(changed)
	}
}

// RenderPrompt 对齐 PromptManager.render_prompt + _render：
//   - 先把 {{ }} 换成私有占位符（对齐 __post_init__ / _render 第一步）
//   - 解析 {var} 字段：嵌套 prompt > 模板内部上下文 > 全局构造函数 > 报错
//   - 渲染完成后把私有占位符还原为字面 { }
func (m *Manager) RenderPrompt(tpl *Template) (string, error) {
	return m.render(tpl, 0, map[string]RenderFunc{})
}

func (m *Manager) render(tpl *Template, depth int, additional map[string]RenderFunc) (string, error) {
	if depth > 10 {
		return "", fmt.Errorf("递归层级过深，可能存在循环引用")
	}
	text := strings.ReplaceAll(tpl.template, "{{", LeftBraceRepl)
	text = strings.ReplaceAll(text, "}}", RightBraceRepl)

	fields := extractFieldNames(text)
	rendered := make(map[string]string, len(fields))
	for _, fieldName := range fields {
		switch {
		case m.hasPrompt(fieldName):
			nested, err := m.GetPrompt(fieldName)
			if err != nil {
				return "", err
			}
			merged := mergeContexts(additional, tpl.contextsSnapshot())
			out, err := m.render(nested, depth+1, merged)
			if err != nil {
				return "", err
			}
			rendered[fieldName] = out
		case hasContext(tpl.contextsSnapshot(), fieldName):
			fn := tpl.contextsSnapshot()[fieldName]
			out, err := invokeRenderFunc(fn, tpl.Name, fieldName, true)
			if err != nil {
				return "", err
			}
			rendered[fieldName] = out
		case m.hasGlobalConstructor(fieldName):
			fn := m.globalConstructor(fieldName)
			out, err := invokeRenderFunc(fn, tpl.Name, fieldName, false)
			if err != nil {
				return "", err
			}
			rendered[fieldName] = out
		case hasContext(additional, fieldName):
			fn := additional[fieldName]
			out, err := invokeRenderFunc(fn, tpl.Name, fieldName, true)
			if err != nil {
				return "", err
			}
			rendered[fieldName] = out
		default:
			return "", fmt.Errorf("Prompt '%s' 中缺少必要的内容块或构建函数: '%s'", tpl.Name, fieldName)
		}
	}
	out := replaceFields(text, rendered)
	return strings.NewReplacer(LeftBraceRepl, "{", RightBraceRepl, "}").Replace(out), nil
}

// RenderString 渲染一段非托管模板文本（用户内容中的 { } 由 0xFDE9/0xFDEA 保护再还原）。
// 对齐「用户内容中的 { / } 在渲染前用私有占位符保护再还原」的语义：
// 用户文本里的 { } 不会参与 {var} 解析。
func RenderString(userText string) string {
	return strings.NewReplacer("{", LeftBraceRepl, "}", RightBraceRepl).Replace(userText)
}

// RestoreString 把渲染文本中的私有占位符还原为字面 { }。
func RestoreString(s string) string {
	return strings.NewReplacer(LeftBraceRepl, "{", RightBraceRepl, "}").Replace(s)
}

// ---------------------------------------------------------------------------
// {var} 解析与替换 — 对齐 string.Formatter.parse + format(**fields)
// ---------------------------------------------------------------------------

// fieldPattern 匹配 {name}（不含格式说明符的简单形态，MaiBot 模板均为此形态）。
var fieldPattern = regexp.MustCompile(`\{([^{}]+)\}`)

// extractFieldNames 对齐 _formatter.parse 收集 field_name 的去重集合（保持出现顺序）。
func extractFieldNames(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, match := range fieldPattern.FindAllStringSubmatch(text, -1) {
		name := match[1]
		// Python Formatter 会在 '.'/ '[' 处切分属性访问；MaiBot 模板未用到，
		// 但为对齐语义按第一个 '.' 截断。
		if idx := strings.IndexAny(name, ".["); idx >= 0 {
			name = name[:idx]
		}
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// replaceFields 对齐 str.format(**fields)：只替换已知字段，未命中的保持原样
// （与 Python 不同处：Python 对未知字段抛 KeyError；本函数面向已解析字段集，
// 未知字段在 extract 阶段已报错，不会走到这里）。
func replaceFields(text string, fields map[string]string) string {
	return fieldPattern.ReplaceAllStringFunc(text, func(match string) string {
		name := match[1 : len(match)-1]
		if idx := strings.IndexAny(name, ".["); idx >= 0 {
			name = name[:idx]
		}
		if v, ok := fields[name]; ok {
			return v
		}
		return match
	})
}

func (m *Manager) hasPrompt(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.prompts[name]
	return ok
}

func (m *Manager) hasGlobalConstructor(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.contextConstructors[name]
	return ok
}

func (m *Manager) globalConstructor(name string) RenderFunc {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.contextConstructors[name]
}

func contextsSnapshot(contexts map[string]RenderFunc) map[string]RenderFunc {
	if len(contexts) == 0 {
		return map[string]RenderFunc{}
	}
	out := make(map[string]RenderFunc, len(contexts))
	for k, v := range contexts {
		out[k] = v
	}
	return out
}

// mergeContexts 对齐 `additional | prompt.prompt_render_context`：模板内部优先。
func mergeContexts(additional, inner map[string]RenderFunc) map[string]RenderFunc {
	merged := make(map[string]RenderFunc, len(additional)+len(inner))
	for k, v := range additional {
		merged[k] = v
	}
	for k, v := range inner {
		merged[k] = v
	}
	return merged
}

func hasContext(contexts map[string]RenderFunc, name string) bool {
	_, ok := contexts[name]
	return ok
}

// invokeRenderFunc 对齐 _get_function_result（错误包装与 panic 保护）。
func invokeRenderFunc(fn RenderFunc, promptName, fieldName string, isPromptContext bool) (out string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("调用 Prompt '%s' 上下文构造函数 '%s' 时 panic: %v", promptName, fieldName, r)
		}
	}()
	out, err = fn(promptName)
	if err != nil {
		if isPromptContext {
			return "", fmt.Errorf("调用 Prompt '%s' 内部上下文构造函数 '%s' 时出错: %w", promptName, fieldName, err)
		}
		return "", fmt.Errorf("调用上下文构造函数 '%s' 时出错: %w", fieldName, err)
	}
	return out, nil
}
