package memory

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	contracts "oh-my-qqbot/internal"
	"oh-my-qqbot/internal/message"
)

// ---------------------------------------------------------------------------
// 测试脚手架
// ---------------------------------------------------------------------------

// fakeKernel 汇总三个写回能力（记录调用参数）。
type fakeKernel struct {
	mu          sync.Mutex
	summaries   []string
	summaryChat []string
	images      []string
	imageChat   []string
	facts       []contracts.ProfileFact
	factPersons []string
}

func (f *fakeKernel) IngestSummary(_ context.Context, chatID string, summary string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.summaries = append(f.summaries, summary)
	f.summaryChat = append(f.summaryChat, chatID)
	return nil
}

func (f *fakeKernel) IngestImageMemory(_ context.Context, chatID string, description string, hash string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.images = append(f.images, description)
	f.imageChat = append(f.imageChat, chatID)
	return nil
}

func (f *fakeKernel) UpsertPersonFact(_ context.Context, personID string, _ string, fact contracts.ProfileFact) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.facts = append(f.facts, fact)
	f.factPersons = append(f.factPersons, personID)
	return nil
}

func (f *fakeKernel) SummaryCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.summaries)
}

func (f *fakeKernel) ImageCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.images)
}

func (f *fakeKernel) FactCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.facts)
}

// fakeRepository 是内存消息仓储（支持 session/message_id/before_time 查询）。
type fakeRepository struct {
	mu       sync.Mutex
	messages []*message.SessionMessage
}

func (r *fakeRepository) add(msg *message.SessionMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.messages = append(r.messages, msg)
}

func (r *fakeRepository) Query(_ context.Context, query MessageQuery) ([]*message.SessionMessage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*message.SessionMessage, 0, len(r.messages))
	for _, msg := range r.messages {
		if query.SessionID != "" && msg.SessionID != query.SessionID {
			continue
		}
		if query.MessageID != "" && msg.MessageID != query.MessageID {
			continue
		}
		if query.BeforeTime != nil && msg.Timestamp.After(*query.BeforeTime) {
			continue
		}
		if query.FilterBot && msg.MessageInfo.UserInfo.UserID == "bot-1" {
			continue
		}
		out = append(out, msg)
	}
	if query.Limit > 0 && len(out) > query.Limit {
		out = out[len(out)-query.Limit:]
	}
	return out, nil
}

func (r *fakeRepository) Count(_ context.Context, sessionID string, endTime *time.Time) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, msg := range r.messages {
		if sessionID != "" && msg.SessionID != sessionID {
			continue
		}
		if endTime != nil && msg.Timestamp.After(*endTime) {
			continue
		}
		count++
	}
	return count, nil
}

func (r *fakeRepository) ByID(_ context.Context, sessionID string, messageID string) ([]*message.SessionMessage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*message.SessionMessage, 0, 1)
	for _, msg := range r.messages {
		if sessionID != "" && msg.SessionID != sessionID {
			continue
		}
		if msg.MessageID != messageID {
			continue
		}
		out = append(out, msg)
	}
	return out, nil
}

// newFlowMessage 构造一条测试消息。
func newFlowMessage(sessionID string, messageID string, userID string, text string, at time.Time) *message.SessionMessage {
	msg := message.NewSessionMessage(messageID, at, "qq")
	msg.SessionID = sessionID
	msg.MessageInfo.UserInfo = message.NewUserInfo(userID, "阿强", "")
	msg.ProcessedPlainText = message.StringPtr(text)
	return msg
}

func testPersonID(platform string, userID string) string { return platform + ":" + userID }

func testIsBotSelf(_ string, userID string) bool { return userID == "bot-1" }

// testPrivateSessionLookup 返回私聊会话身份（session.user_id = 对端用户）。
func testPrivateSessionLookup(sessionID string, platform string, userID string) SessionLookupFunc {
	return func(candidate string) (*SessionIdentity, bool) {
		if candidate != sessionID {
			return nil, false
		}
		return &SessionIdentity{SessionID: sessionID, Platform: platform, UserID: userID}, true
	}
}

// waitFor 轮询等待条件成立（上限 2s）。
func waitFor(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", description)
}

