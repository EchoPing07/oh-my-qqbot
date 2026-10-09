package replyeffect

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// This file mirrors 参考/MaiBot/src/maisaka/reply_effect/tracker.py
// （行为以该文件为权威）：会话级回复效果观察器。
//
// 与 Python 版的差异：
//   - asyncio → goroutine + mutex；每条记录的「观察窗口计时器」是一个
//     time.AfterFunc（Clock 可注入，测试用假时钟）。
//   - 评审并发用带缓冲 channel 信号量限制（EVALUATION_CONCURRENCY）。
//   - 落库改走 Store 接口（Python 版为 JSON 镜像 + SQL 摘要的复合存储）。
//
// 会话标识：Runtime 由装配层适配（Python 版直接读 chat_stream）。

// Runtime 是 tracker 需要的会话信息边界（Python 版读 chat_stream）。
type Runtime interface {
	SessionID() string
	SessionName() string
	Platform() string
	PlatformTypeID() string
	IsGroupSession() bool
	GroupID() string
	UserID() string
}

// RuntimeFunc 是 Runtime 的函数式适配器（便于装配与测试）。
type RuntimeFunc struct {
	SessionIDValue      string
	SessionNameValue    string
	PlatformValue       string
	PlatformTypeIDValue string
	IsGroupValue        bool
	GroupIDValue        string
	UserIDValue         string
}

// SessionID implements Runtime.
func (r RuntimeFunc) SessionID() string { return r.SessionIDValue }

// SessionName implements Runtime.
func (r RuntimeFunc) SessionName() string { return r.SessionNameValue }

// Platform implements Runtime.
func (r RuntimeFunc) Platform() string { return r.PlatformValue }

// PlatformTypeID implements Runtime.
func (r RuntimeFunc) PlatformTypeID() string {
	if r.PlatformTypeIDValue != "" {
		return r.PlatformTypeIDValue
	}
	return r.SessionIDValue
}

// IsGroupSession implements Runtime.
func (r RuntimeFunc) IsGroupSession() bool { return r.IsGroupValue }

// GroupID implements Runtime.
func (r RuntimeFunc) GroupID() string { return r.GroupIDValue }

// UserID implements Runtime.
func (r RuntimeFunc) UserID() string { return r.UserIDValue }

// Clock 抽象当前时间与延迟执行（测试注入假时钟）。
type Clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, fn func()) Timer
}

// Timer 是 AfterFunc 返回的计时器。
type Timer interface {
	Stop() bool
}

// SystemClock 是默认时钟（time 包）。
type SystemClock struct{}

// Now implements Clock.
func (SystemClock) Now() time.Time { return time.Now() }

// AfterFunc implements Clock.
func (SystemClock) AfterFunc(d time.Duration, fn func()) Timer { return time.AfterFunc(d, fn) }

// TrackerConfig 汇总 tracker 的全部可注入参数（配置值经此处传入，本包不读配置）。
type TrackerConfig struct {
	// Enabled mirrors debug.enable_reply_effect_tracking（默认关）。
	Enabled bool
	// BotIdentity mirrors global_config.bot（judge prompt 用）。
	BotIdentity BotIdentity
	// ObservationWindow mirrors OBSERVATION_WINDOW_SECONDS（默认 30min）。
	ObservationWindow time.Duration
	// FollowupLimit mirrors SESSION_FOLLOWUP_LIMIT（默认 15）。
	FollowupLimit int
	// EvaluationTimeout mirrors EVALUATION_TOTAL_TIMEOUT_SECONDS（默认 60s）。
	EvaluationTimeout time.Duration
	// EvaluationConcurrency mirrors EVALUATION_CONCURRENCY（默认 2）。
	EvaluationConcurrency int
	// MaxRecordsPerSession mirrors log.maisaka_reply_effect_limit（默认 256）。
	MaxRecordsPerSession int
	// MaxContextItems 限制上下文快照条数（0 表示不裁剪）。
	MaxContextItems int
	// JudgeRunner 是 LLM 评审边界（nil 时评审直接失败，链路仍可用）。
	JudgeRunner JudgeRunner
	// Store 是持久化边界（nil 时使用内存 store）。
	Store Store
	// Clock 是时钟（nil 时使用系统时钟）。
	Clock Clock
	// Logger 是日志器（nil 时使用 slog.Default）。
	Logger *slog.Logger
	// MirrorDir 非空时额外写 JSON 诊断镜像（对齐 Python 的 logs/maisaka_reply_effect）。
	MirrorDir string
}

