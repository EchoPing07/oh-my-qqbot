package prompt

// TemplateText 以只读方式返回模板原文（跨包组装 replyer 系统提示词用）。
// 只添加访问器，不改变 loader.go 的任何现有行为。
func TemplateText(tpl *Template) string {
	if tpl == nil {
		return ""
	}
	tpl.mu.Lock()
	defer tpl.mu.Unlock()
	return tpl.template
}
