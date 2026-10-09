package memory

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	contracts "oh-my-qqbot/internal"
	"oh-my-qqbot/internal/message"
)

// This file mirrors 参考/MaiBot/src/maisaka/memory/heuristic_injector.py
// (behaviour is authoritative there): 群聊按可配置间隔用最近消息构造聊天印象
// prompt，检索内核长期记忆，命中以【启发式记忆-内部参考】注入 Planner。

// HeuristicMemoryReferenceMarker mirrors HEURISTIC_MEMORY_REFERENCE_MARKER.
const HeuristicMemoryReferenceMarker = "【启发式记忆-内部参考】"

// heuristicMemoryReferenceHeader mirrors `_format_reference` 的第二行。
const heuristicMemoryReferenceHeader = "Internal long-term memory recalled from the current chat impression. " +
	"Use it only as reasoning context; do not quote it verbatim to the user."

// 记忆来源前缀（对齐 _SOURCE_CHAT_SUMMARY_PREFIX / _SOURCE_PERSON_FACT_PREFIX）。
const (
	sourceChatSummaryPrefix = "chat_summary:"
	sourcePersonFactPrefix  = "person_fact:"
)

// HeuristicMemoryOptions mirrors a_memorix.integration 的启发式记忆配置项。
type HeuristicMemoryOptions struct {
	// Enabled mirrors heuristic_memory_recall_enabled（默认 false）。
	Enabled bool
	// WindowSize mirrors heuristic_memory_recall_window_size（默认 20）。
	WindowSize int
	// Limit mirrors heuristic_memory_recall_limit（默认 3）。
	Limit int
	// MaxChars mirrors heuristic_memory_recall_max_chars（默认 900）。
	MaxChars int
	// MinIntervalSeconds mirrors heuristic_memory_recall_min_interval_seconds（默认 180）。
	MinIntervalSeconds int
	// MinNewMessages mirrors heuristic_memory_recall_min_new_messages（默认 60）。
	MinNewMessages int
	// CacheTTLSeconds mirrors heuristic_memory_recall_cache_ttl_seconds（默认 300）。
	CacheTTLSeconds int
	// CrossChatEnabled mirrors heuristic_memory_cross_chat_enabled（默认 false）。
	CrossChatEnabled bool
	// GroupToPrivateEnabled mirrors heuristic_memory_group_to_private_enabled。
	GroupToPrivateEnabled bool
	// PrivateToGroupEnabled mirrors heuristic_memory_private_to_group_enabled。
	PrivateToGroupEnabled bool
}

// DefaultHeuristicMemoryOptions 返回照抄 Python 默认值的配置。
func DefaultHeuristicMemoryOptions() HeuristicMemoryOptions {
	return HeuristicMemoryOptions{
		Enabled:            false,
		WindowSize:         20,
		Limit:              3,
		MaxChars:           900,
		MinIntervalSeconds: 180,
		MinNewMessages:     60,
		CacheTTLSeconds:    300,
	}
}

// HeuristicMemoryRecallState mirrors HeuristicMemoryRecallState.
type HeuristicMemoryRecallState struct {
	LastRecallAt     float64
	LastMessageCount int
	CachedReference  string
	CacheExpiresAt   float64
}

// HeuristicMemoryContext mirrors HeuristicMemoryContext.
type HeuristicMemoryContext struct {
	Session           *SessionIdentity
	RecentMessages    []*message.SessionMessage
	ActivePersonIDs   map[string]bool
	TotalMessageCount int
}

// HitSourceResolver 把检索命中的 ID 解析为来源标记（对齐
// `_resolve_hit_sources_by_hash`：Python 经 memory_delete_admin(action="preview") 反查
// paragraph 的 source / source_type / chat_id / person_id）。
//
// 返回 nil 表示无法解析，此时按 unknown 来源处理（命中被拒绝）。
type HitSourceResolver func(ctx context.Context, hitIDs []string) map[string]HitSource

// HitSource 是一条命中记录的来源元数据。
type HitSource struct {
	// Source mirrors metadata["source"]（如 chat_summary:<session> / person_fact:<person>）。
	Source string
	// SourceType mirrors metadata["source_type"]（chat_summary / person_fact）。
	SourceType string
	// ChatID / PersonID mirror 同名字段。
	ChatID   string
	PersonID string
}