func (c TrackerConfig) normalized() TrackerConfig {
	if c.ObservationWindow <= 0 {
		c.ObservationWindow = ObservationWindow
	}
	if c.FollowupLimit <= 0 {
		c.FollowupLimit = SessionFollowupLimit
	}
	if c.EvaluationTimeout <= 0 {
		c.EvaluationTimeout = EvaluationTotalTimeout
	}
	if c.EvaluationConcurrency <= 0 {
		c.EvaluationConcurrency = EvaluationConcurrency
	}
	if c.MaxRecordsPerSession <= 0 {
		c.MaxRecordsPerSession = DefaultMaxRecordsPerSession
	}
	if c.Clock == nil {
		c.Clock = SystemClock{}
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Store == nil {
		c.Store = NewMemoryStore()
	}
	return c
}

// Tracker mirrors ReplyEffectTracker：单个会话内的回复效果观察器。
type Tracker struct {
	cfg     TrackerConfig
	runtime Runtime

	mu      sync.Mutex
	pending map[string]*Record
	tracked map[string]*Record
	timers  map[string]Timer
	// evaluating 记录仍在评审中的 effect_id（wait_for_idle 用）。
	evaluating map[string]bool
	// changed 是一次评审中被同步修改、需要在锁外落库的候选记录。
	changed []*Record
	closed  bool
	sem     chan struct{}
	wg      sync.WaitGroup
	started bool
}

// NewTracker 构造会话级 tracker。
func NewTracker(runtime Runtime, cfg TrackerConfig) *Tracker {
	normalized := cfg.normalized()
	concurrency := normalized.EvaluationConcurrency
	return &Tracker{
		cfg:        normalized,
		runtime:    runtime,
		pending:    map[string]*Record{},
		tracked:    map[string]*Record{},
		timers:     map[string]Timer{},
		evaluating: map[string]bool{},
		sem:        make(chan struct{}, concurrency),
	}
}

// Enabled 返回观察是否启用（构造参数默认关）。
func (t *Tracker) Enabled() bool { return t.cfg.Enabled }

// Start mirrors ReplyEffectTracker.start：恢复未完成记录并重建观察计时器/评审任务。
func (t *Tracker) Start(ctx context.Context) {
	if !t.cfg.Enabled {
		return
	}
	t.mu.Lock()
	if t.started {
		t.mu.Unlock()
		return
	}
	t.started = true
	sessionID := t.sessionID()
	t.mu.Unlock()

	restored, err := t.cfg.Store.LoadUnfinished(ctx, sessionID)
	if err != nil {
		t.cfg.Logger.Warn("恢复回复效果记录失败", "session_id", sessionID, "error", err)
		return
	}
	related := map[string]bool{}
	for _, record := range restored {
		for index := range record.FollowupMessages {
			for _, effectID := range record.FollowupMessages[index].CandidateEffectIDs {
				related[effectID] = true
			}
			for _, association := range record.FollowupMessages[index].Associations {
				related[association.EffectID] = true
			}
		}
	}
	if len(related) > 0 {
		ids := make([]string, 0, len(related))
		for effectID := range related {
			ids = append(ids, effectID)
		}
		if records, err := t.cfg.Store.LoadByIDs(ctx, ids); err == nil {
			t.mu.Lock()
			for _, record := range records {
				t.tracked[record.EffectID] = record
			}
			t.mu.Unlock()
		}
	}

	type ready struct {
		effectID string
		reason   string
	}
	readyList := make([]ready, 0, len(restored))
	now := t.cfg.Clock.Now()
	for _, record := range restored {
		originalStatus := record.Status
		record.Status = StatusPending
		elapsed := now.Sub(mustParseTimestamp(record.CreatedAt))
		if elapsed < 0 {
			elapsed = 0
		}
		remaining := t.cfg.ObservationWindow - elapsed
		t.mu.Lock()
		t.pending[record.EffectID] = record
		t.tracked[record.EffectID] = record
		t.mu.Unlock()
		switch {
		case originalStatus == StatusEvaluating:
			reason := record.FinalizeReason
			if reason == "" {
				reason = ReasonRuntimeRecovery
			}
			readyList = append(readyList, ready{record.EffectID, reason})
		case len(record.FollowupMessages) >= t.cfg.FollowupLimit:
			readyList = append(readyList, ready{record.EffectID, ReasonFollowupsLimit})
		case remaining <= 0:
			readyList = append(readyList, ready{record.EffectID, ReasonRuntimeRecovery})
		default:
			t.armTimer(record.EffectID, remaining)
		}
	}
	for _, item := range readyList {
		t.scheduleEvaluation(ctx, item.effectID, item.reason)
	}
	if len(restored) > 0 {
		t.cfg.Logger.Info("恢复回复效果记录", "session_id", sessionID,
			"total", len(restored), "ready", len(readyList), "waiting", len(restored)-len(readyList))
	}
}

// TrackReplyRequest 是 RegisterReply 的登记入参
// （对齐 runtime.track_reply_effect 与 ReplyEffectTracker.record_reply）。
type TrackReplyRequest struct {
	ToolCallID         string
	TargetMessageID    string
	TargetUser         UserSnapshot
	SetQuote           bool
	ReplyText          string
	ReplySegments      []string
	PlannerReasoning   string
	ToolContext        map[string]any
	SendResults        []map[string]any
	ReplyMetadata      map[string]any
	ContextSnapshot    []ContextItem
	ModelName          string
	RequestFingerprint string
	PromptFingerprint  string
	Now                time.Time
}

// RegisterReply mirrors ReplyEffectTracker.record_reply：登记一条已发送的回复。
// 返回 nil 表示观察未启用（调用方无需处理）。
func (t *Tracker) RegisterReply(ctx context.Context, request TrackReplyRequest) (*Record, error) {
	if !t.cfg.Enabled {
		return nil, nil
	}
	t.Start(ctx)
	now := request.Now
	if now.IsZero() {
		now = t.cfg.Clock.Now()
	}
	createdAt := FormatTimestamp(now)
	sentMessageIDs := make([]string, 0, len(request.SendResults))
	for _, result := range request.SendResults {
		if messageID := trimSpace(toStringValue(result["message_id"])); messageID != "" {
			sentMessageIDs = append(sentMessageIDs, messageID)
		}
	}
	contextSnapshot := request.ContextSnapshot
	if t.cfg.MaxContextItems > 0 && len(contextSnapshot) > t.cfg.MaxContextItems {
		contextSnapshot = contextSnapshot[len(contextSnapshot)-t.cfg.MaxContextItems:]
	}
	preActivity := countPreActivity(contextSnapshot, now)
	record := &Record{
		EffectID:  newEffectID(),
		Status:    StatusPending,
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
		Session: SessionSnapshot{
			SessionID:      t.sessionID(),
			PlatformTypeID: t.runtime.PlatformTypeID(),
			Platform:       t.runtime.Platform(),
			ChatType:       t.chatType(),
			GroupID:        t.runtime.GroupID(),
			UserID:         t.runtime.UserID(),
			SessionName:    t.runtime.SessionName(),
		},
		Reply: ReplySnapshot{
			ToolCallID:         request.ToolCallID,
			TargetMessageID:    request.TargetMessageID,
			SetQuote:           request.SetQuote,
			ReplyText:          request.ReplyText,
			ReplySegments:      append([]string(nil), request.ReplySegments...),
			PlannerReasoning:   request.PlannerReasoning,
			SentMessageIDs:     sentMessageIDs,
			ModelName:          request.ModelName,
			RequestFingerprint: request.RequestFingerprint,
			PromptFingerprint:  request.PromptFingerprint,
			StrategyPrimary:    "other",
			StrategySecondary:  []string{},
			ToolContext:        request.ToolContext,
			SendResults:        request.SendResults,
			ReplyMetadata:      request.ReplyMetadata,
		},
		TargetUser:        request.TargetUser,
		PreActivityCount:  preActivity,
		PreActivityBucket: ActivityBucket(preActivity),
		ContextSnapshot:   contextSnapshot,
		EvaluationVersion: EvaluationVersion,
		SchemaVersion:     SchemaVersion,
	}
	if err := t.cfg.Store.Save(ctx, record); err != nil {
		return nil, err
	}
	t.writeMirror(record)
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return record, nil
	}
	t.pending[record.EffectID] = record
	t.tracked[record.EffectID] = record
	t.mu.Unlock()
	t.armTimer(record.EffectID, t.cfg.ObservationWindow)
	return record, nil
}

