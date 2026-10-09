package context

import (
	"fmt"
	"strconv"
	"strings"

	"oh-my-qqbot/internal/message"
)

// This file mirrors the forward-message rendering helpers of
// 参考/MaiBot/src/maisaka/context/messages.py:
// `contains_complex_message`, `_build_complex_message_prompt_text`,
// `_render_component_for_prompt`, `_build_forward_preview_block`,
// `_render_components_inline`, `_normalize_inline_text` and the
// `view_forward_message` path expansion helpers.

// ContainsComplexMessage mirrors
// messages.contains_complex_message: true when the sequence carries a
// ForwardNodeComponent that must be expanded through a tool.
func ContainsComplexMessage(sequence *message.MessageSequence) bool {
	if sequence == nil {
		return false
	}
	for _, component := range sequence.Components {
		if _, ok := component.(*message.ForwardNodeComponent); ok {
			return true
		}
	}
	return false
}

// BuildComplexMessagePromptText mirrors `_build_complex_message_prompt_text`:
// it renders each component of the sequence into prompt text and joins the
// non-empty parts with newlines.
func BuildComplexMessagePromptText(sequence *message.MessageSequence) string {
	if sequence == nil {
		return ""
	}
	parts := make([]string, 0, len(sequence.Components))
	for _, component := range sequence.Components {
		rendered := RenderComponentForPrompt(component)
		if rendered != "" {
			parts = append(parts, rendered)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

// BuildFullComplexMessageContent mirrors
// `build_full_complex_message_content_from_sequence`: it returns the text of the
// requested forward path, or the top-level forward preview when path is empty.
func BuildFullComplexMessageContent(sequence *message.MessageSequence, path []int) (string, error) {
	return buildComplexMessageFullText(sequence, path)
}

// RenderComponentForPrompt mirrors `_render_component_for_prompt`.
func RenderComponentForPrompt(component message.Component) string {
	switch typed := component.(type) {
	case *message.TextComponent:
		return strings.TrimSpace(typed.Text)
	case *message.ImageComponent:
		if strings.TrimSpace(typed.Content) != "" {
			return strings.TrimSpace(typed.Content)
		}
		return ImageVisualPlaceholder
	case *message.EmojiComponent:
		if strings.TrimSpace(typed.Content) != "" {
			return strings.TrimSpace(typed.Content)
		}
		return EmojiFallbackText
	case *message.VoiceComponent:
		if strings.TrimSpace(typed.Content) != "" {
			return strings.TrimSpace(typed.Content)
		}
		return VoiceFallbackText
	case *message.FileComponent:
		return typed.ToPlainText()
	case *message.AtComponent:
		return RenderAtComponentText(typed)
	case *message.ReplyComponent:
		return ""
	case *message.ForwardNodeComponent:
		return BuildForwardPreviewBlock(typed)
	case *message.DictComponent:
		if rawType, ok := typed.Data["type"].(string); ok && strings.TrimSpace(rawType) != "" {
			return "[" + strings.TrimSpace(rawType) + "消息]"
		}
		return "[非标准消息]"
	default:
		_ = typed
		return ""
	}
}

// BuildForwardPreviewBlock mirrors `_build_forward_preview_block`.
func BuildForwardPreviewBlock(component *message.ForwardNodeComponent) string {
	if component == nil {
		return ""
	}
	lines := []string{"[消息类型]转发消息", fmt.Sprintf("预览前%d条：", ForwardPreviewLimit)}
	previewNodes := component.ForwardComponents
	if len(previewNodes) > ForwardPreviewLimit {
		previewNodes = previewNodes[:ForwardPreviewLimit]
	}
	for _, node := range previewNodes {
		content := RenderComponentsInline(node.Content)
		if content == "" {
			content = "[空消息]"
		}
		lines = append(lines, node.DisplayName()+"："+content)
	}
	totalCount := len(component.ForwardComponents)
	if totalCount > ForwardPreviewLimit {
		lines = append(lines, "......")
		lines = append(lines, fmt.Sprintf("共%d条，可以使用 view_forward_message 查看完整转发内容。", totalCount))
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// RenderComponentsInline mirrors `_render_components_inline`.
func RenderComponentsInline(components []message.Component) string {
	parts := make([]string, 0, len(components))
	for _, component := range components {
		if _, ok := component.(*message.ForwardNodeComponent); ok {
			parts = append(parts, "[转发消息]")
			continue
		}
		normalized := NormalizeInlineText(RenderComponentForPrompt(component))
		if normalized != "" {
			parts = append(parts, normalized)
		}
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}

// NormalizeInlineText mirrors `_normalize_inline_text`.
func NormalizeInlineText(text string) string {
	return strings.TrimSpace(strings.Join(strings.Fields(text), " "))
}

// ---------------------------------------------------------------------------
// view_forward_message path expansion
// ---------------------------------------------------------------------------

func buildComplexMessageFullText(sequence *message.MessageSequence, path []int) (string, error) {
	if sequence == nil {
		return "", nil
	}
	rootComponents := make([]*message.ForwardNodeComponent, 0, len(sequence.Components))
	for _, component := range sequence.Components {
		if node, ok := component.(*message.ForwardNodeComponent); ok {
			rootComponents = append(rootComponents, node)
		}
	}
	if len(path) > 0 {
		target, err := resolveForwardComponent(rootComponents, path)
		if err != nil {
			return "", err
		}
		return buildForwardFullText(target, path), nil
	}

	lines := make([]string, 0, len(rootComponents))
	for index, component := range rootComponents {
		lines = append(lines, buildForwardFullText(component, []int{index}))
	}
	return strings.TrimSpace(strings.Join(lines, "\n")), nil
}

// resolveForwardComponent mirrors `_resolve_forward_component`.
func resolveForwardComponent(rootComponents []*message.ForwardNodeComponent, path []int) (*message.ForwardNodeComponent, error) {
	currentComponents := rootComponents
	var currentComponent *message.ForwardNodeComponent
	for depth, componentIndex := range path {
		if componentIndex < 0 || componentIndex >= len(currentComponents) {
			return nil, fmt.Errorf("转发消息路径无效：第 %d 级索引 %d 超出范围。", depth+1, componentIndex)
		}
		currentComponent = currentComponents[componentIndex]
		currentComponents = collectNestedForwardComponents(currentComponent)
	}
	if currentComponent == nil {
		return nil, fmt.Errorf("转发消息路径不能为空。")
	}
	return currentComponent, nil
}

func collectNestedForwardComponents(component *message.ForwardNodeComponent) []*message.ForwardNodeComponent {
	nested := make([]*message.ForwardNodeComponent, 0)
	if component == nil {
		return nested
	}
	for _, node := range component.ForwardComponents {
		for _, item := range node.Content {
			if nestedComponent, ok := item.(*message.ForwardNodeComponent); ok {
				nested = append(nested, nestedComponent)
			}
		}
	}
	return nested
}

func buildForwardFullText(component *message.ForwardNodeComponent, path []int) string {
	lines := []string{"【合并转发消息:"}
	nestedComponentIndex := 0
	for _, node := range component.ForwardComponents {
		content, nextIndex := renderComponentsForBrowser(node.Content, path, nestedComponentIndex)
		nestedComponentIndex = nextIndex
		if content == "" {
			content = "[空消息]"
		}
		lines = append(lines, fmt.Sprintf("【%s】: %s", node.DisplayName(), content))
	}
	lines = append(lines, "】")
	return strings.Join(lines, "\n")
}

func renderComponentsForBrowser(components []message.Component, parentPath []int, nestedComponentIndex int) (string, int) {
	parts := make([]string, 0, len(components))
	for _, component := range components {
		if _, ok := component.(*message.ForwardNodeComponent); ok {
			nestedPath := append(append([]int{}, parentPath...), nestedComponentIndex)
			parts = append(parts, fmt.Sprintf("[嵌套转发消息，path=%s，可再次调用 view_forward_message 展开]", FormatPath(nestedPath)))
			nestedComponentIndex++
			continue
		}
		normalized := NormalizeInlineText(RenderComponentForPrompt(component))
		if normalized != "" {
			parts = append(parts, normalized)
		}
	}
	return strings.TrimSpace(strings.Join(parts, " ")), nestedComponentIndex
}

// FormatPath mirrors the Python list repr used in the nested-forward hint.
func FormatPath(path []int) string {
	items := make([]string, 0, len(path))
	for _, value := range path {
		items = append(items, strconv.Itoa(value))
	}
	return "[" + strings.Join(items, ", ") + "]"
}