// ---------------------------------------------------------------------------
// PersonFactWritebackService
// ---------------------------------------------------------------------------

func TestPersonFactWorkerExtractsAndWritesBack(t *testing.T) {
	repository := &fakeRepository{}
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.Local)
	now = now.Add(-time.Minute)
	repository.add(newFlowMessage("s1", "u1", "user-1", "我对青霉素过敏", now))

	kernel := &fakeKernel{}
	llm := &fakeLLM{response: `[{"fact":"阿强对青霉素过敏","evidence_message_id":"u1","evidence_quote":"我对青霉素过敏"}]`}
	worker := NewPersonFactWritebackService()
	worker.Enabled = true
	worker.LLM = llm.Call
	worker.Writer = kernel
	worker.Messages = repository.Query
	worker.Counts = repository.Count
	worker.ByID = repository.ByID
	worker.IsBotSelf = testIsBotSelf
	worker.PersonID = testPersonID
	worker.Sessions = testPrivateSessionLookup("s1", "qq", "user-1")

	reply := newFlowMessage("s1", "b1", "bot-1", "记下了，别乱吃东西", now.Add(30*time.Second))
	reply.ReplyTo = message.StringPtr("u1")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker.Start(ctx)
	defer worker.Shutdown()

	if !worker.Enqueue(reply) {
		t.Fatalf("入队应成功")
	}
	waitFor(t, "人物事实写回", func() bool { return kernel.FactCount() == 1 })
	if worker.Processed() != 1 {
		t.Fatalf("应处理 1 条: %d", worker.Processed())
	}
	if !strings.Contains(kernel.facts[0].Content, "青霉素") {
		t.Fatalf("事实内容应来自抽取结果: %q", kernel.facts[0].Content)
	}
	if kernel.factPersons[0] != "qq:user-1" {
		t.Fatalf("person_id 应按平台+用户生成: %q", kernel.factPersons[0])
	}
	if kernel.facts[0].Source != "user_supported:server_verified" {
		t.Fatalf("直证校验通过应标记 server_verified: %q", kernel.facts[0].Source)
	}
}

func TestPersonFactWorkerSkipsEphemeralAndDisabled(t *testing.T) {
	kernel := &fakeKernel{}
	worker := NewPersonFactWritebackService()
	worker.Enabled = true
	worker.LLM = (&fakeLLM{response: `[{"fact":"阿强很好","evidence_message_id":"u1","evidence_quote":"我很好"}]`}).Call
	worker.Writer = kernel
	worker.ByID = (&fakeRepository{}).ByID
	worker.IsBotSelf = testIsBotSelf
	worker.PersonID = testPersonID

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker.Start(ctx)
	defer worker.Shutdown()

	if worker.Enqueue(newFlowMessage("s1", "b1", "bot-1", "好的", time.Now())) {
		waitFor(t, "短寒暄应被跳过", func() bool { return worker.Processed() == 1 })
	}
	if kernel.FactCount() != 0 {
		t.Fatalf("寒暄不应写入事实: %d", kernel.FactCount())
	}

	disabled := NewPersonFactWritebackService()
	disabled.Enabled = false
	disabled.Start(ctx)
	defer disabled.Shutdown()
	if disabled.Enqueue(newFlowMessage("s1", "b2", "bot-1", "这是一段足够长的回复内容", time.Now())) {
		t.Fatalf("未启用时不应入队")
	}
}