// ObserveUserMessage mirrors ReplyEffectTracker.observe_user_message：
// 把消息写入当时所有 pending 候选，并锁定显式引用关系。
func (t *Tracker) ObserveUserMessage(ctx context.Context, message *ObservedMessage) {
	if !t.cfg.Enabled || message == nil {
		return
	}
	if message.SessionID != "" && message.SessionID != t.sessionID() {
		return
	}
	t.Start(ctx)
	readyIDs := make([]string, 0)
	t.mu.Lock()
	if len(t.pending) == 0 {
		t.mu.Unlock()
		return
	}
	candidateIDs := make([]string, 0, len(t.pending))
	for effectID := range t.pending {
		candidateIDs = append(candidateIDs, effectID)
	}
	sort.Strings(candidateIDs)
	sentIDToEffect := map[string]string{}
	for effectID, record := range t.pending {
		for _, messageID := range record.Reply.SentMessageIDs {
			sentIDToEffect[messageID] = effectID
		}
	}
	for effectID, record := range t.pending {
		if record.Status != StatusPending {
			continue
		}
		followup := t.buildFollowupSnapshot(message, record, candidateIDs)
		quotedIDs := map[string]bool{}
		for _, quoted := range followup.QuoteTargetIDs {
			quotedIDs[quoted] = true
		}
		if followup.ReplyTo != "" {
			quotedIDs[followup.ReplyTo] = true
		}
		for quoted := range quotedIDs {
			if quotedEffectID, ok := sentIDToEffect[quoted]; ok {
				followup.Associations = append(followup.Associations, ReplyAssociation{
					EffectID:              quotedEffectID,
					AttributionType:       AttributionExplicitQuote,
					AttributionConfidence: 1.0,
					StanceTarget:          "bot_content",
					Stance:                "neutral",
					Contribution:          "maintain",
					EvaluatorConfidence:   0.0,
				})
			}
		}
		record.FollowupMessages = append(record.FollowupMessages, followup)
		record.UpdatedAt = FormatTimestamp(t.cfg.Clock.Now())
		if err := t.cfg.Store.Save(ctx, record); err == nil {
			t.writeMirror(record)
		}
		if len(record.FollowupMessages) >= t.cfg.FollowupLimit {
			readyIDs = append(readyIDs, effectID)
		}
	}
	t.mu.Unlock()
	for _, effectID := range readyIDs {
		t.scheduleEvaluation(ctx, effectID, ReasonFollowupsLimit)
	}
}

