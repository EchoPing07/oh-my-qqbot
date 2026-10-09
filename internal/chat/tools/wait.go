package tools

import (
	"fmt"
	"strings"
)

// This file mirrors 参考/MaiBot/src/maisaka/builtin_tool/wait.py
// (behaviour is authoritative there): wait 内置工具。

// WaitRuntime 是 wait 工具需要的 runtime 面（由 P4a runtime 直接满足）。
type WaitRuntime interface {
	// TryEnterWaitState mirrors _try_enter_wait_state: returns (entered,
	// currentCount, maxCount)。
	TryEnterWaitState(seconds *float64, toolCallID string, logicalTurnID string) (bool, int, int)
}

// GetWaitToolSpec mirrors get_tool_spec（声明照抄）。
func GetWaitToolSpec() ToolSpec {
	return ToolSpec{
		Name:        "wait",
		Description: "暂停当前对话并固定等待一段时间。",
		ParametersSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"seconds": map[string]any{
					"type":        "integer",
					"description": "等待秒数。",
				},
			},
			"required": []string{"seconds"},
		},
		ProviderName: "maisaka_builtin",
		ProviderType: "builtin",
		Enabled:      true,
		Stage:        StageBoth,
		Visibility:   VisibilityVisible,
	}
}

// HandleWaitTool mirrors handle_tool（wait）。
func HandleWaitTool(_ contextT, runtime WaitRuntime, invocation ToolInvocation) ToolResult {
	toolName := invocation.ToolName
	rawSeconds := invocation.Arguments["seconds"]
	waitSeconds := 30
	switch v := rawSeconds.(type) {
	case float64:
		waitSeconds = int(v)
	case int:
		waitSeconds = v
	case string:
		var parsed int
		if _, err := fmt.Sscanf(strings.TrimSpace(v), "%d", &parsed); err == nil {
			waitSeconds = parsed
		}
	}
	if waitSeconds < 0 {
		waitSeconds = 0
	}
	seconds := float64(waitSeconds)
	entered, currentCount, maxCount := runtime.TryEnterWaitState(
		&seconds, invocation.CallID, invocation.LogicalTurnID)
	if !entered {
		return BuildSuccessResult(
			toolName,
			fmt.Sprintf("连续 wait 已达到上限 %d 次，本次不再进入等待；视为多次等待后仍无后续，当前对话进入休息。", maxCount),
			nil,
			map[string]any{
				"pause_execution":            true,
				"wait_limit_reached":         true,
				"wait_rest":                  true,
				"consecutive_wait_count":     currentCount,
				"max_consecutive_wait_count": maxCount,
			},
			nil,
		)
	}
	return BuildSuccessResult(
		toolName,
		fmt.Sprintf("当前对话循环进入等待状态，将固定等待 %d 秒；期间收到的新消息不会提前打断本次等待。连续 wait 次数：%d/%d。",
			waitSeconds, currentCount, maxCount),
		nil,
		map[string]any{
			"pause_execution":            true,
			"consecutive_wait_count":     currentCount,
			"max_consecutive_wait_count": maxCount,
		},
		nil,
	)
}
