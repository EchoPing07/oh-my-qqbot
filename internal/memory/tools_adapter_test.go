package memory_test

import (
	"context"
	"strings"
	"testing"

	contracts "oh-my-qqbot/internal"
	"oh-my-qqbot/internal/chat/tools"
	"oh-my-qqbot/internal/memory"
)

// 编译期断言：适配器满足 query_memory 工具的注入边界。
var _ tools.MemorySearcher = (*memory.ToolsMemorySearcher)(nil)

type toolsStubSearcher struct{ calls int }

func (s *toolsStubSearcher) SearchMemory(_ context.Context, _ string, _ string, _ string, _ int) ([]contracts.MemoryHit, error) {
	s.calls++
	return []contracts.MemoryHit{{
		Kind: "paragraph", ID: "hash-1", Text: "阿强对青霉素过敏", Score: 0.9, ChatID: "s1",
	}}, nil
}

// TestToolsMemorySearcherFeedsQueryMemoryTool 端到端：门面 → 工具 → 回执文案。
func TestToolsMemorySearcherFeedsQueryMemoryTool(t *testing.T) {
	searcher := &toolsStubSearcher{}
	service := memory.NewMemoryService(memory.KernelSurface{Searcher: searcher})
	adapter := memory.NewToolsMemorySearcher(service, 5)

	result := tools.HandleQueryMemoryTool(context.Background(), &tools.QueryMemoryDeps{
		Searcher:     adapter,
		SessionID:    "s1",
		DefaultLimit: 5,
	}, tools.ToolInvocation{
		ToolName:  "query_memory",
		Arguments: map[string]any{"query": "阿强过敏", "mode": "search", "limit": float64(3)},
	})
	if !result.Success {
		t.Fatalf("工具应成功: %+v", result)
	}
	if !strings.Contains(result.Content, "阿强对青霉素过敏") {
		t.Fatalf("回执应包含命中内容: %q", result.Content)
	}
	if result.Metadata["replyer_memory_reference"] == nil {
		t.Fatalf("应产出 replyer 参考块元数据: %+v", result.Metadata)
	}
	if searcher.calls != 1 {
		t.Fatalf("应调用内核一次: %d", searcher.calls)
	}
}

// TestToolsMemorySearcherRejectsUnsupportedMode 验证 mode 白名单由工具层把关。
func TestToolsMemorySearcherRejectsUnsupportedMode(t *testing.T) {
	service := memory.NewMemoryService(memory.KernelSurface{Searcher: &toolsStubSearcher{}})
	adapter := memory.NewToolsMemorySearcher(service, 5)
	result := tools.HandleQueryMemoryTool(context.Background(), &tools.QueryMemoryDeps{
		Searcher: adapter, SessionID: "s1", DefaultLimit: 5,
	}, tools.ToolInvocation{
		ToolName:  "query_memory",
		Arguments: map[string]any{"query": "x", "mode": "bogus"},
	})
	if result.Success {
		t.Fatalf("未知模式应失败: %+v", result)
	}
	if !strings.Contains(result.ErrorMessage, "search/time/hybrid/episode/aggregate") {
		t.Fatalf("错误文案应列出五种模式: %q", result.ErrorMessage)
	}
}
