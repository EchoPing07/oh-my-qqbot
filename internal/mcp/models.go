package mcp

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// This file mirrors 参考/MaiBot/src/mcp_module/models.py: MCP SDK 原始对象与
// 主程序内部数据结构之间的转换(内容块/注解/图标/工具结果)。

// ---------------------------------------------------------------------------
// 统一工具层数据结构（对齐 src/core/tooling.py 的 ToolIcon/ToolAnnotation/
// ToolContentItem，Go 侧同样落在本包，避免 import internal/chat/tools 的具体
// 实现细节造成循环依赖）
// ---------------------------------------------------------------------------

// ToolIcon mirrors ToolIcon。
type ToolIcon struct {
	Src      string
	MIMEType string
	Sizes    []string
}

// ToolAnnotation mirrors ToolAnnotation。
type ToolAnnotation struct {
	Audience    []string
	Priority    *float64
	Title       string
	ReadOnly    *bool
	Destructive *bool
	Idempotent  *bool
	OpenWorld   *bool
	Metadata    map[string]any
}

// ToolContentType mirrors ToolContentItem.content_type 的取值集合。
type ToolContentType string

// Content type values.
const (
	ContentText         ToolContentType = "text"
	ContentImage        ToolContentType = "image"
	ContentAudio        ToolContentType = "audio"
	ContentResourceLink ToolContentType = "resource_link"
	ContentResource     ToolContentType = "resource"
	ContentBinary       ToolContentType = "binary"
	ContentUnknown      ToolContentType = "unknown"
)

// ToolContentItem mirrors ToolContentItem。
type ToolContentItem struct {
	ContentType ToolContentType
	Text        string
	Data        string
	MIMEType    string
	URI         string
	Name        string
	Description string
	Annotation  *ToolAnnotation
	Metadata    map[string]any
}

// BuildHistoryText mirrors ToolContentItem.build_history_text（文案照抄）。
func (item ToolContentItem) BuildHistoryText() string {
	switch item.ContentType {
	case ContentText:
		return strings.TrimSpace(item.Text)
	case ContentImage:
		return "[图片内容 " + defaultString(item.MIMEType, "unknown") + "]"
	case ContentAudio:
		return "[音频内容 " + defaultString(item.MIMEType, "unknown") + "]"
	case ContentResourceLink:
		return "[资源链接] " + firstNonEmpty(item.Name, item.URI, "资源链接")
	case ContentResource:
		if trimmed := strings.TrimSpace(item.Text); trimmed != "" {
			return trimmed
		}
		return "[嵌入资源] " + firstNonEmpty(item.Name, item.URI, "嵌入资源")
	case ContentBinary:
		return "[二进制内容 " + defaultString(item.MIMEType, "unknown") + "]"
	default:
		return "[" + string(item.ContentType) + " 内容]"
	}
}

// ---------------------------------------------------------------------------
// 元数据与图标/注解转换
// ---------------------------------------------------------------------------

func metaToMap(meta mcp.Meta) map[string]any {
	if len(meta) == 0 {
		return map[string]any{}
	}
	dump := make(map[string]any, len(meta))
	for key, value := range meta {
		dump[key] = value
	}
	return dump
}


// BuildToolIcon mirrors build_tool_icon。
func BuildToolIcon(raw mcp.Icon) ToolIcon {
	sizes := make([]string, 0, len(raw.Sizes))
	for _, size := range raw.Sizes {
		sizes = append(sizes, size)
	}
	return ToolIcon{
		Src:      raw.Source,
		MIMEType: raw.MIMEType,
		Sizes:    sizes,
	}
}

// BuildToolIcons mirrors build_tool_content_items 的图标分支。
func BuildToolIcons(rawIcons []mcp.Icon) []ToolIcon {
	if len(rawIcons) == 0 {
		return nil
	}
	icons := make([]ToolIcon, 0, len(rawIcons))
	for _, raw := range rawIcons {
		icons = append(icons, BuildToolIcon(raw))
	}
	return icons
}

// BuildToolAnnotation mirrors build_tool_annotation: 无有效内容时返回 nil。
func BuildToolAnnotation(raw *mcp.ToolAnnotations) *ToolAnnotation {
	if raw == nil {
		return nil
	}
	annotation := &ToolAnnotation{
		Title:      strings.TrimSpace(raw.Title),
		ReadOnly:   boolPtr(raw.ReadOnlyHint),
		Idempotent: boolPtr(raw.IdempotentHint),
		Metadata:   map[string]any{},
	}
	if raw.DestructiveHint != nil {
		annotation.Destructive = boolPtr(*raw.DestructiveHint)
	}
	if raw.OpenWorldHint != nil {
		annotation.OpenWorld = boolPtr(*raw.OpenWorldHint)
	}
	return annotation
}

// BuildContentAnnotation mirrors build_tool_annotation 作用于内容块注解的分支。
func BuildContentAnnotation(raw *mcp.Annotations) *ToolAnnotation {
	if raw == nil {
		return nil
	}
	annotation := &ToolAnnotation{Metadata: map[string]any{}}
	for _, role := range raw.Audience {
		annotation.Audience = append(annotation.Audience, string(role))
	}
	if raw.Priority != 0 {
		priority := raw.Priority
		annotation.Priority = &priority
	}
	if len(annotation.Audience) == 0 && annotation.Priority == nil {
		return nil
	}
	return annotation
}

func boolPtr(value bool) *bool {
	copied := value
	return &copied
}

// ---------------------------------------------------------------------------
// 内容块转换（对齐 build_tool_content_item / build_tool_content_items）
// ---------------------------------------------------------------------------