// HeuristicMemoryInjector mirrors HeuristicMemoryInjector.
//
// 本包不 import internal/config，配置经 Options 注入；runtime 侧可用
// BuildHeuristicReferenceMessage 适配成一条 ReferenceMessage。
type HeuristicMemoryInjector struct {
	Options HeuristicMemoryOptions
	LLM     LLMFunc
	Prompts PromptFunc
	Kernel  MemorySearcher
	// Sessions 解析已注册聊天流（对齐 chat_manager.get_existing_session_by_session_id）。
	Sessions SessionLookupFunc
	// Messages 读取最近消息（对齐 find_messages）。
	Messages MessageQueryFunc
	// Counts 统计会话消息数（对齐 count_messages）。
	Counts MessageCountFunc
	// PersonID mirrors get_person_id。
	PersonID PersonIDFunc
	// Resolver 解析命中来源（对齐 `_resolve_hit_sources_by_hash`；nil 时按
	// hit.ChatID 与 source 前缀启发式判定）。
	Resolver HitSourceResolver
	// Now 注入当前时间（秒，测试用）。
	Now func() float64
	// Logf 日志回掉。
	Logf func(format string, args ...any)

	mu     sync.Mutex
	states map[string]*HeuristicMemoryRecallState
	// SearchCalls 记录内核检索次数（测试/观测用）。
	SearchCalls int
}

// NewHeuristicMemoryInjector 构造注入器。
func NewHeuristicMemoryInjector(options HeuristicMemoryOptions) *HeuristicMemoryInjector {
	return &HeuristicMemoryInjector{Options: options, states: map[string]*HeuristicMemoryRecallState{}}
}

func (h *HeuristicMemoryInjector) now() float64 {
	if h.Now != nil {
		return h.Now()
	}
	return float64(time.Now().UnixNano()) / 1e9
}

func (h *HeuristicMemoryInjector) log(format string, args ...any) {
	if h.Logf != nil {
		h.Logf(format, args...)
	}
}

func (h *HeuristicMemoryInjector) windowSize() int {
	if h.Options.WindowSize > 0 {
		return h.Options.WindowSize
	}
	return 20
}

func (h *HeuristicMemoryInjector) limit() int {
	if h.Options.Limit > 0 {
		return h.Options.Limit
	}
	return 3
}

func (h *HeuristicMemoryInjector) maxChars() int {
	if h.Options.MaxChars > 0 {
		return h.Options.MaxChars
	}
	return 900
}

// BuildInjectionMessage mirrors build_injection_message（返回给 Planner 的
// 一次性参考文本；空字符串表示本轮不注入）。
func (h *HeuristicMemoryInjector) BuildInjectionMessage(ctx context.Context, sessionID string) (string, error) {
	if !h.Options.Enabled {
		h.ClearSessionReference(sessionID)
		return "", nil
	}
	if h.Sessions == nil {
		return "", nil
	}
	session, ok := h.Sessions(sessionID)
	if !ok || session == nil {
		h.log("启发式记忆跳过：无法解析真实聊天流 session_id=%q", sessionID)
		return "", nil
	}
	sessionKey := session.SessionID
	windowSize := h.windowSize()

	totalMessageCount := 0
	if h.Counts != nil {
		count, err := h.Counts(ctx, sessionKey, nil)
		if err != nil {
			return "", err
		}
		totalMessageCount = count
	}
	if totalMessageCount < windowSize {
		h.ClearSessionReference(sessionKey)
		return "", nil
	}

	state := h.stateFor(sessionKey)
	now := h.now()
	cacheTTL := h.Options.CacheTTLSeconds
	if state.CachedReference != "" && cacheTTL > 0 && now < state.CacheExpiresAt {
		return state.CachedReference, nil
	}
	if !CanTriggerHeuristicRecall(*state, totalMessageCount, now,
		maxInt(h.Options.MinIntervalSeconds, 0), maxInt(h.Options.MinNewMessages, 1)) {
		h.ClearSessionReference(sessionKey)
		return "", nil
	}
	if h.Messages == nil {
		return "", nil
	}
	recentMessages, err := h.Messages(ctx, MessageQuery{
		SessionID: sessionKey,
		Limit:     windowSize,
		LimitMode: "latest",
	})
	if err != nil {
		return "", err
	}
	if len(recentMessages) < windowSize {
		h.ClearSessionReference(sessionKey)
		return "", nil
	}

	context := HeuristicMemoryContext{
		Session:           session,
		RecentMessages:    recentMessages,
		ActivePersonIDs:   h.CollectActivePersonIDs(recentMessages),
		TotalMessageCount: totalMessageCount,
	}
	impression, err := h.buildChatImpression(ctx, context)
	if err != nil {
		h.ClearSessionReference(sessionKey)
		return "", err
	}
	if impression == "" {
		h.ClearSessionReference(sessionKey)
		return "", nil
	}
	hits := h.searchRelatedMemory(ctx, impression, context)
	reference := FormatHeuristicMemoryReference(hits, h.maxChars())

	h.mu.Lock()
	state.LastRecallAt = now
	state.LastMessageCount = totalMessageCount
	state.CachedReference = reference
	if cacheTTL > 0 {
		state.CacheExpiresAt = now + float64(cacheTTL)
	} else {
		state.CacheExpiresAt = 0
	}
	h.mu.Unlock()

	if reference != "" {
		h.log("启发式记忆自然拉起成功: session_id=%s hits=%d total_messages=%d",
			sessionKey, len(hits), totalMessageCount)
	}
	return reference, nil
}

