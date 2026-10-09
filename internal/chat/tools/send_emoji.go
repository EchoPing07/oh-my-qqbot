package tools

import (
	"fmt"
)

// This file mirrors 参考/MaiBot/src/maisaka/builtin_tool/send_emoji.py +
// show_emoji_list.py (behaviour is authoritative there): 表情包工具骨架。
//
// P4b 只定义工具声明与执行骨架；emoji 拼图合成（x/image）与 EmojiManager
// 属于 internal/emoji（P9 表情包工作包），通过 EmojiBoundary 留桩注入。

// EmojiSender 是 send_emoji 的最小发送边界（internal/emoji 后续实现）。
type EmojiSender interface {
	// SendEmojiByEmotion 按情绪标签相似度选择并发送一个表情包
	//（旧逻辑；返回 (描述, 命中情绪, 错误)）。
	SendEmojiByEmotion(ctx contextT, sessionID string, requestedEmotion string, reasoning string) (description string, matchedEmotion string, err error)
}

// EmojiLister 是 show_emoji_list 的最小拼图边界（internal/emoji 后续实现）。
type EmojiLister interface {
	// BuildCollage 返回一次拼图快照：(PNG 字节, 序号->hash 表, 起始序号)。
	BuildCollage(ctx contextT) (pngBytes []byte, indexToHash map[int]string, startIndex int, err error)
}

// emoji success/failure 文案（照抄 send_emoji.py 常量）。
const (
	emojiSuccessMessage          = "表情包发送成功"
	emojiVLMNotConfiguredMessage = "错误，没有配置视觉模型，无法使用表情包功能"
)

// GetSendEmojiToolSpec mirrors get_tool_spec（声明照抄）。
func GetSendEmojiToolSpec() ToolSpec {
	return ToolSpec{
		Name:        "send_emoji",
		Description: "发送一个表情包来表达情绪，参与聊天。",
		ParametersSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		ProviderName: "maisaka_builtin",
		ProviderType: "builtin",
		Enabled:      true,
		Stage:        StageAction,
		Visibility:   VisibilityVisible,
	}
}

// GetShowEmojiListToolSpec mirrors get_tool_spec（声明照抄）。
func GetShowEmojiListToolSpec() ToolSpec {
	return ToolSpec{
		Name: "show_emoji_list",
		Description: "查看随机抽取的表情包选择拼图。图片会进入你的上下文，不会发送给聊天对象。" +
			"调用 reply 时将选中图片的序号填入 attach_emoji；历史拼图的序号也可使用。",
		ParametersSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		ProviderName: "maisaka_builtin",
		ProviderType: "builtin",
		Enabled:      true,
		Stage:        StageAction,
		Visibility:   VisibilityVisible,
	}
}

// HandleSendEmojiTool mirrors handle_tool（send_emoji 骨架：委托注入的 EmojiSender；
// 未注入时回执 VLM 未配置文案）。
func HandleSendEmojiTool(ctx contextT, sender EmojiSender, sessionID string, reasoning string, invocation ToolInvocation) ToolResult {
	toolName := invocation.ToolName
	structured := map[string]any{
		"success":         false,
		"message":         "",
		"description":     "",
		"emotion":         []string{},
		"matched_emotion": "",
		"reason":          "",
	}
	requestedEmotion := toString(invocation.Arguments["emotion"])

	if sender == nil {
		structured["message"] = emojiVLMNotConfiguredMessage
		return BuildFailureResult(toolName, emojiVLMNotConfiguredMessage, structured, nil)
	}

	description, matchedEmotion, err := sender.SendEmojiByEmotion(ctx, sessionID, requestedEmotion, reasoning)
	if err != nil {
		structured["message"] = fmt.Sprintf("发送表情包时发生异常：%v", err)
		return BuildFailureResult(toolName, structured["message"].(string), structured, nil)
	}
	structured["success"] = true
	structured["message"] = emojiSuccessMessage
	structured["description"] = description
	structured["matched_emotion"] = matchedEmotion
	return BuildSuccessResult(toolName, emojiSuccessMessage, structured, nil, nil)
}

// HandleShowEmojiListTool mirrors handle_tool（show_emoji_list 骨架：
// 拼图合成由注入的 EmojiLister 完成，返回 post_history 携带拼图 item）。
func HandleShowEmojiListTool(ctx contextT, lister EmojiLister, invocation ToolInvocation) ToolResult {
	toolName := invocation.ToolName
	if lister == nil {
		return BuildFailureResult(toolName, "当前表情包库中没有可用表情。", nil, nil)
	}
	pngBytes, indexToHash, startIndex, err := lister.BuildCollage(ctx)
	if err != nil {
		return BuildFailureResult(toolName, fmt.Sprintf("生成表情包拼图失败：%v", err), nil, nil)
	}
	endIndex := startIndex
	for index := range indexToHash {
		if index > endIndex {
			endIndex = index
		}
	}
	text := fmt.Sprintf("表情包选择图，序号 %d–%d。请在 reply.attach_emoji 中填写选中的整数序号；表情包将于文字回复后单独发送。",
		startIndex, endIndex)
	postHistory := []PostHistoryEntry{{
		ItemID:      fmt.Sprintf("emoji_candidates_%d", startIndex),
		VisibleText: text,
		PayloadJSON: pngBytes, // 拼图 PNG；装配层构造 ContextImagePart
	}}
	return BuildSuccessResult(toolName, text, map[string]any{
		"emoji_indices": indexToHash,
	}, nil, postHistory)
}