// FinalizeAll mirrors finalize_all（默认 reason="runtime_stop"）。
func (t *Tracker) FinalizeAll(ctx context.Context, reason string) {
	if !t.cfg.Enabled {
		return
	}
	if reason == "" {
		reason = ReasonRuntimeStop
	}
	t.mu.Lock()
	ids := make([]string, 0, len(t.pending))
	for effectID := range t.pending {
		ids = append(ids, effectID)
	}
	t.mu.Unlock()
	for _, effectID := range ids {
		t.scheduleEvaluation(ctx, effectID, reason)
	}
	t.WaitForIdle()
}

// Finalize mirrors ReplyEffectTracker.finalize（单条记录结算）。
func (t *Tracker) Finalize(ctx context.Context, effectID, reason string) {
	t.scheduleEvaluation(ctx, effectID, reason)
}

// WaitForIdle 等待当前登记的评审任务结束（供停机与测试使用）。
func (t *Tracker) WaitForIdle() {
	for {
		t.mu.Lock()
		count := len(t.evaluating)
		t.mu.Unlock()
		if count == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// Close mirrors stop/clear：取消观察计时器并结算未完成记录。
func (t *Tracker) Close(ctx context.Context) {
	t.mu.Lock()
	t.closed = true
	timers := make([]Timer, 0, len(t.timers))
	for _, timer := range t.timers {
		timers = append(timers, timer)
	}
	t.timers = map[string]Timer{}
	ids := make([]string, 0, len(t.pending))
	for effectID := range t.pending {
		ids = append(ids, effectID)
	}
	t.mu.Unlock()
	for _, timer := range timers {
		timer.Stop()
	}
	for _, effectID := range ids {
		t.scheduleEvaluation(ctx, effectID, ReasonRuntimeStop)
	}
	t.wg.Wait()
}

// Records 返回当前跟踪的全部记录（按创建时间升序，WebUI/测试读取用）。
func (t *Tracker) Records() []*Record {
	t.mu.Lock()
	defer t.mu.Unlock()
	records := make([]*Record, 0, len(t.tracked))
	for _, record := range t.tracked {
		records = append(records, record.Clone())
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].CreatedAt == records[j].CreatedAt {
			return records[i].EffectID < records[j].EffectID
		}
		return records[i].CreatedAt < records[j].CreatedAt
	})
	return records
}

