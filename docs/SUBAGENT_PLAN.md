# oh-my-qqbot 协同开发分工（子代理工作包）

> 每个工作包的子代理只允许使用 CodeBuddy 渠道的 `glm-5.3-flash`、`hy4-preview-f`、`deepseek-v4.1-flash` 三个模型。
> 契约接口已定在 `internal/contracts.go`（internal 包，无外部依赖，全部实现方依赖它）。
> 参考 Python 版在 `参考/MaiBot/`，Diana(Go) 在 `参考/Diana/`（只抄工程实现思路，不抄行为设计；行为以 MaiBot 为准）。
> 共同验收：`go build ./...` 与 `go vet ./...` 通过；核心纯逻辑带表驱动测试；代码内注释标注对齐的 Python 源文件与函数名。

## 纯函数/独立层（先做，无外部依赖）

### P1 config + prompt + database（M0）
- `internal/config/`：Config 总类（对齐 `src/config/official_configs.py` 裁剪面，字段/默认值/拼写照抄，含 `mid_term_memory_lenth`）、ModelConfig（对齐 `model_configs.py` + `default_model_config.py`）、TOML 读写/缺省合成/热重载（fsnotify）、版本升级。输出 `config/bot_config.toml`、`config/model_config.toml` 模板。
- `internal/prompt/`：`.prompt` 加载（zh-CN）、`{var}` 渲染 + `{{}}`/0xFDE9 转义保护（对齐 `src/prompt/prompt_template.py` 或 `src/common/prompt_i18n.py` 语义）、`.meta.toml`、`data/custom_prompts/zh-CN` 覆盖。复制 `参考/MaiBot/prompts/zh-CN/` 到根 `prompts/zh-CN/`。
- `internal/database/`：modernc.org/sqlite 打开/WAL/读写池、`mai_messages`/`llm_usage`/`images`/`tool_records`/`expressions`/`jargons`/`person_info`/`chat_sessions` 建表（对齐 `database_model.py` 字段，raw_content 用 JSON）、models.go 访问函数。
- 依赖：无。验收：`go test ./internal/config/... ./internal/prompt/... ./internal/database/...`。

### P2 message + llm（M2 前半 + 消息模型）
- `internal/message/`：组件化消息（Text/Image/Emoji/Voice/At/Reply/Forward/Dict）、SessionMessage/UserInfo/GroupInfo（对齐 `src/chat/message_receive/message.py` + `src/common/data_models/message_component_data_model.py`）、MessageSequence JSON 序列化（替代 msgpack）、`session_id = md5(platform|account:x|scope:y|group_id)` 精确照抄（读 `src/chat/message_receive/utils_session.py` 实际实现）。
- `internal/llm/`：OpenAI 兼容客户端（chat completions + 工具调用 + usage 含 prompt_cache_*；结构参考 `参考/Diana/model/llm/openai_compatible.go`、`types.go`）、ContextItem、任务路由 orchestrator（task→model_list 随机→超时→重试→下一模型，对齐 `src/llm_models/` 语义）、ModelUsage 记账（写入 llm_usage 表）、embedding 客户端。构造时接收 `internal.LLMLayer` 所需能力。
- 依赖：P1 的 database（usage 记账）。验收：客户端结构单测（httptest 模拟 server），路由/重试单测，session_id 测试。

### P3 qq 渠道（M1）
- `internal/qq/`：QQ 官方机器人渠道全套——appID/secret→token 缓存、WS 网关（wss://api.sgroup.qq.com/websocket）、心跳/RESUME/分片鉴权、 intents `GROUP_AND_C2C_EVENT=1<<25`，事件 `GROUP_MESSAGE_CREATE`（全量）+ `GROUP_AT_MESSAGE_CREATE` + `C2C_MESSAGE_CREATE`、`mentions[].is_you`→is_at、文本含 bot 名/别名→is_mentioned；REST（api.sgroup.qq.com）文本/主动消息（msg_id+msg_seq 自增、40054005 换 seq 重试）、引用 `message_reference`、额度台账（单群 20/qpm 滑窗、每群每日 1000、全局 60/30 qpm 可配；发送前检查+原子更新）、REFIDX 台账（引用自己：发送响应 ext_info.ref_idx；引用别人：入站 message_scene.ext.msg_idx）、自发消息过滤（发送台账 2min TTL + author.id）、富媒体分片上传（媒体 API，先 upload_prepare/parts 拿 file_info 再 msg_type 7）。事件→`internal` 契约 + `internal/message.SessionMessage`。
- 关键约束见 REWRITE_PLAN §6.12；结构可参考 `参考/Diana/model/assistant/qq_official*.go` 的工程模式（token/心跳/重连），但发送层双模（被动→主动降级）与额度台账按计划自实现；**不照抄 Diana 的「群主动消息已关闭」假设**。
- 依赖：P2 的 message。验收：types 解析单测（用真实 payload 样例）、额度台账单测、msg_seq/降级逻辑单测（httptest 模拟）。

