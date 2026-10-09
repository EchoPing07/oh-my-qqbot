package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExtractFieldNames 校验 {var} 字段收集：去重、保序、属性访问截断。
func TestExtractFieldNames(t *testing.T) {
	got := extractFieldNames("你好 {name}，{item} 与 {name} 以及 {user.name}")
	want := []string{"name", "item", "user"}
	if len(got) != len(want) {
		t.Fatalf("字段数不一致: got=%v want=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("字段[%d] = %q, want %q（应保序且去重）", i, got[i], want[i])
		}
	}
	// {{ }} 已保护后不应被识别为字段。
	if got := extractFieldNames(strings.ReplaceAll("{{not_a_field}}", "{{", LeftBraceRepl)); len(got) != 0 {
		t.Errorf("保护后的 {{ }} 不应被识别为字段: %v", got)
	}
}

// TestReplaceFields 校验已知字段替换、未知字段保持原样。
func TestReplaceFields(t *testing.T) {
	out := replaceFields("{a} {b} {c}", map[string]string{"a": "1", "b": "2"})
	if out != "1 2 {c}" {
		t.Errorf("replaceFields = %q, want %q", out, "1 2 {c}")
	}
}

// TestRenderStringRestoreString 校验 0xFDE9/0xFDEA 花括号保护与还原。
func TestRenderStringRestoreString(t *testing.T) {
	if LeftBraceRepl != "\uFDE9" || RightBraceRepl != "\uFDEA" {
		t.Fatalf("占位符常量应与 MaiBot 一致: %q / %q", LeftBraceRepl, RightBraceRepl)
	}
	src := "用户内容 {with} 花括号 {{and}} }"
	protected := RenderString(src)
	// 用户文本中的花括号不能以裸 { } 形式存在（不参与 {var} 解析）。
	if strings.ContainsAny(protected, "{}") {
		t.Errorf("保护后不应残留裸花括号: %q", protected)
	}
	if restored := RestoreString(protected); restored != src {
		t.Errorf("还原应无损: got %q, want %q", restored, src)
	}
}

