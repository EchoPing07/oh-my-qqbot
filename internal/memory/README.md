# internal/memory — 记忆上层（M8）

MaiBot 记忆**上层行为**的 Go 重写。底层长期记忆内核是 P5 的 `internal/memorix`
（实现 `internal.MemoryKernel`）；本包只做「压缩/召回/注入/自动写回/门面」。

## 文件对照

| 文件 | MaiBot 来源 | 内容 |
|---|---|---|
| `midterm.go` | `src/maisaka/memory/mid_term.py` | 聊天回想：裁切消息 → `mid_term_memory_summary.prompt` 压缩 → `{summary, recall_cues[3-5]}`；最近消息 embedding × recall_cues 余弦召回（阈值 **0.8**，严格大于）→ `【聊天回想-内部参考】` Reference |
| `heuristic.go` | `src/maisaka/memory/heuristic_injector.py` | 启发式记忆拉起：窗口 20 / 间隔 180s / 增量 60 / TTL 300s / limit 3 / max 900 字，命中以 `【启发式记忆-内部参考】` 注入 |
| `profile.go` | `src/maisaka/memory/person_profile.py` + `A_memorix/core/utils/profile_text.py` | 人物画像：候选收集（当前对象优先）→ 结构化画像文本 → `【人物画像-内部参考】` 注入；person facts 写回内核 |
| `flow.go` | `src/services/memory_flow_service.py` | 三个后台 worker（goroutine + buffered channel **256**）+ `MemoryAutomationService` 触发点 |
| `service.go` | `src/services/memory_service.py` | `MemoryService` 门面（search/ingest/profile/image/query_memory 工具支撑） |
| `types.go` | — | 包内最小注入接口（LLM/Embedding/Prompt/内核/仓储） |
| `flow.go`（人物事实部分） | `src/services/person_fact_verifier.py` | `VerifyDirectPersonFact` 直证校验（临时标记/反问/人物不匹配均拒绝） |

## 依赖约定

本包**不** import `internal/config`、`internal/llm` 具体客户端、`internal/qq`。
所有外部能力通过 `types.go` 的小接口注入：

```go
memory.MidTermSummarizer{
    LLM:      myLLM.Call,          // LLMFunc
    Prompts:  myPrompt.Render,     // PromptFunc
    Embedder: myEmbedder.Embed,    // EmbedFunc
    Kernel:   kernel,              // SummaryIngester（internal.MemoryKernel 子集）
    SessionIDFn: func() string { return runtime.SessionID },
}
```

允许的只读依赖：`internal/chat/context`（Context 消息类型）、`internal/message`、
`internal`（contracts）。

## 与 P4b runtime 的对接

`MidTermSummarizer` 直接满足 `runtime.MidTermMemorySummarizer`：

```go
var _ chatruntime.MidTermMemorySummarizer = (*memory.MidTermSummarizer)(nil)

runtime.SetMidTermMemorySummarizer(&memory.MidTermSummarizer{...})
```

`runtime.PostProcessChatHistoryAfterCycle` 会调用它，把返回的 payload 经
`chatcontext.BuildMidTermMemoryComplexMessage` + `InsertMidTermMemoryMessage`
折叠为一条 `mid_term_memory` 消息（本包也提供 `BuildMidTermMemoryFullText`
用于完整展开）。

召回侧（对齐 `_refresh_mid_term_memory_reference_message`）：

```go
builder := &memory.RecallBuilder{Embedder: embedder.Embed}
history, reference, err := builder.InjectRecallReference(ctx, history, selectedHistory)
```

`RecallBuilder.BuildReferenceMessage` 会自行跳过已注入过的 `(摘要ID, 匹配段)`，
避免同一段重复召回。

## 三个后台 worker（§6.15）

触发点严格对齐 Python：`on_incoming_message` → image；`on_message_sent` → fact + summary。

| worker | channel | 触发条件 | 落库动作 |
|---|---|---|---|
| `PersonFactWritebackService` | 256 | 消息发送后入队 | 抽取事实 → `person_fact_verifier` 直证校验 → `Kernel.UpsertPersonFact`；历史事实重验循环（游标 `person_fact_reverify_cursor.json` 原子落盘） |
| `ChatSummaryWritebackService` | 256 | 累积消息量 ≥ 阈值（默认 36） | `a_memorix_chat_summary.prompt` 增量总结 → `Kernel.IngestSummary`；`context_length = min(configured, pending)`；触发游标可持久化（`StatePath`） |
| `ImageMemoryWritebackService` | journal | 入站图片描述完成后 | `Kernel.IngestImageMemory`；journal（`session_id, message_id`）保证重投递安全 + 指数退避重试 |

```go
automation := memory.NewMemoryAutomationService(factWorker, summaryWorker, imageWorker)
automation.Start(ctx)
automation.OnIncomingMessage(msg)   // 图片写回
automation.OnMessageSent(msg)       // 事实 + 摘要写回
defer automation.Shutdown()
```

## 人物事实直证校验

`VerifyDirectPersonFact` 是纯函数 + 仓储查询，语义照抄 `person_fact_verifier.py`：

- 事实与证据引文的主谓必须一致（第一人称 → 第三人称改写）；
- 含 `今天/昨天/明天/刚才/暂时/可能/似乎/也许/打算/计划` 一律拒绝；
- 证据消息必须在同一 session、同一 person_id、非 bot 自身；
- 引文末尾为 `？/?`、或引文前后有未收束的句子 → 拒绝。

## 测试

```bash
go test ./internal/memory/...
```

覆盖：压缩（假 LLM + 假 embedding）、召回阈值 **0.8 边界**（余弦恰好 0.8 不命中）、
重复召回抑制、三 worker 触发与 journal 重投递/退避、重验游标持久化、
启发式窗口/间隔门控与作用域过滤、结构化画像往返、门面五模式降级。