## 核心链路（M3-M5）

### P4 chat 触发器 + runtime + context + tools + replyer（M3-M5 核心）
- `internal/chat/trigger/`：scheduler/gates/likelihood/dynamic_gate/idle_backoff，权重与常数照抄（`reply_likelihood.py` 截距 -1.2005 + 8 特征、`dynamic_gate.py` 3600s/0.91/20/40s/11 档阈值表、`gates.py`、`idle_backoff.py`）。
- `internal/chat/runtime/`：MaisakaHeartFlowChatting 状态机（RUNNING/WAIT/STOP）、engine 内部轮次（MAX 10）、planner prompt 组装（`chat_loop_service.py` + `maisaka_chat.prompt`）、interrupt。
- `internal/chat/context/`：LLMContextMessage 四类（SessionBacked/Reference/ModelOutput/ToolResult）、裁剪（孤儿工具结果清理、mid-term 折叠、count_in_context=false 不计数）、adapter 可见文本 `[HH:MM:SS][msg_id:别名][名字]内容`。
- `internal/chat/tools/`：ToolRegistry + reply/wait/query_memory/send_emoji/show_emoji_list/view_forward_message/fetch_history/tool_search（对齐 `builtin_tool/`）；reply 工具走 replyer→后处理→sender。
- `internal/chat/replyer/`：generator（人设拼接 `{bot_name}，也有人叫你{aliases}` + 人格 + 情绪后缀）、expression_selector、postprocess（`utils.py` 断句/合并/标点/默认回复照抄）、typo（gse+go-pinyin+char_frequency.json）、splitter_llm、kaomoji、typing_time。
- `internal/chat/manager/` + `internal/chat/heartflow/`：ChatManager/session 持久化/HeartflowManager（LRU100+24h 保活）+ runtime 引擎接线。
- 依赖：P1/P2/P3 接口。验收：gates/likelihood/splitter/typo/session 相关表驱动测试通过。

### P5 A_memorix 内核（M7）
- `internal/memorix/`：kernel.go 实现 `internal.MemoryKernel`；storage（paragraphs/entities/relations/episodes/profiles + 链接表 + FTS5 虚表 + vectors 表 + 内存 flat 内积索引）；retrieval（dual_path 融合、BM25 gse 分词、Aho-Corasick、Personalized PageRank 幂迭代、图召回 grounding 0.78/0.55/0.4/0.3、图可靠性 0.65/0.10/0.25、PPR alpha 照抄 Python 版常数——读 `src/A_memorix/` 相应模块提取常数）；extract（LLM 实体/关系/段落抽取 + JSON 修复）；embed（embedding 客户端接入、维度探测、失败降级 metadata-only）。
- 依赖：P2 llm（嵌入/抽取用）。验收：BM25/AC/PPR/融合常数单测；写入对话后语义/时间检索命中测试。

### P6 记忆上层 + 学习系统（M6/M8/M10）
- `internal/memory/`：midterm（mid_term_memory_summary 压缩/recall_cues/0.8 阈值召回注入）、heuristic、profile、flow（fact/summary/image 三 worker，channel 256，触发点对齐 memory_flow_service.py）、service.go 实现 `internal.MemoryKernel` 门面（依赖 P5 内核）。
- `internal/learners/`：expression.go（学习+review+并发闸门 max_expression_learner=3）、vector_index.go（JSON 持久化、余弦+聚类 kmeans+MMR）、jargon.go、jargon_miner.go；behavior.go 实验开关骨架（默认关）。
- 依赖：P4 replyer/selector 接口 + P5 内核。验收：vector_index 数值稳定性单测、flow 触发逻辑单测。