// newTestManager 在临时目录构造内置+自定义两层模板。
func newTestManager(t *testing.T) (*Manager, string) {
	t.Helper()
	root := t.TempDir()
	localeDir := filepath.Join(root, "prompts", DefaultLocale)
	if err := os.MkdirAll(localeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile := func(path, content string) {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// 主模板：{var} 替换 + {{ }} 字面花括号 + 嵌套引用。
	writeFile(filepath.Join(localeDir, "main.prompt"),
		"名字={name} 字面={{字面}} 嵌套={sub}\n")
	// 嵌套子模板。
	writeFile(filepath.Join(localeDir, "sub.prompt"), "[子模板:{detail}]\n")

	customRoot := filepath.Join(root, "data", "custom_prompts")
	m := NewManager(filepath.Join(root, "prompts"), customRoot, DefaultLocale)
	return m, customRoot
}

// TestManagerRenderVarAndBraceProtection 端到端：{var} 替换、{{ }} 还原为字面 { }、嵌套渲染。
func TestManagerRenderVarAndBraceProtection(t *testing.T) {
	m, _ := newTestManager(t)
	if err := m.LoadPrompts(); err != nil {
		t.Fatalf("LoadPrompts 失败: %v", err)
	}
	tpl, err := m.GetPrompt("main")
	if err != nil {
		t.Fatalf("GetPrompt 失败: %v", err)
	}
	if err := tpl.AddContext("name", "麦麦"); err != nil {
		t.Fatal(err)
	}
	if err := tpl.AddContext("name", "重复"); err == nil {
		t.Error("重复注册上下文应报错")
	}
	// main 的嵌套引用 {sub} 会递归渲染，sub 的 detail 走全局构造函数。
	if err := m.AddContextConstructor("detail", func(string) (string, error) { return "细节", nil }); err != nil {
		t.Fatal(err)
	}
	out, err := m.RenderPrompt(tpl)
	if err != nil {
		t.Fatalf("RenderPrompt 失败: %v", err)
	}
	// {{字面}} → {字面}（0xFDE9/0xFDEA 还原）；{name} 已替换；{sub} 走嵌套渲染。
	if !strings.Contains(out, "名字=麦麦") {
		t.Errorf("{var} 未替换: %q", out)
	}
	if !strings.Contains(out, "字面={字面}") {
		t.Errorf("{{ }} 应还原为字面花括号: %q", out)
	}
}

// TestManagerNestedAndMissingContext 校验嵌套引用与缺失上下文报错。
func TestManagerNestedAndMissingContext(t *testing.T) {
	m, _ := newTestManager(t)
	if err := m.LoadPrompts(); err != nil {
		t.Fatal(err)
	}
	// 缺失上下文 → 报错。
	tpl, err := m.GetPrompt("sub")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.RenderPrompt(tpl); err == nil {
		t.Error("缺失上下文 'detail' 应报错")
	}
	// 通过全局构造函数补齐后，main 的嵌套引用 {sub} 可完整渲染。
	if err := m.AddContextConstructor("detail", func(string) (string, error) { return "细节", nil }); err != nil {
		t.Fatal(err)
	}
	if err := m.AddContextConstructor("detail", func(string) (string, error) { return "x", nil }); err == nil {
		t.Error("重复注册全局构造函数应报错")
	}
	main, err := m.GetPrompt("main")
	if err != nil {
		t.Fatal(err)
	}
	if err := main.AddContext("name", "麦麦"); err != nil {
		t.Fatal(err)
	}
	out, err := m.RenderPrompt(main)
	if err != nil {
		t.Fatalf("嵌套渲染失败: %v", err)
	}
	if !strings.Contains(out, "[子模板:细节]") {
		t.Errorf("嵌套 prompt 应递归渲染: %q", out)
	}
}

// TestLoadPromptsRejectsBareBraces 校验裸 {} 模板被 validateTemplate 拒绝。
func TestLoadPromptsRejectsBareBraces(t *testing.T) {
	m, _ := newTestManager(t)
	// 追加一个含裸 {} 的非法模板（LoadPrompts 会校验目录内全部文件）。
	badPath := filepath.Join(m.promptsRoot, DefaultLocale, "bad.prompt")
	if err := os.WriteFile(badPath, []byte("占位符 {} 非法\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.LoadPrompts(); err == nil {
		t.Error("含裸 {} 的模板应导致 LoadPrompts 报错")
	}
	if err := validateTemplate("x", ""); err == nil {
		t.Error("空模板应报错")
	}
	if err := validateTemplate("", "内容"); err == nil {
		t.Error("空名称应报错")
	}
	if err := validateTemplate("x", "{{ok}} {name}"); err != nil {
		t.Errorf("合法模板不应报错: %v", err)
	}
}

// TestNormalizePromptName 校验名称规范化与非法名称拒绝。
func TestNormalizePromptName(t *testing.T) {
	if got, err := NormalizePromptName("main.prompt"); err != nil || got != "main" {
		t.Errorf("NormalizePromptName = %q, %v", got, err)
	}
	for _, bad := range []string{"", ".", "..", "a/b", "a b"} {
		if _, err := NormalizePromptName(bad); err == nil {
			t.Errorf("非法名称 %q 应报错", bad)
		}
	}
}

// TestDiscoverAndCustomOverride 校验语言目录发现与 custom_prompts 覆盖。
func TestDiscoverAndCustomOverride(t *testing.T) {
	m, customRoot := newTestManager(t)
	if err := m.LoadPrompts(); err != nil {
		t.Fatal(err)
	}
	// custom 层覆盖内置 sub。
	subDir := filepath.Join(customRoot, DefaultLocale)
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "sub.prompt"), []byte("[覆盖:{detail}]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.Reload()
	tpl, err := m.GetPrompt("sub")
	if err != nil {
		t.Fatal(err)
	}
	// 同包测试可直接读私有字段，验证 custom 层确实覆盖了内置模板。
	if tpl.template != "[覆盖:{detail}]\n" {
		t.Errorf("custom_prompts 应覆盖内置模板，实际: %q", tpl.template)
	}
}