// PendingCount 返回仍在观察窗口内的记录数（测试用）。
func (t *Tracker) PendingCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.pending)
}

// ---------------------------------------------------------------------------
// 内部：计时器 / 结算 / 评审
// ---------------------------------------------------------------------------

func (t *Tracker) armTimer(effectID string, delay time.Duration) {
	if delay < 0 {
		delay = 0
	}
	timer := t.cfg.Clock.AfterFunc(delay, func() {
		t.scheduleEvaluation(context.Background(), effectID, ReasonWindowTimeout)
	})
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		timer.Stop()
		return
	}
	t.timers[effectID] = timer
	t.mu.Unlock()
}

// scheduleEvaluation mirrors _schedule_evaluation：原子关闭观察窗口，
// 并保证每条记录只创建一个评审任务。
func (t *Tracker) scheduleEvaluation(ctx context.Context, effectID, reason string) {
	t.mu.Lock()
	record, ok := t.pending[effectID]
	if !ok || record.Status != StatusPending {
		t.mu.Unlock()
		return
	}
	delete(t.pending, effectID)
	if timer, ok := t.timers[effectID]; ok {
		timer.Stop()
		delete(t.timers, effectID)
	}
	if reason == ReasonFollowupsLimit && len(record.FollowupMessages) > t.cfg.FollowupLimit {
		record.FollowupMessages = record.FollowupMessages[:t.cfg.FollowupLimit]
	}
	if !CompleteObservationReasons[reason] {
		record.EvaluationVersion = EvaluationVersion
		record.Status = StatusIncomplete
		record.Scores = nil
		record.FinalizedAt = FormatTimestamp(t.cfg.Clock.Now())
		record.UpdatedAt = record.FinalizedAt
		record.FinalizeReason = reason
		record.ConfidenceNote = buildConfidenceNote(record)
		record.FollowupSummary = buildFollowupSummary(record)
		t.mu.Unlock()
		if err := t.cfg.Store.Save(ctx, record); err == nil {
			t.writeMirror(record)
		}
		return
	}
	record.Status = StatusEvaluating
	record.FinalizeReason = reason
	record.UpdatedAt = FormatTimestamp(t.cfg.Clock.Now())
	t.evaluating[effectID] = true
	t.wg.Add(1)
	t.mu.Unlock()
	if err := t.cfg.Store.Save(ctx, record); err == nil {
		t.writeMirror(record)
	}
	go func() {
		defer t.wg.Done()
		t.evaluateRecord(record, reason)
		t.mu.Lock()
		delete(t.evaluating, effectID)
		t.mu.Unlock()
	}()
}