### P7 表情包 + 图片 + 人物 + sender + monitor（M9 部分）
- `internal/emoji/`：注册/VLM 描述/情绪标签/偷表情（emoji_replace.prompt 决策）/旧逻辑按情绪 Top10 选择/新逻辑拼图（256px 格 + 64px 序号角标，x/image 合成）。
- `internal/image/`：ImageManager（保存/sha256/VLM 描述/清理）。
- `internal/person/`：person_id(md5)/群名片/信息管理。
- `internal/chat/sender/`：send_service（出站构造→渠道→存库→回写历史 guided_reply）。
- `internal/monitor/`：事件 bus 实现 `internal.EventBus`（零订阅零开销）。
- 依赖：P2/P3。验收：拼图合成单测（生成 png 断言尺寸）、monitor bus 单测。

### P8 WebUI 后端 + monitor 事件接入（M13）
- `internal/webui/`：net/http 服务器 + embed/磁盘静态托管 + SPA fallback；TokenManager 鉴权（启动生成/读取 token、日志打印一次、Authorization 头 + WS 握手临时 token 60s）；WS hub（域/主题订阅：logs/maisaka_monitor/chat）；REST 路由：config/model/system/logs/memory/person/expression/jargon/emoji/behavior/reasoning_process/statistics/reply_effects/chat/mcp/data_transfer/news/user_emoji（对齐 `src/webui/routers/` 行为，可分批，聊天平台 WEBUI_CHAT_PLATFORM 虚拟平台接入 ChatManager）。
- 订阅 `internal.EventBus` 转发 WS。
- 依赖：P1/P4/P7。验收：鉴权单测、WS hub 单测、路由 smoke 测试（httptest）。

### P9 MCP 客户端（M12）
- `internal/mcp/`：go-sdk 三传输（stdio/streamable_http/sse）、进程级单例 Manager、config_signature 热切换、lease 退休、工具注册进 ToolRegistry（provider_type=mcp、deferred）、权限 allow_users/allow_chats/permission、Host Sampling 桥（服务器 sampling 回调→planner LLM）。
- 依赖：P4 tools registry。验收：stdio 服务器挂载 smoke 测试（可用简单进程）、签名热切换单测。

### P10 WebUI 前端（M14）
- `web/dashboard/`：fork `参考/MaiBot/dashboard/` 源码（AGPLv3，保留版权），React19+Vite+TanStack+Tailwind4；移除插件系统页面（plugin-config/plugins/plugin-mirrors/plugin-webui）与 Electron 打包；API client 对齐 P8 路由；i18n zh-CN 为主。页面：概览/聊天/聊天管理/监控/资源(表达/黑话/表情/行为/知识图谱)/人物/日志/MCP/模型配置/配置中心/统计/数据传输/回复效果/登录/初始化。
- 依赖：P8 路由定型。验收：`npm install && npm run build` 通过，dist 产物存在。
- 注意：这是 Node 工程，子代理同样限 CodeBuddy 三模型。

### P11 打磨（M15）
- reply_effect（回复后随访评审实验系统）、数据导出导入、news 数据源配置化拉取、统计聚合、知识图谱页只读数据接口、README（GPLv3 兼容声明 + 衍生来源 + 主动额度提示）。
- 依赖：P5-P8。验收：相应单测 + build。

## 执行顺序

1. **第一波（并行）**：P1、P2（P2 只依赖 P1 的 database 接口，可先定义好 DB 访问接口桩）
2. **第二波（并行）**：P3、P5（P5 只依赖 P2 的 llm 接口桩）
3. **第三波**：P4（核心，最大包，可拆两个子代理：P4a trigger/runtime/context，P4b tools/replyer/manager/heartflow）
4. **第四波（并行）**：P6、P7、P8 骨架
5. **第五波（并行）**：P9、P10、P11
6. 收尾：main.go 装配、端到端 go build + go vet + go test 全绿、README。
