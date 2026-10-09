package tools

import (
	"strings"
)

// This file mirrors the builtin tool catalogue of
// 参考/MaiBot/src/maisaka/builtin_tool/__init__.py: BUILTIN_TOOL_ENTRIES 的
// Go 形态（插件/switch_chat/query_person_profile/query_image_memory 属后续工作包）。

// FeatureFlags mirrors the config switches that gate builtin tool visibility
// (_is_builtin_tool_enabled_by_config)。由装配层填充；本包不 import config。
type FeatureFlags struct {
	// EnableMemoryQueryTool mirrors a_memorix.integration.enable_memory_query_tool.
	EnableMemoryQueryTool bool
	// MemoryQueryDefaultLimit mirrors integration.memory_query_default_limit.
	MemoryQueryDefaultLimit int
	// UseNewEmojiSendLogic mirrors emoji.use_new_send_logic
	// (show_emoji_list 启用 / send_emoji 关闭)。
	UseNewEmojiSendLogic bool
	// FocusMode mirrors experimental.focus_mode（fetch_history 可见性）。
	FocusMode bool
	// UseVectorExpression mirrors expression.use_vector_expression.
	UseVectorExpression bool
	// EnableReplyAt mirrors chat.enable_reply_at.
	EnableReplyAt bool
	// EnableReplyQuote mirrors chat.reply_style.enable_reply_quote.
	EnableReplyQuote bool
	// BotNickname mirrors bot.nickname.
	BotNickname string
}

// BuiltinEntries returns the builtin tool entries with the config-driven
// visibility gates wired in (顺序照抄 BUILTIN_TOOL_ENTRIES)。
func BuiltinEntries(flags FeatureFlags) []BuiltinToolEntry {
	replyDeps := &ReplyDependencies{}
	replyDeps.BotNameFn = func() string { return flags.BotNickname }
	return []BuiltinToolEntry{
		{
			Name:            "wait",
			GetSpec:         GetWaitToolSpec,
			Stage:           StageBoth,
			Visibility:      VisibilityVisible,
			EnabledByConfig: func() bool { return true },
		},
		{
			Name: "reply",
			GetSpec: func() ToolSpec {
				return GetReplyToolSpec(replyDeps, flags.UseNewEmojiSendLogic, flags.UseVectorExpression)
			},
			Stage:           StageAction,
			Visibility:      VisibilityVisible,
			EnabledByConfig: func() bool { return true },
		},
		{
			Name:            "view_forward_message",
			GetSpec:         GetViewForwardMessageToolSpec,
			Stage:           StageAction,
			Visibility:      VisibilityDeferred,
			EnabledByConfig: func() bool { return true },
		},
		{
			Name:            "query_memory",
			GetSpec:         func() ToolSpec { return GetQueryMemoryToolSpec(flags.EnableMemoryQueryTool) },
			Stage:           StageAction,
			Visibility:      VisibilityVisible,
			EnabledByConfig: func() bool { return true },
		},
		{
			Name:       "send_emoji",
			GetSpec:    GetSendEmojiToolSpec,
			Stage:      StageAction,
			Visibility: VisibilityVisible,
			// send_emoji: 未开启新发送逻辑时可用（照抄取反分支）。
			EnabledByConfig: func() bool { return !flags.UseNewEmojiSendLogic },
		},
		{
			Name:       "show_emoji_list",
			GetSpec:    GetShowEmojiListToolSpec,
			Stage:      StageAction,
			Visibility: VisibilityVisible,
			// show_emoji_list: 仅新发送逻辑开启时可用。
			EnabledByConfig: func() bool { return flags.UseNewEmojiSendLogic },
		},
		{
			Name:            "tool_search",
			GetSpec:         GetToolSearchToolSpec,
			Stage:           StageAction,
			Visibility:      VisibilityVisible,
			EnabledByConfig: func() bool { return true },
		},
		{
			Name:       "fetch_history",
			GetSpec:    GetFetchHistoryToolSpec,
			Stage:      StageAction,
			Visibility: VisibilityVisible,
			// fetch_history: 仅 focus_mode 开启时可用（照抄 experimental 分支）。
			EnabledByConfig: func() bool { return flags.FocusMode },
		},
	}
}

// TrimSpecName is a tiny helper shared by tests (spec names are trimmed at
// registration time).
func TrimSpecName(name string) string { return strings.TrimSpace(name) }