// evaluateRecord mirrors _evaluate_record：有限并发下执行语义归因、评分与落库。
func (t *Tracker) evaluateRecord(record *Record, reason string) {
	t.sem <- struct{}{}
	defer func() { <-t.sem }()

	ctx, cancel := context.WithTimeout(context.Background(), t.cfg.EvaluationTimeout)
	defer cancel()

	// 在锁内克隆工作副本：随访写入与关联同步都在锁内改字段，避免读写竞争。
	t.mu.Lock()
	work := record.Clone()
	t.mu.Unlock()

	candidates := t.candidateRecords(work)
	judgment, judgeErr := JudgeReplyEffect(ctx, work, candidates, t.cfg.BotIdentity, t.cfg.JudgeRunner)

	// 把工作副本发布回跟踪表，并把评审关联边同步到全部候选记录（含自身）。
	t.mu.Lock()
	work.EvaluationVersion = EvaluationVersion
	if judgeErr != nil {
		work.Status = StatusEvaluationFailed
		work.EvaluationError = judgeErr.Error()
	} else {
		work.Reply.StrategyPrimary = judgment.Primary
		work.Reply.StrategySecondary = judgment.Secondary
		work.Reply.StrategyConfidence = judgment.StrategyConfidence
		t.tracked[work.EffectID] = work
		t.changed = t.applyAssociationsLocked(judgment.Associations)
		work.Scores = ScoreRecord(work)
		work.Status = StatusFinalized
	}
	work.FinalizedAt = FormatTimestamp(t.cfg.Clock.Now())
	work.UpdatedAt = work.FinalizedAt
	work.FinalizeReason = reason
	work.ConfidenceNote = buildConfidenceNote(work)
	work.FollowupSummary = buildFollowupSummary(work)
	t.tracked[work.EffectID] = work
	t.pruneSessionRecords()
	changed := append([]*Record{}, t.changed...)
	t.changed = nil
	t.mu.Unlock()

	if work.Status == StatusEvaluationFailed {
		t.cfg.Logger.Warn("回复效果评审失败",
			"effect_id", work.EffectID, "session_id", work.Session.SessionID, "error", work.EvaluationError)
	}
	records := append(changed, work)
	for _, item := range records {
		if err := t.cfg.Store.Save(context.Background(), item); err != nil {
			t.cfg.Logger.Debug("保存回复效果记录失败", "effect_id", item.EffectID, "error", err)
			continue
		}
		t.writeMirror(item)
	}
}

// candidateRecords mirrors _evaluate_record 的候选排序：
// 自身优先，其余按与当前记录创建时间的接近程度排序。
func (t *Tracker) candidateRecords(record *Record) []*Record {
	ids := map[string]bool{record.EffectID: true}
	for index := range record.FollowupMessages {
		for _, effectID := range record.FollowupMessages[index].CandidateEffectIDs {
			ids[effectID] = true
		}
	}
	t.mu.Lock()
	candidates := make([]*Record, 0, len(ids))
	for effectID := range ids {
		if tracked, ok := t.tracked[effectID]; ok {
			candidates = append(candidates, tracked)
		}
	}
	t.mu.Unlock()
	base := mustParseTimestamp(record.CreatedAt)
	sort.Slice(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		leftSelf := left.EffectID != record.EffectID
		rightSelf := right.EffectID != record.EffectID
		if leftSelf != rightSelf {
			return !leftSelf
		}
		leftDelta := absDuration(mustParseTimestamp(left.CreatedAt).Sub(base))
		rightDelta := absDuration(mustParseTimestamp(right.CreatedAt).Sub(base))
		if leftDelta == rightDelta {
			return left.EffectID < right.EffectID
		}
		return leftDelta < rightDelta
	})
	return candidates
}

