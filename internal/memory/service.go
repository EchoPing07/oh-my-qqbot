package memory

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// This file mirrors 参考/MaiBot/src/services/memory_service.py
// (behaviour is authoritative there): MemoryService 门面。
//
// Python 版把每次调用转发给 A_memorix host service 的组件方法；Go 版直接持有
// internal.MemoryKernel 的能力面（KernelSurface，由装配层注入 P5 内核）。

// DefaultSearchLimit mirrors search(limit=5) 的默认值。
const DefaultSearchLimit = 5

// MemoryService 是长期记忆的统一门面（对齐 services/memory_service.py 的调用面）。
type MemoryService struct {
	// Surface 汇总内核能力（任意字段可为 nil：对应能力不可用）。
	Surface KernelSurface
	// Logf 日志回调。
	Logf func(format string, args ...any)
}

// NewMemoryService 构造门面。
func NewMemoryService(surface KernelSurface) *MemoryService {
	return &MemoryService{Surface: surface}
}

func (s *MemoryService) log(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// ---------------------------------------------------------------------------
// 检索
// ---------------------------------------------------------------------------

// SearchOptions mirrors search() 的关键字参数（裁剪掉 Python 的 time_*/person 过滤
// 参数中内核不支持的部分）。
type SearchOptions struct {
	Limit         int
	Mode          string
	ChatID        string
	PersonID      string
	RespectFilter bool
	UserID        string
	GroupID       string
}

// Search mirrors MemoryService.search：空 query 视为无效请求（返回空结果）。
func (s *MemoryService) Search(ctx context.Context, query string, options SearchOptions) MemorySearchResult {
	cleanQuery := strings.TrimSpace(query)
	if cleanQuery == "" {
		return MemorySearchResult{}
	}
	if s.Surface.Searcher == nil {
		return MemorySearchResult{Success: false, Error: "memory kernel unavailable"}
	}
	limit := options.Limit
	if limit < 1 {
		limit = DefaultSearchLimit
	}
	mode := strings.TrimSpace(options.Mode)
	if mode == "" {
		mode = "search"
	}
	hits, err := s.Surface.Searcher.SearchMemory(ctx, mode, cleanQuery, options.ChatID, limit)
	if err != nil {
		s.log("长期记忆搜索失败: %v", err)
		return MemorySearchResult{Success: false, Error: err.Error()}
	}
	return MemorySearchResult{
		Hits:    ToMemorySearchHits(hits),
		Success: true,
	}
}

// SearchText mirrors MemorySearchResult.to_text（截断 + 编号）。
func SearchText(result MemorySearchResult, limit int, maxContentChars int) string {
	if len(result.Hits) == 0 {
		return ""
	}
	if limit < 1 {
		limit = 1
	}
	if maxContentChars <= 0 {
		maxContentChars = 160
	}
	lines := make([]string, 0, limit)
	for index, hit := range result.Hits {
		if index >= limit {
			break
		}
		content := strings.ReplaceAll(strings.TrimSpace(hit.Content), "\n", " ")
		runes := []rune(content)
		if len(runes) > maxContentChars {
			content = string(runes[:maxContentChars]) + "..."
		}
		lines = append(lines, fmt.Sprintf("%d. %s", index+1, content))
	}
	return strings.Join(lines, "\n")
}

// ---------------------------------------------------------------------------
// 写入
// ---------------------------------------------------------------------------

// IngestSummaryOptions mirrors ingest_summary 的关键字参数。
type IngestSummaryOptions struct {
	ChatID       string
	Text         string
	Participants []string
	TimeStart    *time.Time
	TimeEnd      *time.Time
	Tags         []string
	Metadata     map[string]any
	UserID       string
	GroupID      string
	// ExternalID 对齐 external_id（幂等键）。
	ExternalID string
}

// IngestSummaryResult mirrors MemoryWriteResult（含内部摘要文本，供调用方复用）。
type IngestSummaryResult struct {
	MemoryWriteResult
	// Summary 是实际写入内核的摘要文本（空表示无增量内容）。
	Summary string
}

// IngestSummary mirrors MemoryService.ingest_summary：text 为空时用
// a_memorix_chat_summary.prompt 从聊天流生成摘要。
func (s *MemoryService) IngestSummary(
	ctx context.Context,
	options IngestSummaryOptions,
	generator *ChatSummaryGenerator,
) IngestSummaryResult {
	chatID := strings.TrimSpace(options.ChatID)
	text := strings.TrimSpace(options.Text)
	if text == "" {
		if generator == nil || !metadataBool(options.Metadata, "generate_from_chat") {
			return IngestSummaryResult{}
		}
		generated, err := generator.Generate(ctx, chatID, options.TimeEnd, intMetadata(options.Metadata, "context_length"))
		if err != nil {
			return IngestSummaryResult{MemoryWriteResult: MemoryWriteResult{Success: false, Detail: err.Error()}}
		}
		text = generated
	}
	if text == "" {
		return IngestSummaryResult{}
	}
	if s.Surface.Summaries == nil {
		return IngestSummaryResult{MemoryWriteResult: MemoryWriteResult{Success: false, Detail: "memory kernel unavailable"}}
	}
	ts := time.Now()
	if options.TimeEnd != nil {
		ts = *options.TimeEnd
	}
	if err := s.Surface.Summaries.IngestSummary(ctx, chatID, text, ts); err != nil {
		s.log("长期记忆写入摘要失败: %v", err)
		return IngestSummaryResult{MemoryWriteResult: MemoryWriteResult{Success: false, Detail: err.Error()}}
	}
	externalID := strings.TrimSpace(options.ExternalID)
	if externalID == "" {
		externalID = "chat_summary:" + chatID
	}
	return IngestSummaryResult{
		MemoryWriteResult: MemoryWriteResult{Success: true, StoredIDs: []string{externalID}},
		Summary:           text,
	}
}

// IngestText mirrors MemoryService.ingest_text。
func (s *MemoryService) IngestText(ctx context.Context, chatID string, text string, ts time.Time) MemoryWriteResult {
	cleanText := strings.TrimSpace(text)
	if cleanText == "" {
		return MemoryWriteResult{}
	}
	if s.Surface.TextWriter == nil {
		return MemoryWriteResult{Success: false, Detail: "memory kernel unavailable"}
	}
	if err := s.Surface.TextWriter.IngestText(ctx, chatID, cleanText, ts); err != nil {
		s.log("长期记忆写入文本失败: %v", err)
		return MemoryWriteResult{Success: false, Detail: err.Error()}
	}
	return MemoryWriteResult{Success: true}
}

// ---------------------------------------------------------------------------
// 人物画像
// ---------------------------------------------------------------------------

// GetPersonProfile mirrors MemoryService.get_person_profile。
func (s *MemoryService) GetPersonProfile(ctx context.Context, personID string, limit int) PersonProfileResult {
	cleanPersonID := strings.TrimSpace(personID)
	if cleanPersonID == "" {
		return PersonProfileResult{}
	}
	if s.Surface.Profiles == nil {
		return PersonProfileResult{}
	}
	profile, err := s.Surface.Profiles.GetPersonProfile(ctx, cleanPersonID)
	if err != nil {
		s.log("获取人物画像失败: %v", err)
		return PersonProfileResult{}
	}
	if profile == nil {
		return PersonProfileResult{}
	}
	limitFacts := limit
	if limitFacts < 1 {
		limitFacts = ProfileQueryLimit
	}
	facts := profile.Facts
	if len(facts) > limitFacts {
		facts = facts[:limitFacts]
	}
	evidence := make([]map[string]any, 0, len(facts))
	traits := make([]string, 0, len(facts))
	for _, fact := range facts {
		content := strings.TrimSpace(fact.Content)
		if content == "" {
			continue
		}
		traits = append(traits, content)
		evidence = append(evidence, map[string]any{
			"content":    content,
			"category":   fact.Category,
			"confidence": fact.Confidence,
			"source":     fact.Source,
			"fact_id":    fact.ID,
		})
	}
	summary := strings.TrimSpace(profile.Summary)
	if summary == "" {
		summary = strings.Join(traits, "；")
	}
	return PersonProfileResult{Summary: summary, Traits: traits, Evidence: evidence}
}

// QueryPersonProfile mirrors MemoryService.query_person_profile（工具支撑面：
// person_id 或 person_name 至少给一个）。
func (s *MemoryService) QueryPersonProfile(
	ctx context.Context,
	personID string,
	personName string,
	limit int,
) (PersonProfileResult, error) {
	cleanPersonID := strings.TrimSpace(personID)
	if cleanPersonID == "" && strings.TrimSpace(personName) == "" {
		return PersonProfileResult{}, fmt.Errorf("query_person_profile 需要提供 person_id 或 person_name 中的一个。")
	}
	if cleanPersonID == "" {
		return PersonProfileResult{}, fmt.Errorf("未找到人物画像。")
	}
	result := s.GetPersonProfile(ctx, cleanPersonID, limit)
	if result.Summary == "" && len(result.Traits) == 0 {
		return result, fmt.Errorf("未找到人物画像。")
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// 图片记忆
// ---------------------------------------------------------------------------

// IngestImageMemory mirrors MemoryService.image_memory(action="ingest")。
func (s *MemoryService) IngestImageMemory(
	ctx context.Context,
	chatID string,
	description string,
	hash string,
	ts time.Time,
) MemoryWriteResult {
	if strings.TrimSpace(description) == "" {
		return MemoryWriteResult{}
	}
	if s.Surface.Images == nil {
		return MemoryWriteResult{Success: false, Detail: "memory kernel unavailable"}
	}
	if err := s.Surface.Images.IngestImageMemory(ctx, chatID, description, hash, ts); err != nil {
		s.log("图片记忆写入失败: %v", err)
		return MemoryWriteResult{Success: false, Detail: err.Error()}
	}
	return MemoryWriteResult{Success: true}
}

// ---------------------------------------------------------------------------
// query_memory 工具支撑（内部工具查询入口）
// ---------------------------------------------------------------------------

// QueryMemoryRequest mirrors 内置工具 query_memory 的入参（Go 视角）。
type QueryMemoryRequest struct {
	Query      string
	Mode       string
	ChatID     string
	PersonName string
	Limit      int
	// TimeStart / TimeEnd mirror 工具的时间条件（nil 表示未给出）。
	TimeStart *time.Time
	TimeEnd   *time.Time
	// RespectFilter mirrors respect_filter（默认 true）。
	RespectFilter bool
}

// QueryMemoryToolResult 是 query_memory 工具的结果视图（对齐
// internal/chat/tools.MemorySearchResult；避免循环依赖，用门面结构承载）。
type QueryMemoryToolResult struct {
	Success       bool
	Error         string
	Summary       string
	Hits          []MemorySearchHit
	Filtered      bool
	EffectiveMode string
	Limit         int
}

// QueryMemory mirrors 内置工具 query_memory 的检索语义（五模式全保留）。
//
// EffectiveMode 对齐 Python 的 effective_mode 回写规则：time/hybrid 缺时间条件
// 时降级为 search（内核只接受 mode + query，时间条件由 query 承载）。
func (s *MemoryService) QueryMemory(ctx context.Context, request QueryMemoryRequest) QueryMemoryToolResult {
	mode := strings.ToLower(strings.TrimSpace(request.Mode))
	effective := mode
	if effective == "" {
		effective = "search"
	}
	if (effective == "time" || effective == "hybrid") && request.TimeStart == nil && request.TimeEnd == nil {
		effective = "search"
	}
	limit := request.Limit
	if limit < 1 {
		limit = DefaultSearchLimit
	}
	if limit > 20 {
		limit = 20
	}
	result := QueryMemoryToolResult{EffectiveMode: effective, Limit: limit}
	searchResult := s.Search(ctx, request.Query, SearchOptions{
		Limit:         limit,
		Mode:          effective,
		ChatID:        request.ChatID,
		RespectFilter: request.RespectFilter,
	})
	result.Success = searchResult.Success
	result.Error = searchResult.Error
	result.Summary = searchResult.Summary
	result.Hits = searchResult.Hits
	result.Filtered = searchResult.Filtered
	return result
}

// ---------------------------------------------------------------------------
// 后台自动化服务接入（§6.15）
// ---------------------------------------------------------------------------

// ChatSummaryGenerator 用 a_memorix_chat_summary.prompt 从聊天流生成摘要，
// 供 ingest_summary(text="") 的 generate_from_chat 路径与 ChatSummaryWritebackService 复用。
type ChatSummaryGenerator struct {
	LLM      LLMFunc
	Prompts  PromptFunc
	Messages MessageQueryFunc
	Counts   MessageCountFunc
	// BotName / PersonalityContext / PreviousSummaryContext 供 prompt 渲染。
	BotName                string
	PersonalityContext     string
	PreviousSummaryContext string
	// DefaultContextLength mirrors chat_summary_writeback_context_length。
	DefaultContextLength int
}

// Generate 生成指定聊天流最近 contextLength 条消息的增量摘要。
func (g *ChatSummaryGenerator) Generate(
	ctx context.Context,
	chatID string,
	timeEnd *time.Time,
	contextLength int,
) (string, error) {
	if g == nil || g.LLM == nil || g.Prompts == nil || g.Messages == nil {
		return "", nil
	}
	if contextLength <= 0 {
		contextLength = g.DefaultContextLength
	}
	if contextLength <= 0 {
		contextLength = 36
	}
	items, err := g.Messages(ctx, MessageQuery{
		SessionID:  chatID,
		BeforeTime: timeEnd,
		Limit:      contextLength,
		LimitMode:  "latest",
	})
	if err != nil {
		return "", err
	}
	if len(items) == 0 {
		return "", nil
	}
	prompt, err := g.Prompts("a_memorix_chat_summary", map[string]string{
		"bot_name":                 defaultString(g.BotName, "麦麦"),
		"personality_context":      g.PersonalityContext,
		"previous_summary_context": g.PreviousSummaryContext,
		"chat_history":             FormatChatHistoryForSummary(items),
		"image_evidence_catalog":   "",
	})
	if err != nil {
		return "", err
	}
	raw, err := g.LLM(ctx, LLMRequest{
		Task:        "utils",
		RequestType: "A_memorix.chat_summary",
		SessionID:   chatID,
		Messages:    []LLMMessage{{Role: "user", Content: prompt}},
	})
	if err != nil {
		return "", err
	}
	return ParseChatSummaryResponse(raw), nil
}

func metadataBool(metadata map[string]any, key string) bool {
	if metadata == nil {
		return false
	}
	value, ok := metadata[key]
	if !ok {
		return false
	}
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		return strings.EqualFold(strings.TrimSpace(typed), "true")
	}
	return false
}

func intMetadata(metadata map[string]any, key string) int {
	if metadata == nil {
		return 0
	}
	value, ok := metadata[key]
	if !ok {
		return 0
	}
	number, ok := toFloat64(value)
	if !ok {
		return 0
	}
	return int(number)
}