// BuildToolContentItem mirrors build_tool_content_item。
func BuildToolContentItem(raw mcp.Content) ToolContentItem {
	switch content := raw.(type) {
	case *mcp.TextContent:
		return ToolContentItem{
			ContentType: ContentText,
			Text:        content.Text,
			Annotation:  BuildContentAnnotation(content.Annotations),
			Metadata:    metaToMap(content.Meta),
		}
	case *mcp.ImageContent:
		return ToolContentItem{
			ContentType: ContentImage,
			Data:        string(content.Data),
			MIMEType:    content.MIMEType,
			Annotation:  BuildContentAnnotation(content.Annotations),
			Metadata:    metaToMap(content.Meta),
		}
	case *mcp.AudioContent:
		return ToolContentItem{
			ContentType: ContentAudio,
			Data:        string(content.Data),
			MIMEType:    content.MIMEType,
			Annotation:  BuildContentAnnotation(content.Annotations),
			Metadata:    metaToMap(content.Meta),
		}
	case *mcp.ResourceLink:
		return ToolContentItem{
			ContentType: ContentResourceLink,
			URI:         content.URI,
			Name:        content.Name,
			Description: content.Description,
			MIMEType:    content.MIMEType,
			Annotation:  BuildContentAnnotation(content.Annotations),
			Metadata:    metaToMap(content.Meta),
		}
	case *mcp.EmbeddedResource:
		item := ToolContentItem{
			ContentType: ContentResource,
			Annotation:  BuildContentAnnotation(content.Annotations),
			Metadata:    metaToMap(content.Meta),
		}
		if content.Resource != nil {
			item.URI = content.Resource.URI
			item.MIMEType = content.Resource.MIMEType
			item.Text = string(content.Resource.Text)
			item.Data = string(content.Resource.Blob)
			for key, value := range metaToMap(content.Resource.Meta) {
				item.Metadata[key] = value
			}
		}
		return item
	case *mcp.ToolUseContent:
		encoded, err := json.Marshal(content.Input)
		if err != nil {
			encoded = []byte("{}")
		}
		return ToolContentItem{
			ContentType: ContentText,
			Text:        "[工具调用] " + content.Name + " " + string(encoded),
			Metadata:    metaToMap(content.Meta),
		}
	case *mcp.ToolResultContent:
		return ToolContentItem{
			ContentType: ContentText,
			Text:        SummarizeToolResultContent(content),
			Metadata:    metaToMap(content.Meta),
		}
	default:
		return ToolContentItem{
			ContentType: ContentUnknown,
			Text:        "",
			Metadata:    map[string]any{},
		}
	}
}

// BuildToolContentItems mirrors build_tool_content_items。
func BuildToolContentItems(rawContents []mcp.Content) []ToolContentItem {
	if len(rawContents) == 0 {
		return nil
	}
	items := make([]ToolContentItem, 0, len(rawContents))
	for _, raw := range rawContents {
		items = append(items, BuildToolContentItem(raw))
	}
	return items
}

// SummarizeToolResultContent mirrors MCPHostLLMBridge._summarize_tool_result_content:
// 汇总 tool_result 内容块的文本,并追加 structuredContent 的 JSON 表示。
func SummarizeToolResultContent(content *mcp.ToolResultContent) string {
	if content == nil {
		return ""
	}
	parts := make([]string, 0, len(content.Content)+1)
	for _, item := range BuildToolContentItems(content.Content) {
		if text := strings.TrimSpace(item.BuildHistoryText()); text != "" {
			parts = append(parts, text)
		}
	}
	if content.StructuredContent != nil {
		if encoded, err := json.Marshal(content.StructuredContent); err == nil {
			parts = append(parts, string(encoded))
		} else {
			parts = append(parts, stringify(content.StructuredContent))
		}
	}
	summary := strings.TrimSpace(strings.Join(parts, "\n"))
	if content.IsError {
		if summary != "" {
			return "工具执行失败：\n" + summary
		}
		return "工具执行失败。"
	}
	return summary
}

// JoinTextParts mirrors connection.py 中 `"\n".join(text_parts).strip()`
// 的文本拼接语义。
func JoinTextParts(items []ToolContentItem) string {
	parts := make([]string, 0, len(items))
	for _, item := range items {
		if item.ContentType != ContentText {
			continue
		}
		if trimmed := strings.TrimSpace(item.Text); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

// HistoryText mirrors ToolExecutionResult.get_history_content: 优先文本内容,
// 其次内容块摘要,再次 structured_content,最后错误信息。
func HistoryText(content string, items []ToolContentItem, structured any, errorMessage string) string {
	if trimmed := strings.TrimSpace(content); trimmed != "" {
		return trimmed
	}
	parts := make([]string, 0, len(items))
	for _, item := range items {
		if text := strings.TrimSpace(item.BuildHistoryText()); text != "" {
			parts = append(parts, text)
		}
	}
	if joined := strings.TrimSpace(strings.Join(parts, "\n")); joined != "" {
		return joined
	}
	if structured != nil {
		if text, ok := structured.(string); ok {
			return strings.TrimSpace(text)
		}
		if encoded, err := json.Marshal(structured); err == nil {
			return string(encoded)
		}
		return strings.TrimSpace(stringify(structured))
	}
	return strings.TrimSpace(errorMessage)
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func defaultString(value string, fallback string) string {
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		return trimmed
	}
	return fallback
}

func stringify(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case json.Number:
		return typed.String()
	case bool:
		return strconv.FormatBool(typed)
	case float64:
		return strconv.FormatFloat(typed, 'g', -1, 64)
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	default:
		if encoded, err := json.Marshal(typed); err == nil {
			return string(encoded)
		}
		return ""
	}
}
