package memory

import (
	"context"

	"oh-my-qqbot/internal/chat/tools"
)

// This file bridges the MemoryService facade to the query_memory builtin tool
// (P4b, internal/chat/tools). The tool declares its own minimal read-only
// interface `tools.MemorySearcher`; this adapter is the concrete implementation
// wired by the assembly layer.

// 编译期断言：ToolsMemorySearcher 满足 tools.MemorySearcher。
var _ tools.MemorySearcher = (*ToolsMemorySearcher)(nil)

// ToolsMemorySearcher 把 MemoryService 的检索能力适配为 query_memory 工具的
// 注入边界（`internal/chat/tools.QueryMemoryDeps.Searcher`）。
type ToolsMemorySearcher struct {
	Service *MemoryService
	// DefaultLimit mirrors a_memorix.integration.memory_query_default_limit。
	DefaultLimit int
}

// NewToolsMemorySearcher 构造工具侧适配器。
func NewToolsMemorySearcher(service *MemoryService, defaultLimit int) *ToolsMemorySearcher {
	if defaultLimit < 1 {
		defaultLimit = DefaultSearchLimit
	}
	return &ToolsMemorySearcher{Service: service, DefaultLimit: defaultLimit}
}

// SearchMemory mirrors tools.MemorySearcher：五模式全部保留，mode 直接映射内核
// `SearchMemory`（§6.9：query_memory 五模式全部保留）。
func (a *ToolsMemorySearcher) SearchMemory(
	ctx context.Context,
	mode string,
	query string,
	chatID string,
	topK int,
) (tools.MemorySearchResult, error) {
	if a == nil || a.Service == nil {
		return tools.MemorySearchResult{Success: false, Error: "memory service unavailable"}, nil
	}
	limit := topK
	if limit < 1 {
		limit = a.DefaultLimit
	}
	result := a.Service.Search(ctx, query, SearchOptions{
		Limit:         limit,
		Mode:          mode,
		ChatID:        chatID,
		RespectFilter: true,
	})
	hits := make([]tools.MemorySearchHit, 0, len(result.Hits))
	for _, hit := range result.Hits {
		hits = append(hits, tools.MemorySearchHit{
			Content:   hit.Content,
			Score:     hit.Score,
			Type:      hit.Type,
			Source:    hit.Source,
			Hash:      hit.Hash,
			Metadata:  hit.Metadata,
			EpisodeID: hit.EpisodeID,
			Title:     hit.Title,
		})
	}
	return tools.MemorySearchResult{
		Summary:  result.Summary,
		Hits:     hits,
		Filtered: result.Filtered,
		Success:  result.Success,
		Error:    result.Error,
	}, nil
}