// applyAssociationsLocked mirrors _apply_associations：把批量评审的关联边同步到
// 所有仍保留的候选记录（显式引用关联保持锁定）。返回被修改的记录（调用方在锁外落库）。
// 调用方必须持有 t.mu。
func (t *Tracker) applyAssociationsLocked(evaluated map[string][]ReplyAssociation) []*Record {
	changedRecords := make([]*Record, 0)
	for _, tracked := range t.tracked {
		changed := false
		for index := range tracked.FollowupMessages {
			followup := &tracked.FollowupMessages[index]
			parsed, ok := evaluated[followup.MessageID]
			if !ok {
				continue
			}
			existing := map[string]ReplyAssociation{}
			order := make([]string, 0, len(followup.Associations))
			for _, association := range followup.Associations {
				if _, ok := existing[association.EffectID]; !ok {
					order = append(order, association.EffectID)
				}
				existing[association.EffectID] = association
			}
			for _, association := range parsed {
				locked, ok := existing[association.EffectID]
				if ok && locked.AttributionType == AttributionExplicitQuote {
					association.AttributionType = AttributionExplicitQuote
					association.AttributionConfidence = 1.0
				}
				if _, ok := existing[association.EffectID]; !ok {
					order = append(order, association.EffectID)
				}
				existing[association.EffectID] = association
			}
			merged := make([]ReplyAssociation, 0, len(existing))
			for _, effectID := range order {
				merged = append(merged, existing[effectID])
			}
			followup.Associations = merged
			changed = true
		}
		if changed && (tracked.Status == StatusPending || tracked.Status == StatusEvaluating) {
			changedRecords = append(changedRecords, tracked)
		}
	}
	return changedRecords
}

// pruneSessionRecords 按会话上限裁剪跟踪表（对齐 ReplyEffectStorage 的裁剪语义：
// 保留最新的 MaxRecordsPerSession 条，仅裁剪已结算记录）。
func (t *Tracker) pruneSessionRecords() {
	limit := t.cfg.MaxRecordsPerSession
	if limit <= 0 || len(t.tracked) <= limit {
		return
	}
	finalized := make([]*Record, 0, len(t.tracked))
	for _, record := range t.tracked {
		if record.Status == StatusPending || record.Status == StatusEvaluating {
			continue
		}
		finalized = append(finalized, record)
	}
	sort.Slice(finalized, func(i, j int) bool {
		if finalized[i].CreatedAt == finalized[j].CreatedAt {
			return finalized[i].EffectID > finalized[j].EffectID
		}
		return finalized[i].CreatedAt > finalized[j].CreatedAt
	})
	for index := limit; index < len(finalized); index++ {
		delete(t.tracked, finalized[index].EffectID)
	}
}

func (t *Tracker) buildFollowupSnapshot(message *ObservedMessage, record *Record, candidateIDs []string) FollowupSnapshot {
	userID := trimSpace(message.UserID)
	return FollowupSnapshot{
		MessageID:          message.MessageID,
		Timestamp:          FormatTimestamp(message.Timestamp),
		UserID:             userID,
		Nickname:           message.Nickname,
		Cardname:           message.Cardname,
		VisibleText:        message.VisibleText,
		PlainText:          message.PlainText,
		LatencySeconds:     roundTo(maxDurationSeconds(message.Timestamp.Sub(mustParseTimestamp(record.CreatedAt))), 3),
		IsTargetUser:       record.TargetUser.UserID != "" && userID == record.TargetUser.UserID,
		ReplyTo:            message.ReplyTo,
		QuoteTargetIDs:     append([]string(nil), message.QuoteTargetIDs...),
		CandidateEffectIDs: append([]string(nil), candidateIDs...),
		Associations:       []ReplyAssociation{},
		Attachments:        append([]VisualAttachment(nil), message.Attachments...),
	}
}