// BuildHeuristicReferenceMessage 把注入文本包装成 Planner 参考消息载荷
// （对齐 runtime 侧注入形态；nil 表示本轮不注入）。
func (h *HeuristicMemoryInjector) BuildHeuristicReferenceMessage(
	ctx context.Context,
	sessionID string,
) (*ReferencePayload, error) {
	text, err := h.BuildInjectionMessage(ctx, sessionID)
	if err != nil || text == "" {
		return nil, err
	}
	return &ReferencePayload{
		Content:       text,
		Kind:          string(referenceMessageType("custom")),
		DisplayPrefix: "[参考消息]",
	}, nil
}

// ClearSessionReference mirrors clear_session_reference.
func (h *HeuristicMemoryInjector) ClearSessionReference(sessionID string) {
	key := strings.TrimSpace(sessionID)
	h.mu.Lock()
	defer h.mu.Unlock()
	state, ok := h.states[key]
	if !ok {
		return
	}
	state.CachedReference = ""
	state.CacheExpiresAt = 0
}

func (h *HeuristicMemoryInjector) stateFor(sessionID string) *HeuristicMemoryRecallState {
	h.mu.Lock()
	defer h.mu.Unlock()
	state, ok := h.states[sessionID]
	if !ok {
		state = &HeuristicMemoryRecallState{}
		h.states[sessionID] = state
	}
	return state
}

// CanTriggerHeuristicRecall mirrors `_can_trigger`.
func CanTriggerHeuristicRecall(
	state HeuristicMemoryRecallState,
	totalMessageCount int,
	now float64,
	minIntervalSeconds int,
	minNewMessages int,
) bool {
	if state.LastMessageCount <= 0 {
		return true
	}
	if minIntervalSeconds > 0 && now-state.LastRecallAt < float64(minIntervalSeconds) {
		return false
	}
	return totalMessageCount-state.LastMessageCount >= minNewMessages
}

