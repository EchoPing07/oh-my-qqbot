package tools

import (
	"context"
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

// BuiltinToolDeps 聚合内置工具 handler 的运行期依赖（由装配层注入）。
// 所有字段均可缺省：依赖未装配时对应工具回执明确的失败结果，而不是 panic。
type BuiltinToolDeps struct {
	// RuntimeFor 按会话返回运行时门面（wait / view_forward_message 使用）；
	// 返回 nil 表示会话未注册。
	RuntimeFor func(sessionID string) SessionRuntime
	// ReplyFor 按会话返回 reply 工具的完整依赖；nil 表示 reply 链路未装配。
	ReplyFor func(sessionID string) *ReplyDependencies
	// FocusHistoryFor 按会话返回 fetch_history 的门面（焦点模式）。
	FocusHistoryFor func(sessionID string) FetchHistoryRuntime
	// Memory 提供长期记忆检索（query_memory）；nil 时该工具回执失败。
	Memory MemorySearcher
	// EmojiSender / EmojiLister 是表情包边界（骨架语义：nil 时按
	// 「视觉模型未配置 / 无可用表情」文案回执，与 Python 未配置行为一致）。
	EmojiSender EmojiSender
	EmojiLister EmojiLister
}

// SessionRuntime 是内置工具按会话解析的运行时窄接口并集
// （internal/chat/runtime.Runtime 天然满足；本包不 import runtime）。
type SessionRuntime interface {
	WaitRuntime
	ForwardViewRuntime
	ReplyRuntime
}

// unwiredResult 是依赖未装配时的统一失败回执（防御 nil 依赖调用 panic）。
func unwiredResult(toolName string) ToolResult {
	return BuildFailureResult(toolName, "该工具的运行时依赖尚未装配，无法执行。", nil, nil)
}

// resolveRuntime 按 toolCtx.SessionID 解析会话运行时（未装配时返回 nil）。
func resolveRuntime(deps *BuiltinToolDeps, toolCtx *RuntimeContext) SessionRuntime {
	if deps == nil || deps.RuntimeFor == nil || toolCtx == nil {
		return nil
	}
	return deps.RuntimeFor(toolCtx.SessionID)
}

// BuiltinEntries returns the builtin tool entries with the config-driven
// visibility gates wired in (顺序照抄 BUILTIN_TOOL_ENTRIES)。
// deps 携带 handler 的运行期依赖；允许为 nil（handler 仍全部装配，
// 调用时回执「未装配」失败而非 nil handler panic）。
func BuiltinEntries(flags FeatureFlags, deps *BuiltinToolDeps) []BuiltinToolEntry {
	replyDeps := &ReplyDependencies{}
	replyDeps.BotNameFn = func() string { return flags.BotNickname }
	return []BuiltinToolEntry{
		{
			Name:    "wait",
			GetSpec: GetWaitToolSpec,
			// wait：进入固定等待（依赖会话运行时的等待状态机）。
			HandleTool: func(ctx context.Context, toolCtx *RuntimeContext, invocation ToolInvocation) ToolResult {
				runtime := resolveRuntime(deps, toolCtx)
				if runtime == nil {
					return unwiredResult(invocation.ToolName)
				}
				return HandleWaitTool(ctx, runtime, invocation)
			},
			Stage:           StageBoth,
			Visibility:      VisibilityVisible,
			EnabledByConfig: func() bool { return true },
		},
		{
			Name: "reply",
			GetSpec: func() ToolSpec {
				return GetReplyToolSpec(replyDeps, flags.UseNewEmojiSendLogic, flags.UseVectorExpression)
			},
			// reply：生成可见回复并发送（runtime/sender/generator/postprocessor
			// 全部按会话解析；任一缺失即失败回执）。
			HandleTool: func(ctx context.Context, toolCtx *RuntimeContext, invocation ToolInvocation) ToolResult {
				var replyDeps *ReplyDependencies
				if deps != nil && deps.ReplyFor != nil && toolCtx != nil {
					replyDeps = deps.ReplyFor(toolCtx.SessionID)
				}
				if replyDeps == nil || replyDeps.Runtime == nil || replyDeps.Sender == nil ||
					replyDeps.Generator == nil || replyDeps.PostProcessor == nil {
					return unwiredResult(invocation.ToolName)
				}
				return HandleReplyTool(ctx, replyDeps, invocation)
			},
			Stage:           StageAction,
			Visibility:      VisibilityVisible,
			EnabledByConfig: func() bool { return true },
		},
		{
			Name:    "view_forward_message",
			GetSpec: GetViewForwardMessageToolSpec,
			// view_forward_message：逐层展开合并转发消息。
			HandleTool: func(_ context.Context, toolCtx *RuntimeContext, invocation ToolInvocation) ToolResult {
				runtime := resolveRuntime(deps, toolCtx)
				if runtime == nil {
					return unwiredResult(invocation.ToolName)
				}
				return HandleViewForwardMessageTool(runtime, invocation)
			},
			Stage:           StageAction,
			Visibility:      VisibilityDeferred,
			EnabledByConfig: func() bool { return true },
		},
		{
			Name:    "query_memory",
			GetSpec: func() ToolSpec { return GetQueryMemoryToolSpec(flags.EnableMemoryQueryTool) },
			// query_memory：检索长期记忆（会话上下文取自 toolCtx）。
			HandleTool: func(ctx context.Context, toolCtx *RuntimeContext, invocation ToolInvocation) ToolResult {
				if deps == nil || deps.Memory == nil || toolCtx == nil {
					return unwiredResult(invocation.ToolName)
				}
				return HandleQueryMemoryTool(ctx, &QueryMemoryDeps{
					Searcher:     deps.Memory,
					SessionID:    toolCtx.SessionID,
					DefaultLimit: flags.MemoryQueryDefaultLimit,
				}, invocation)
			},
			Stage:           StageAction,
			Visibility:      VisibilityVisible,
			EnabledByConfig: func() bool { return true },
		},
		{
			Name:    "send_emoji",
			GetSpec: GetSendEmojiToolSpec,
			// send_emoji：委托注入的 EmojiSender（nil 时按未配置文案回执）。
			HandleTool: func(ctx context.Context, toolCtx *RuntimeContext, invocation ToolInvocation) ToolResult {
				var emojiSender EmojiSender
				if deps != nil {
					emojiSender = deps.EmojiSender
				}
				sessionID := ""
				if toolCtx != nil {
					sessionID = toolCtx.SessionID
				}
				return HandleSendEmojiTool(ctx, emojiSender, sessionID, invocation.Reasoning, invocation)
			},
			Stage:      StageAction,
			Visibility: VisibilityVisible,
			// send_emoji: 未开启新发送逻辑时可用（照抄取反分支）。
			EnabledByConfig: func() bool { return !flags.UseNewEmojiSendLogic },
		},
		{
			Name:    "show_emoji_list",
			GetSpec: GetShowEmojiListToolSpec,
			// show_emoji_list：委托注入的 EmojiLister（nil 时按无表情文案回执）。
			HandleTool: func(ctx context.Context, _ *RuntimeContext, invocation ToolInvocation) ToolResult {
				var lister EmojiLister
				if deps != nil {
					lister = deps.EmojiLister
				}
				return HandleShowEmojiListTool(ctx, lister, invocation)
			},
			Stage:      StageAction,
			Visibility: VisibilityVisible,
			// show_emoji_list: 仅新发送逻辑开启时可用。
			EnabledByConfig: func() bool { return flags.UseNewEmojiSendLogic },
		},
		{
			Name:    "tool_search",
			GetSpec: GetToolSearchToolSpec,
			// tool_search 的 handler 需要注册表自身（搜索/发现 deferred tools），
			// 由 NewToolRegistry 在构建时回填（见 registry.go），这里不填。
			Stage:           StageAction,
			Visibility:      VisibilityVisible,
			EnabledByConfig: func() bool { return true },
		},
		{
			Name:    "fetch_history",
			GetSpec: GetFetchHistoryToolSpec,
			// fetch_history：焦点模式取回未入上下文的历史消息。
			HandleTool: func(_ context.Context, toolCtx *RuntimeContext, invocation ToolInvocation) ToolResult {
				var runtime FetchHistoryRuntime
				if deps != nil && deps.FocusHistoryFor != nil && toolCtx != nil {
					runtime = deps.FocusHistoryFor(toolCtx.SessionID)
				}
				if runtime == nil {
					return unwiredResult(invocation.ToolName)
				}
				return HandleFetchHistoryTool(runtime, invocation)
			},
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