func (t *Tracker) writeMirror(record *Record) {
	if t.cfg.MirrorDir == "" {
		return
	}
	dir := filepath.Join(t.cfg.MirrorDir, sanitizePathSegment(record.Session.PlatformTypeID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	payload, err := encodeRecordJSON(record)
	if err != nil {
		return
	}
	path := filepath.Join(dir, fmt.Sprintf("%d_%s.json",
		t.cfg.Clock.Now().UnixMilli(), strings.ReplaceAll(record.EffectID, "-", "")))
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.cfg.Logger.Debug("写回复效果镜像失败", "path", path, "error", err)
	}
}

func (t *Tracker) sessionID() string {
	if t.runtime == nil {
		return ""
	}
	return t.runtime.SessionID()
}

func (t *Tracker) chatType() string {
	if t.runtime != nil && t.runtime.IsGroupSession() {
		return "group"
	}
	return "private"
}

// ObservedMessage 是观察到的后续用户消息（装配层由 SessionMessage 适配）。
type ObservedMessage struct {
	SessionID      string
	MessageID      string
	Timestamp      time.Time
	UserID         string
	Nickname       string
	Cardname       string
	VisibleText    string
	PlainText      string
	ReplyTo        string
	QuoteTargetIDs []string
	Attachments    []VisualAttachment
}

// ---------------------------------------------------------------------------
// 文案与摘要
// ---------------------------------------------------------------------------

// buildConfidenceNote mirrors _build_confidence_note。
func buildConfidenceNote(record *Record) string {
	switch {
	case record.Status == StatusEvaluationFailed:
		return "评审输出校验失败，本记录未参与策略统计。"
	case record.Status == StatusIncomplete:
		return "观察窗口不完整，未进行评分。"
	case len(record.FollowupMessages) == 0:
		return "观察窗口内没有后续用户消息。"
	default:
		return "已完成完整观察窗口与语义归因。"
	}
}

// buildFollowupSummary mirrors _build_followup_summary。
func buildFollowupSummary(record *Record) FollowupSummary {
	associatedIDs := map[string]bool{}
	participants := map[string]bool{}
	for index := range record.FollowupMessages {
		followup := &record.FollowupMessages[index]
		for _, association := range followup.Associations {
			if association.EffectID == record.EffectID {
				associatedIDs[followup.MessageID] = true
				participants[followup.UserID] = true
				break
			}
		}
	}
	return FollowupSummary{
		TotalCount:       len(record.FollowupMessages),
		AssociatedCount:  len(associatedIDs),
		ParticipantCount: len(participants),
	}
}

// countPreActivity mirrors _count_pre_activity：Bot 回复前 2 分钟内的用户消息数。
func countPreActivity(contextSnapshot []ContextItem, now time.Time) int {
	cutoff := now.Add(-PreActivityWindow)
	count := 0
	for index := range contextSnapshot {
		item := &contextSnapshot[index]
		if item.Role == "assistant" {
			continue
		}
		timestamp, err := ParseTimestamp(item.Timestamp)
		if err != nil {
			continue
		}
		if !timestamp.Before(cutoff) {
			count++
		}
	}
	return count
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func mustParseTimestamp(value string) time.Time {
	parsed, err := ParseTimestamp(value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func absDuration(value time.Duration) time.Duration {
	if value < 0 {
		return -value
	}
	return value
}

func maxDurationSeconds(value time.Duration) float64 {
	if value < 0 {
		return 0
	}
	return value.Seconds()
}

func sanitizePathSegment(value string) string {
	normalized := strings.TrimSpace(value)
	if normalized == "" {
		return "unknown_chat"
	}
	replacer := strings.NewReplacer("/", "_", "\\", "_", ":", "_", "..", "_", "*", "_", "?", "_")
	return replacer.Replace(normalized)
}

// newEffectID 生成 36 字符 UUID v4（对齐 Python uuid.uuid4）。
func newEffectID() string {
	return newUUIDv4()
}
