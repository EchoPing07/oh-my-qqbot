# internal/llm — P2 work package

OpenAI 兼容模型层：客户端 + ContextItem + 任务路由 + usage 记账 + embedding。
仅依赖标准库与 `internal/contracts.go`（不 import 其它 internal 包，P1 未完成也能编译）。

## 文件 → 对齐源

| 本包文件 | 对齐的 MaiBot 源 | 说明 |
| --- | --- | --- |
| `openai_client.go` | `src/llm_models/model_client/openai_client.py`（`_convert_messages`、`_convert_tool_options`、`_extract_usage_record`）<br>`src/llm_models/openai_compat.py`（base_url 规范化、auth_type 分支、extra_params 拆分）<br>`src/config/model_configs.py`（APIProvider/ModelInfo/TaskConfig 字段与默认值） | 工程结构参考 `参考/Diana/model/llm/openai_compatible.go` + `types.go`（仅结构，不抄行为），用 `net/http` 手写 |
| `context_item.go` | `src/llm_models/payload_content/context_item.py`（`ContextItemMeta`/`ContextTextPart`/`ContextImagePart`/`ContextToolCall`/`ContextItemBuilder`；frozen dataclass union → kind-tagged struct） | `ItemsToMessages`/`MessagesToItems` 与 `internal.ChatMessage` 互转 |
| `orchestrator.go` | `src/llm_models/utils_model.py::LLMOrchestrator`（`_get_task_config_or_raise` 的 `EMPTY_TASK_FALLBACKS` 链、`_select_model` 三策略与 balance 评分、`_attempt_request_on_model` 重试分类、`_attempt_request_on_model_with_timeout` 的 hard_timeout、`_execute_request` 的换模型循环、`_resolve_effective_temperature/max_tokens`） | |
| `usage.go` | `src/llm_models/utils.py::LLMUsageRecorder`（`record_usage_to_database`、`_calculate_input_cost`、`_resolve_prices`、`_CACHE_REPORTING_MODELS`）<br>`src/common/database/database_model.py::ModelUsage`（`llm_usage` 表字段） | 见下方 UsageWriter |
| `embedding.go` | `src/llm_models/model_client/openai_client.py::get_embedding` / `get_image_embedding`<br>`参考/Diana/model/llm/embeddings.go`（工程结构） | `/embeddings` 客户端 |

## 契约实现

```go
var _ internal.LLMLayer = (*Orchestrator)(nil)   // Chat / Embed
```

* `Chat(ctx, "planner"|"replyer"|"utils"|...)` → task 路由。
* `Embed(ctx, texts)` → 走 `embedding` task（强制 sequential 选择，对齐 Python
  `EMBEDDING_TASK_NAMES` 注释：向量空间必须一致，不能随机/均衡切模型）。
* 额外便捷面：`ChatForTask`（自定义 request_type）、`ChatWithSession`（单次 session 覆盖）、
  `EmbedText` / `EmbedRemoteImage`（图片嵌入，供 P5 选用）、`WaitForUsage`（优雅关停刷账）。

## 路由语义（对齐 `_execute_request`）

```
task 名 → TaskConfig（空 model_list 按 fast_model→utils、learner→utils、mid_memory→fast_model 继用）
        → 选择策略：balance | random | sequential（embedding/image_embedding 强制 sequential）
        → 每个模型内重试 max_retry 次（默认 3，至少 1 次）
        → 可重试：空回复、解析失败、网络错误、HTTP 429/5xx（带 Retry-After 时按其退避）
        → 不可重试：HTTP 4xx 其它、上下文取消 → 直接换下一个模型
        → 每次尝试套 hard_timeout（默认 240s），超时视为该模型失败并换模型
        → 全部失败后上抛最后一个错误
```

usage 记账对齐 Python 的 `(total_tokens, penalty, usage_penalty)` 三元组：

* `total_tokens`：成功响应的 `usage.total_tokens`。
* `penalty`：模型请求失败 +1（balance 评分里 ×300）。
* `usage_penalty`：选中模型 +1、请求结束 -1（balance 评分里 ×1000）。

`balance` 评分 = `total_tokens + penalty*300 + usage_penalty*1000`（取最小）。

## UsageWriter（解耦 P1 的 database 包）

`usage.go` 不 import `internal/database`，只定义接口：

```go
type UsageWriter interface {
    InsertUsage(ctx context.Context, usage ModelUsage) error
}
```

`main` 装配时把 P1 写好的 `llm_usage` 写入函数包成 `UsageWriterFunc` 或实现 `UsageWriter` 即可；
writer 为 nil 时记账静默跳过（对齐 Python「记录失败只记日志、不影响请求」）。
写入在独立 goroutine 中完成，`Orchestrator.WaitForUsage()` 可在关停前等待落库。

## usage / 缓存字段

`parseUsage` 对齐 `_extract_usage_record` 的 None 哨兵语义：

* `prompt_cache_hit_tokens` 优先，其次 `prompt_tokens_details.cached_tokens`；
* `prompt_cache_miss_tokens` 缺失且命中 > 0 时用 `prompt_tokens - hit` 推导；
* `Usage.CacheEnabled` = 供应商**本次是否上报了**任一缓存字段；
  另用 `CacheReportingTracker` 复刻 `_CACHE_REPORTING_MODELS`，之后未上报的响应也按启用计。

计费对齐 `_calculate_input_cost` + `record_usage_to_database`：
命中按 `cache_price_in`、未命中按 `price_in`，输出按 `price_out`（元 / M token），
`ModelPricePeriod` 支持跨午夜窗口，cost 四舍五入 6 位。

## 验收

`go test ./internal/llm/...`（49 个测试，含 `httptest` 模拟 server）：

* 客户端：请求体/请求头/鉴权分支（bearer、header、query）、工具调用往返
  （assistant.tool_calls 回传 + tool 角色带 tool_call_id）、空回复可重试、
  HTTP 4xx/429/5xx 分类、超时取消、content 数组解析。
* usage：hit/miss/cached_tokens 三种上报形态、价格分时窗口、记账行字段与缓存上报记忆。
* 路由：task→model 映射、空 model_list 继用链、模型内重试后换模型（断言请求顺序与 attempt 状态）、
  4xx 不重试、`hard_timeout` 及时切换、context 取消、balance 选择随历史切换、
  显式模型覆盖、prefix messages、`send_temperature=false` 省略字段、embedding 策略与失败切换、
  32 并发请求 + usage 落库、热更新 `SetConfig`。