func TestPersonFactVerifyRequiresExactQuote(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.Local)
	repository := &fakeRepository{}
	repository.add(newFlowMessage("s1", "u1", "user-1", "我对青霉素过敏", now))
	repository.add(newFlowMessage("s1", "u2", "user-2", "我对青霉素过敏", now))

	base := VerifyFactInput{
		Fact:              "阿强对青霉素过敏",
		EvidenceMessageID: "u1",
		EvidenceQuote:     "我对青霉素过敏",
		PersonID:          "qq:user-1",
		PersonName:        "阿强",
		SessionID:         "s1",
		ByID:              repository.ByID,
		IsBotSelf:         testIsBotSelf,
		PersonIDFor:       testPersonID,
	}
	if !VerifyDirectPersonFact(context.Background(), base) {
		t.Fatalf("直接自述应通过校验")
	}

	wrongPerson := base
	wrongPerson.PersonID = "qq:user-2"
	if VerifyDirectPersonFact(context.Background(), wrongPerson) {
		t.Fatalf("人物不匹配不应通过")
	}

	temporary := base
	temporary.EvidenceQuote = "我今天对青霉素过敏"
	if VerifyDirectPersonFact(context.Background(), temporary) {
		t.Fatalf("含临时标记不应通过")
	}

	question := base
	question.EvidenceQuote = "我对青霉素过敏？"
	if VerifyDirectPersonFact(context.Background(), question) {
		t.Fatalf("反问不应通过")
	}
}

func TestPersonFactReverifyCursorPersists(t *testing.T) {
	dir := t.TempDir()
	calls := 0
	worker := NewPersonFactWritebackService()
	worker.KernelDataDir = dir
	worker.Reverify = func(_ context.Context, cursor string, _ int) (ReverifyResult, error) {
		calls++
		if calls == 1 && cursor != "" {
			t.Fatalf("首轮游标应为空: %q", cursor)
		}
		if calls == 2 && cursor != "claim-50" {
			t.Fatalf("第二轮应从持久化游标续跑: %q", cursor)
		}
		return ReverifyResult{Processed: 50, Promoted: 1, NextCursor: "claim-50", HasMore: calls == 1}, nil
	}
	if _, err := worker.RunFactReverifyBatch(context.Background()); err != nil {
		t.Fatalf("重验批次失败: %v", err)
	}
	progress := LoadFactReverifyProgress(dir)
	if progress.Cursor != "claim-50" || progress.ProcessedTotal != 50 || progress.PromotedTotal != 1 {
		t.Fatalf("游标应落盘: %+v", progress)
	}
	if _, err := worker.RunFactReverifyBatch(context.Background()); err != nil {
		t.Fatalf("第二轮重验失败: %v", err)
	}
	progress = LoadFactReverifyProgress(dir)
	if progress.Cursor != "" || progress.ProcessedTotal != 100 {
		t.Fatalf("has_more=false 应清空游标并累计: %+v", progress)
	}
}

// ---------------------------------------------------------------------------
// ChatSummaryWritebackService
// ---------------------------------------------------------------------------