func (h *HeuristicMemoryInjector) buildChatImpression(
	ctx context.Context,
	context HeuristicMemoryContext,
) (string, error) {
	if h.Prompts == nil || h.LLM == nil {
		return "", nil
	}
	chatIdentity := FormatHeuristicChatIdentity(context.Session)
	messageWindow := FormatHeuristicMessageWindow(context.RecentMessages)
	prompt, err := h.Prompts("heuristic_memory_impression", map[string]string{
		"chat_identity":  chatIdentity,
		"message_window": messageWindow,
	})
	if err != nil {
		return "", err
	}
	raw, err := h.LLM(ctx, LLMRequest{
		Task:        "utils",
		RequestType: "heuristic_memory_impression",
		SessionID:   context.Session.SessionID,
		Messages:    []LLMMessage{{Role: "user", Content: prompt}},
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(raw), nil
}

func (h *HeuristicMemoryInjector) searchRelatedMemory(
	ctx context.Context,
	impression string,
	context HeuristicMemoryContext,
) []contracts.MemoryHit {
	if h.Kernel == nil {
		return nil
	}
	limit := h.limit()
	searchLimit := maxInt(limit*4, limit)
	if searchLimit > 20 {
		searchLimit = 20
	}
	chatID := context.Session.SessionID
	if h.Options.CrossChatEnabled {
		chatID = ""
	}
	hits, err := h.Kernel.SearchMemory(ctx, "search", impression, chatID, searchLimit)
	if err != nil {
		h.log("启发式记忆检索失败，已跳过: %v", err)
		return nil
	}
	h.mu.Lock()
	h.SearchCalls++
	h.mu.Unlock()

	filtered := make([]contracts.MemoryHit, 0, limit)
	seen := map[string]bool{}
	sources := h.resolveHitSources(ctx, hits)
	for _, hit := range hits {
		if !h.IsHitAllowedWithSource(hit, context, sources[hit.ID]) {
			continue
		}
		dedupKey := hit.ID
		if dedupKey == "" {
			dedupKey = hit.Text
		}
		if seen[dedupKey] {
			continue
		}
		seen[dedupKey] = true
		filtered = append(filtered, hit)
		if len(filtered) >= limit {
			break
		}
	}
	return filtered
}

// resolveHitSources 批量解析命中来源（对齐 `_resolve_hit_sources_by_hash`）。
func (h *HeuristicMemoryInjector) resolveHitSources(
	ctx context.Context,
	hits []contracts.MemoryHit,
) map[string]HitSource {
	resolved := map[string]HitSource{}
	if h.Resolver == nil {
		return resolved
	}
	// 已有 scope 提示的命中不需要反查（对齐 `_hit_has_scope_hint`）。
	pending := make([]string, 0, len(hits))
	for _, hit := range hits {
		if hit.ID == "" || h.hitHasScopeHint(hit) {
			continue
		}
		pending = append(pending, hit.ID)
	}
	if len(pending) == 0 {
		return resolved
	}
	for id, source := range h.Resolver(ctx, pending) {
		resolved[id] = source
	}
	return resolved
}

// hitHasScopeHint mirrors `_hit_has_scope_hint`。
func (h *HeuristicMemoryInjector) hitHasScopeHint(hit contracts.MemoryHit) bool {
	if strings.TrimSpace(hit.ChatID) != "" {
		return true
	}
	source := strings.TrimSpace(hit.Kind)
	return strings.HasPrefix(source, sourceChatSummaryPrefix) || strings.HasPrefix(source, sourcePersonFactPrefix)
}

// IsHitAllowed mirrors `_is_hit_allowed`（来源作用域门控）；无来源解析结果时按
// hit 自带字段启发式判定。
func (h *HeuristicMemoryInjector) IsHitAllowed(hit contracts.MemoryHit, context HeuristicMemoryContext) bool {
	return h.IsHitAllowedWithSource(hit, context, HitSource{})
}

// IsHitAllowedWithSource mirrors `_is_hit_allowed(hit, ctx, resolved_source=...)`。
func (h *HeuristicMemoryInjector) IsHitAllowedWithSource(
	hit contracts.MemoryHit,
	contextData HeuristicMemoryContext,
	resolved HitSource,
) bool {
	kind := strings.TrimSpace(hit.Kind)
	source := strings.TrimSpace(hit.ChatID)
	if resolved.Source != "" {
		source = strings.TrimSpace(resolved.Source)
	}
	sourceType := strings.TrimSpace(resolved.SourceType)
	chatID := strings.TrimSpace(resolved.ChatID)
	personID := strings.TrimSpace(resolved.PersonID)
	if sourceType == "chat_summary" || strings.HasPrefix(source, sourceChatSummaryPrefix) {
		// chat_summary 的来源是 chat_id（而非 session id）。
		if chatID == "" && strings.HasPrefix(source, sourceChatSummaryPrefix) {
			chatID = strings.TrimPrefix(source, sourceChatSummaryPrefix)
		}
	} else if chatID == "" && !strings.HasPrefix(source, sourceChatSummaryPrefix) &&
		!strings.HasPrefix(source, sourcePersonFactPrefix) {
		chatID = strings.TrimSpace(hit.ChatID)
	}
	if personID == "" && strings.HasPrefix(source, sourcePersonFactPrefix) {
		personID = strings.TrimPrefix(source, sourcePersonFactPrefix)
	}

	if kind == "episode" {
		episodeSource := strings.TrimSpace(resolved.Source)
		return episodeSource == sourceChatSummaryPrefix+contextData.Session.SessionID
	}
	if sourceType == "person_fact" || strings.HasPrefix(source, sourcePersonFactPrefix) {
		return personID != "" && contextData.ActivePersonIDs[personID]
	}
	if sourceType == "chat_summary" || strings.HasPrefix(source, sourceChatSummaryPrefix) {
		return h.isChatMemoryAllowed(resolveHeuristicSourceSessionID(source, chatID), contextData.Session)
	}
	if chatID != "" {
		return h.isChatMemoryAllowed(chatID, contextData.Session)
	}
	return false
}

func resolveHeuristicSourceSessionID(source string, chatID string) string {
	if chatID != "" {
		return chatID
	}
	if strings.HasPrefix(source, sourceChatSummaryPrefix) {
		return strings.TrimPrefix(source, sourceChatSummaryPrefix)
	}
	return ""
}

func (h *HeuristicMemoryInjector) isChatMemoryAllowed(
	sourceSessionID string,
	current *SessionIdentity,
) bool {
	clean := strings.TrimSpace(sourceSessionID)
	if clean == "" || current == nil {
		return false
	}
	if clean == current.SessionID {
		return true
	}
	if !h.Options.CrossChatEnabled || h.Sessions == nil {
		return false
	}
	source, ok := h.Sessions(clean)
	if !ok || source == nil {
		return false
	}
	if source.IsGroup && !current.IsGroup {
		return h.Options.GroupToPrivateEnabled
	}
	if !source.IsGroup && current.IsGroup {
		return h.Options.PrivateToGroupEnabled
	}
	return true
}

// CollectActivePersonIDs mirrors `_collect_active_person_ids`.
func (h *HeuristicMemoryInjector) CollectActivePersonIDs(messages []*message.SessionMessage) map[string]bool {
	personIDs := map[string]bool{}
	if h.PersonID == nil {
		return personIDs
	}
	for _, msg := range messages {
		if msg == nil {
			continue
		}
		platform := strings.TrimSpace(msg.Platform)
		userID := strings.TrimSpace(msg.MessageInfo.UserInfo.UserID)
		if platform != "" && userID != "" {
			personIDs[h.PersonID(platform, userID)] = true
		}
		if msg.RawMessage == nil {
			continue
		}
		for _, component := range msg.RawMessage.Components {
			switch typed := component.(type) {
			case *message.AtComponent:
				if platform != "" && strings.TrimSpace(typed.TargetUserID) != "" {
					personIDs[h.PersonID(platform, strings.TrimSpace(typed.TargetUserID))] = true
				}
			case *message.ReplyComponent:
				target := ""
				if typed.TargetMessageSenderID != nil {
					target = strings.TrimSpace(*typed.TargetMessageSenderID)
				}
				if platform != "" && target != "" {
					personIDs[h.PersonID(platform, target)] = true
				}
			}
		}
	}
	return personIDs
}

// FormatHeuristicChatIdentity mirrors `_format_chat_identity`（文案照抄）。
func FormatHeuristicChatIdentity(session *SessionIdentity) string {
	if session == nil {
		return ""
	}
	chatType := "private"
	if session.IsGroup {
		chatType = "group"
	}
	displayName := strings.TrimSpace(session.DisplayName)
	if displayName == "" {
		displayName = session.SessionID
	}
	parts := []string{
		"chat_type: " + chatType,
		"display_name: " + displayName,
		"platform: " + session.Platform,
	}
	if session.GroupID != "" {
		parts = append(parts, "group_id: "+session.GroupID)
	}
	if session.UserID != "" {
		parts = append(parts, "user_id: "+session.UserID)
	}
	return strings.Join(parts, "\n")
}

// FormatHeuristicMessageWindow mirrors `_format_message_window`。
func FormatHeuristicMessageWindow(messages []*message.SessionMessage) string {
	lines := make([]string, 0, len(messages))
	for _, msg := range messages {
		if msg == nil {
			continue
		}
		text := processedText(msg)
		if text == "" {
			continue
		}
		text = strings.ReplaceAll(strings.ReplaceAll(text, "\r", " "), "\n", " ")
		runes := []rune(text)
		if len(runes) > 220 {
			text = strings.TrimRight(string(runes[:220]), " ") + "..."
		}
		lines = append(lines, "- "+heuristicSenderName(msg)+": "+text)
	}
	if len(lines) == 0 {
		return "no text messages available"
	}
	return strings.Join(lines, "\n")
}

func heuristicSenderName(msg *message.SessionMessage) string {
	name := strings.TrimSpace(msg.MessageInfo.UserInfo.DisplayName())
	if name == "" {
		return "unknown_user"
	}
	return name
}

// FormatHeuristicMemoryReference mirrors `_format_reference`（文案照抄 + 截断）。
func FormatHeuristicMemoryReference(hits []contracts.MemoryHit, maxChars int) string {
	if len(hits) == 0 {
		return ""
	}
	lines := []string{
		HeuristicMemoryReferenceMarker,
		heuristicMemoryReferenceHeader,
		"",
	}
	for index, hit := range hits {
		content := strings.Join(strings.Fields(strings.ReplaceAll(hit.Text, "\n", " ")), " ")
		if content == "" {
			continue
		}
		runes := []rune(content)
		if len(runes) > 180 {
			content = strings.TrimRight(string(runes[:180]), " ") + "..."
		}
		lines = append(lines, fmt.Sprintf("%d. %s", index+1, content))
	}
	reference := strings.TrimSpace(strings.Join(lines, "\n"))
	if maxChars <= 0 {
		maxChars = 900
	}
	if len([]rune(reference)) <= maxChars {
		return reference
	}
	return strings.TrimRight(string([]rune(reference)[:maxChars]), " ") + "..."
}

func maxInt(a int, b int) int {
	if a > b {
		return a
	}
	return b
}
