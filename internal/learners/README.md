# internal/learners — 学习系统（M6, M10, 行为实验）

表达学习、黑话挖掘/解释、行为学习实验骨架的 Go 重写。

## 文件对照

| 文件 | MaiBot 来源 | 内容 |
|---|---|---|
| `expression.go` | `src/learners/expression_learner.py` | 批次闸门（会话互斥 + 全局 `max_expression_learner=3`）、多 message 学习请求、`self_reflect`（`expression_evaluation.prompt`）、入库（完全一致则 count+1）、向量索引同步 |
| `vector_index.go` | `src/chat/replyer/expression_vector_index.py` | JSON 持久化、余弦 TopK、cosine kmeans、MMR 多样性采样（纯 Go 手写） |
| `source.go` | `src/chat/replyer/maisaka_expression_selector.py` 的候选来源 | 实现 `replyer.ExpressionSource`（LoadAllCandidates / BuildVectorCandidatePool / UseVectorExpression） |
| `jargon.go` | `src/learners/jargon_learner.py`、`jargon_explainer.py`、`src/maisaka/jargon_context_matcher.py` | 黑话学习批次闸门、`learn_jargon` 解释入库（三段式推断）、上下文机械匹配 + `[黑话参考]` Reference、`search_jargon` |
| `jargon_miner.go` | `src/services/message_word_frequency_service.py` + `jargon_miner.py` | 分词/停用词/n-gram 短语统计、高频词合并重排、共现挖掘 |
| `behavior.go` | `src/learners/behavior_*.py` | 实验骨架（默认关）：场景分析器 + 行为候选召回 + 参考文案 |

## 依赖约定

本包**不** import `internal/config`、`internal/llm` 具体类型、`internal/qq`。
外部能力通过 `types.go` 注入：`LLMFunc` / `EmbedFunc` / `PromptFunc` /
`ExpressionStore` / `JargonStore` / `SessionExistsFunc` / `IsBotSelfFunc`。

对 `internal/chat/replyer` 只有**接口对齐**用途（`source.go` 提供
`replyer.ExpressionSource` 的实现）；`replyer` 不 import `learners`，无环。

## 表达学习

```go
learner := &learners.ExpressionLearner{
    SessionID: sessionID,
    Options: learners.ExpressionLearnerOptions{
        SelfReflect:         true,
        UseVectorExpression: true,
        VectorIndexPath:     "data/expression_selection/expression_vector_index.json",
        BotName:             "麦麦",
    },
    Store:    expressionStore,       // 由 internal/database 适配
    Prompts:  promptRender,          // learn_style / expression_evaluation
    LLM:      llmLayer.Call,         // learner 任务
    Embedder: embedder.Embed,
}
// 从被裁切的上下文消息学习（只保留 user/guided_reply/outbound_send）
ok, err := learner.LearnFromContextMessages(ctx, removedMessages, isBotSelf, maxLearners)
```

闸门语义照抄 Python：

| 场景 | Reason | 结果 |
|---|---|---|
| `max_expression_learner <= 0` | `max_expression_learner <= 0` | 拒绝 |
| 同会话已有批次 | `session_busy` | 拒绝 |
| 全局活跃数 ≥ 上限 | `global_limit` | 拒绝 |
| 其他 | 获取成功 | 执行批次并在 `defer` 释放 |

## 向量索引（数值稳定性）

- **L2 归一化**在写入与查询两侧都做；零向量/含 NaN/Inf 的向量返回 `nil`（降级为
  metadata-only，不污染余弦计算）。
- **cosine kmeans**：splitmix64 确定性随机（seed `20260621`）+ kmeans++ 增量播种；
  空簇从「成员 ≥ 2 的簇里相似度最低的样本」迁移（`RepairEmptyClusterLabels`）。
- **MMR**：`λ=0.85`，增量维护 `max_diversity_penalties`，只与新选中项比较一次。
- **聚类中心**必须非零；空簇无法修复时保留上一次标签，不 panic。
- **门控**：`STABLE` 且变更比例 `< 5%` → 增量把新向量分配到最近既有簇；
  否则全量重聚类。
- 索引是**派生数据**：同步失败只记录日志，不回滚已成功的入库（对齐 Python 注释）。

持久化把 Python 的 `.json + .npz` 合成单个 JSON（`vectors` 为 `id → []float32`），
写入走 `tmp + rename` 原子替换。

## 黑话

```go
miner := learners.NewJargonMiner("群名")
miner.Store = jargonStore
miner.LLM = llmLayer.Call
miner.Prompts = promptRender   // learn_jargon / jargon_inference_*

learner := &learners.JargonLearner{SessionID: sessionID, Miner: miner, ...}
learner.LearnFromMessages(ctx, messages, maxLearners)
```

- 阈值 `[4, 8, 25, 100]`；`count <= last_inference_count` 或 `is_complete` 时不重复推断。
- 三段式：`jargon_inference_with_context` → `jargon_inference_content_only` →
  `jargon_compare_inference`（`is_similar=true` ⇒ **不是**黑话）。
- `MANUAL` 记录永不被 AI 覆盖。
- 机械匹配打分照抄：`count + (1000 + count*2 + (100-rank)) - index*0.01`。
- 注入形如：

```go
reference := learners.BuildJargonReferenceMessage(userTexts, candidates, options, excluded, time.Now())
// → &ReferencePayload{Kind: "jargon", DisplayPrefix: "[黑话参考]"}
```

## 行为学习（实验，默认关）

`BehaviorPatternSelector.Enabled` 与 `BehaviorScenarioAnalyzer.Enabled` 都默认
`false`。关掉时**不产生任何 LLM 调用**，直接返回空结果；开启后：
场景分析（`behavior_scene_analyze.prompt` + `BehaviorScenarioConstraint`）→
候选召回 → `_build_group_reference_text` 语义的参考文案（文案照抄）。

## 测试

```bash
go test ./internal/learners/...
```

覆盖：黄金向量钉死（L2/余弦/内积/MMR/中心）、kmeans 空簇修复与确定性、
索引持久化与增量/全量重聚类门控、指纹不一致过滤、批次闸门并发
（24 goroutine 峰值 ≤ `max_expression_learner`、申请/释放配对）、
黑话共现挖掘、三段式推断、参考注入与排除、`replyer.ExpressionSource` 端到端选择。