func TestChatSummaryWorkerTriggersOnThreshold(t *testing.T) {
	repository := &fakeRepository{}
	kernel := &fakeKernel{}
	base := time.Date(2026, 1, 2, 3, 0, 0, 0, time.Local)
	for index := 0; index < 5; index++ {
		repository.add(newFlowMessage("s1", "u"+itoa(index), "user-1", "第"+itoa(index)+"条消息", base.Add(time.Duration(index)*time.Minute)))
	}

	worker := NewChatSummaryWritebackService()
	worker.Enabled = true
	worker.MessageThreshold = 3
	worker.ContextLength = 36
	worker.Messages = repository.Query
	worker.Counts = repository.Count
	worker.Kernel = kernel
	worker.LLM = (&fakeLLM{response: `{"summary":"大家在聊第0到第4条消息","entities":[],"relations":[],"facts":[]}`}).Call
	worker.Prompts = fakePrompts(map[string]string{"a_memorix_chat_summary": "{chat_history}"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker.Start(ctx)
	defer worker.Shutdown()

	trigger := newFlowMessage("s1", "b1", "bot-1", "收到", base.Add(6*time.Minute))
	if !worker.Enqueue(trigger) {
		t.Fatalf("入队应成功")
	}
	waitFor(t, "聊天摘要写回", func() bool { return kernel.SummaryCount() == 1 })
	if !strings.Contains(kernel.summaries[0], "第0到第4条消息") {
		t.Fatalf("摘要内容应来自模型输出: %q", kernel.summaries[0])
	}
	state, ok := worker.State("s1")
	if !ok || state.LastTriggerMessageCount != 5 {
		t.Fatalf("触发游标应推进到 5: %+v", state)
	}

	// 未达到阈值时不再触发。
	if worker.Enqueue(trigger) {
		waitFor(t, "第二次入队处理", func() bool { return worker.Processed() == 2 })
	}
	if kernel.SummaryCount() != 1 {
		t.Fatalf("未达阈值不应重复写回: %d", kernel.SummaryCount())
	}
}

func TestChatSummaryWorkerSkipsEmptySummary(t *testing.T) {
	repository := &fakeRepository{}
	base := time.Date(2026, 1, 2, 3, 0, 0, 0, time.Local)
	for index := 0; index < 4; index++ {
		repository.add(newFlowMessage("s1", "u"+itoa(index), "user-1", "闲聊", base.Add(time.Duration(index)*time.Minute)))
	}
	kernel := &fakeKernel{}
	worker := NewChatSummaryWritebackService()
	worker.Enabled = true
	worker.MessageThreshold = 2
	worker.Messages = repository.Query
	worker.Counts = repository.Count
	worker.Kernel = kernel
	// 空 summary = 无增量（对齐 a_memorix_chat_summary.prompt 的约束）。
	worker.LLM = (&fakeLLM{response: `{"summary":"","entities":[],"relations":[],"facts":[]}`}).Call
	worker.Prompts = fakePrompts(map[string]string{"a_memorix_chat_summary": "{chat_history}"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker.Start(ctx)
	defer worker.Shutdown()
	worker.Enqueue(newFlowMessage("s1", "b1", "bot-1", "嗯", base.Add(10*time.Minute)))
	waitFor(t, "空摘要处理", func() bool { return worker.Processed() == 1 })
	if kernel.SummaryCount() != 0 {
		t.Fatalf("空增量摘要不应写入内核")
	}
}

func TestChatSummaryContextLengthIsCappedByPending(t *testing.T) {
	if got := computeContextLength(36, 4); got != 4 {
		t.Fatalf("应取 min(configured, pending): %d", got)
	}
	if got := computeContextLength(30, 100); got != 30 {
		t.Fatalf("pending 大于配置时取配置: %d", got)
	}
	if got := computeContextLength(0, 5); got != 1 {
		t.Fatalf("配置非法应兜底为 1: %d", got)
	}
}

// ---------------------------------------------------------------------------
// ImageMemoryWritebackService
// ---------------------------------------------------------------------------

func newImageFlowMessage(sessionID string, messageID string, hash string, description string, at time.Time) *message.SessionMessage {
	msg := message.NewSessionMessage(messageID, at, "qq")
	msg.SessionID = sessionID
	msg.MessageInfo.UserInfo = message.NewUserInfo("user-1", "阿强", "")
	msg.ProcessedPlainText = message.StringPtr("[图片]")
	msg.RawMessage = message.NewMessageSequence(
		message.NewTextComponent("看看这个"),
		message.NewImageComponent([]byte("fake-image-bytes"), description),
	)
	if hash != "" {
		component := msg.RawMessage.Components[1].(*message.ImageComponent)
		component.BinaryHash = hash
	}
	return msg
}

func TestImageWritebackIsIdempotentViaJournal(t *testing.T) {
	kernel := &fakeKernel{}
	worker := NewImageMemoryWritebackService()
	worker.Enabled = true
	worker.Kernel = kernel

	msg := newImageFlowMessage("s1", "m1", "hash-1", "一只橘猫趴在键盘上", time.Date(2026, 1, 2, 3, 4, 5, 0, time.Local))
	if !worker.Enqueue(msg) {
		t.Fatalf("图片消息应入队")
	}
	// 重复投递仍返回 true（enqueue 本身幂等），但 journal 只保留一条任务。
	worker.Enqueue(msg)
	if worker.Journal.Len() != 1 {
		t.Fatalf("journal 应只有 1 条任务: %d", worker.Journal.Len())
	}

	// 直接处理两次模拟重投递。
	if _, err := worker.HandleMessage(context.Background(), msg); err != nil {
		t.Fatalf("首次写回失败: %v", err)
	}
	job, _ := worker.Journal.Job("s1", "m1")
	worker.Journal.Complete(job.SessionID, job.MessageID)
	if _, err := worker.HandleMessage(context.Background(), msg); err != nil {
		t.Fatalf("重投递应幂等成功: %v", err)
	}
	if kernel.ImageCount() != 2 {
		t.Fatalf("内核收到两次写入（幂等由 external_ref 去重）: %d", kernel.ImageCount())
	}
	completed, ok := worker.Journal.Job("s1", "m1")
	if !ok || completed.Status != "done" {
		t.Fatalf("任务应标记完成: %+v", completed)
	}
}

func TestImageWritebackWorkerRetriesAndKeepsJob(t *testing.T) {
	kernel := &flakyImageKernel{failures: 1}
	worker := NewImageMemoryWritebackService()
	worker.Enabled = true
	worker.Kernel = kernel
	worker.PollInterval = time.Millisecond
	worker.Journal.MaxAttempts = 6
	worker.Journal.Backoff = func(int) float64 { return 0.005 }
	msg := newImageFlowMessage("s1", "m1", "hash-1", "一只橘猫", time.Now())
	worker.SetResolver(func(_ context.Context, sessionID string, messageID string) (*message.SessionMessage, error) {
		return msg, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker.Start(ctx)
	defer worker.Shutdown()
	if !worker.Enqueue(msg) {
		t.Fatalf("应入队")
	}
	waitFor(t, "失败后重试成功", func() bool { return kernel.successes() == 1 })
	if worker.Failures() != 1 {
		t.Fatalf("应记录一次失败: %d", worker.Failures())
	}
	job, ok := worker.Journal.Job("s1", "m1")
	if !ok || job.Status != "done" {
		t.Fatalf("重试成功后任务应为 done: %+v", job)
	}
}

type flakyImageKernel struct {
	mu       sync.Mutex
	failures int
	ok       int
}

func (f *flakyImageKernel) IngestImageMemory(_ context.Context, _ string, _ string, _ string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failures > 0 {
		f.failures--
		return errFakeImage
	}
	f.ok++
	return nil
}

func (f *flakyImageKernel) successes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ok
}

type fakeImageError string

func (e fakeImageError) Error() string { return string(e) }

const errFakeImage = fakeImageError("图片记忆写入失败")

func TestImageWritebackJournalBackoffAndRetryFailed(t *testing.T) {
	journal := NewImageWritebackJournal()
	journal.MaxAttempts = 3
	now := 1000.0
	journal.Now = func() float64 { return now }
	if err := journal.Enqueue("s1", "m1"); err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	job, ok := journal.NextJob()
	if !ok {
		t.Fatalf("应有待处理任务")
	}
	journal.Fail(job, "boom")
	job, _ = journal.Job("s1", "m1")
	if job.Attempts != 1 || job.Status != "pending" || job.RetryAt != now+2 {
		t.Fatalf("首次失败应退避 2 秒并保持 pending: %+v", job)
	}
	if _, ok := journal.NextJob(); ok {
		t.Fatalf("退避期内不应取出任务")
	}
	now += 3
	job, ok = journal.NextJob()
	if !ok {
		t.Fatalf("退避结束后应可取任务")
	}
	journal.Fail(job, "boom")
	now += 6
	job, ok = journal.NextJob()
	if !ok {
		t.Fatalf("第二次退避结束后应可取任务")
	}
	journal.Fail(job, "boom")
	stored, _ := journal.Job("s1", "m1")
	if stored.Status != "failed" || stored.Attempts != 3 {
		t.Fatalf("超过上限应转 failed: %+v", stored)
	}
	if journal.RetryFailed() != 1 {
		t.Fatalf("retry_failed 应重置 1 条")
	}
	reset, _ := journal.Job("s1", "m1")
	if reset.Status != "pending" || reset.Attempts != 0 {
		t.Fatalf("重试后应重置: %+v", reset)
	}
}

func TestIterMessageImageComponentsUsesStablePaths(t *testing.T) {
	msg := message.NewSessionMessage("m1", time.Now(), "qq")
	msg.RawMessage = message.NewMessageSequence(
		message.NewTextComponent("文字"),
		message.NewImageComponent([]byte("a"), "第一张"),
		message.NewTextComponent("间隔"),
		message.NewImageComponent([]byte("b"), "第二张"),
	)
	refs := IterMessageImageComponents(msg)
	if len(refs) != 2 {
		t.Fatalf("应识别 2 张图片: %d", len(refs))
	}
	if refs[0].ComponentPath != "1" || refs[1].ComponentPath != "3" {
		t.Fatalf("组件路径应为稳定下标: %v", []string{refs[0].ComponentPath, refs[1].ComponentPath})
	}
	if !strings.Contains(refs[0].Content, "第一张") {
		t.Fatalf("应带描述内容: %q", refs[0].Content)
	}
}

func TestExplicitUserTextIgnoresImageDescriptions(t *testing.T) {
	msg := message.NewSessionMessage("m1", time.Now(), "qq")
	msg.RawMessage = message.NewMessageSequence(
		message.NewTextComponent("看看这个"),
		message.NewImageComponent([]byte("a"), "[图片：一只猫]"),
		message.NewTextComponent("像不像"),
	)
	if got := ExplicitUserText(msg); got != "看看这个 像不像" {
		t.Fatalf("只应取用户文本组件: %q", got)
	}
}

// ---------------------------------------------------------------------------
// MemoryAutomationService 触发点
// ---------------------------------------------------------------------------

func TestMemoryAutomationServiceTriggers(t *testing.T) {
	kernel := &fakeKernel{}
	repository := &fakeRepository{}
	base := time.Date(2026, 1, 2, 3, 0, 0, 0, time.Local)
	for index := 0; index < 4; index++ {
		repository.add(newFlowMessage("s1", "u"+itoa(index), "user-1", "底稿", base.Add(time.Duration(index)*time.Minute)))
	}
	repository.add(newFlowMessage("s1", "u9", "user-1", "我对青霉素过敏", base))

	fact := NewPersonFactWritebackService()
	fact.Enabled = true
	fact.LLM = (&fakeLLM{response: `[{"fact":"阿强对青霉素过敏","evidence_message_id":"u9","evidence_quote":"我对青霉素过敏"}]`}).Call
	fact.Writer = kernel
	fact.Messages = repository.Query
	fact.ByID = repository.ByID
	fact.IsBotSelf = testIsBotSelf
	fact.PersonID = testPersonID
	fact.Sessions = testPrivateSessionLookup("s1", "qq", "user-1")

	summary := NewChatSummaryWritebackService()
	summary.Enabled = true
	summary.MessageThreshold = 2
	summary.Messages = repository.Query
	summary.Counts = repository.Count
	summary.Kernel = kernel
	summary.LLM = (&fakeLLM{response: `{"summary":"窗口摘要"}`}).Call
	summary.Prompts = fakePrompts(map[string]string{"a_memorix_chat_summary": "{chat_history}"})

	image := NewImageMemoryWritebackService()
	image.Enabled = true
	image.Kernel = kernel

	automation := NewMemoryAutomationService(fact, summary, image)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	automation.Start(ctx)
	defer automation.Shutdown()

	incoming := newImageFlowMessage("s1", "m-img", "hash-x", "一张图", base.Add(time.Minute))
	automation.OnIncomingMessage(incoming)
	if image.Journal.Len() != 1 {
		t.Fatalf("on_incoming_message 应触发图片写回")
	}

	sent := newFlowMessage("s1", "b1", "bot-1", "记下了，注意身体", base.Add(20*time.Minute))
	sent.ReplyTo = message.StringPtr("u9")
	automation.OnMessageSent(sent)
	waitFor(t, "fact + summary 触发", func() bool {
		return kernel.FactCount() == 1 && kernel.SummaryCount() == 1
	})
	if !automation.Started() {
		t.Fatalf("自动化服务应处于启动状态")
	}
}

func TestAutomationServiceIsIdempotentOnStartStop(t *testing.T) {
	automation := NewMemoryAutomationService(nil, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	automation.Start(ctx)
	automation.Start(ctx)
	automation.Shutdown()
	automation.Shutdown()
	if automation.Started() {
		t.Fatalf("Shutdown 后不应仍标记启动")
	}
}
