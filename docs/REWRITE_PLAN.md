# oh-my-qqbot — MaiBot Go 重写计划

> 目标:用 Go 重写 MaiBot(参考/MaiBot)的**核心聊天智能体**,支持 **QQ 官方机器人渠道**,含
> **WebUI / MCP / A_memorix 长期记忆内核**;**不含插件系统**。设计、行为、提示词严格对齐 MaiBot;
> 技术栈换为 Go;QQ 官方协议实现参考 Diana(参考/Diana)。

---

## 1. 范围裁定

### 1.1 保留(MaiBot 设计,必须严格对齐)

| MaiBot 模块 | 位置 | 说明 |
|---|---|---|
| 配置系统 | `src/config/` | bot_config.toml + model_config.toml 双文件、版本化、热重载 |
| 消息模型 | `src/chat/message_receive/message.py`, `src/common/data_models/` | SessionMessage + 组件化消息(Text/Image/Emoji/Voice/At/Reply/Forward) |
| 会话管理 | `src/chat/message_receive/chat_manager.py` | BotChatSession、session_id 计算、ChatSession 持久化 |
| 心流触发器 | `src/maisaka/turn_trigger/` | frequency/dynamic 两种触发模式、空闲退避、回复概率门控(全量收消息前提下决定何时发言,QQ 全量能力可完整落地) |
| 心流运行时 | `src/maisaka/runtime.py`, `src/maisaka/reasoning_engine.py` | 每会话一个 runtime,Planner 内部多轮循环,状态机 RUNNING/WAIT/STOP |
| 规划循环服务 | `src/maisaka/chat_loop_service.py` | Planner prompt 组装、上下文裁剪、工具定义注入 |
| 上下文抽象 | `src/maisaka/context/` | LLMContextMessage 四类:SessionBacked/Reference/ModelOutput/ToolResult |
| 内置工具 | `src/maisaka/builtin_tool/` | reply/wait/query_memory/send_emoji/view_forward_message/fetch_history/tool_search 等 |
| 回复生成器 | `src/chat/replyer/` | replyer prompt 组装、表达方式选择、重试 |
| 回复后处理 | `src/chat/utils/utils.py`, `typo_generator.py`, `llm_sentence_splitter.py` | 断句合并、错别字、标点处理、颜文字保护、打字时间模拟 |
| 表达学习 | `src/learners/expression_learner.py`, `expression_vector_index.py` | situation/style 表达学习 + 向量召回 + LLM 二段选择 |
| 黑话学习 | `src/learners/jargon_learner.py`, `jargon_miner.py` | 黑话挖掘/解释/上下文匹配注入 |
| 行为学习(实验) | `src/learners/behavior_*.py` | 场景-行为-结果经验学习(实验性开关,默认关) |
| 聊天回想 | `src/maisaka/memory/mid_term.py` | 消息压缩 summary + recall_cues,按语义召回注入 |
| 人物系统 | `src/person_info/` | person_id(md5)、群名片、人物信息 |
| 表情包系统 | `src/emoji_system/` | 注册/VLM 描述/情绪标签/偷表情/按情绪选择/拼图选择 |
| 图片系统 | `src/chat/image_system/` | 图片保存、VLM 描述、缓存清理 |
| 记忆注入 | `src/maisaka/memory/heuristic_injector.py`, `person_profile.py` | 启发式记忆拉起、人物画像注入 |
| 长期记忆内核(A_memorix) | `src/A_memorix/` | 段落/实体/关系图谱 + 向量库 + BM25 稀疏检索 + FTS5,双路检索融合 + PPR 重排;SDK 内核接口见 §6.14 |
| 记忆自动写回 | `src/services/memory_flow_service.py` | 人物事实写回、聊天摘要写回、图片记忆写回三个后台 worker(见 §6.15) |
| WebUI 后端 | `src/webui/` | FastAPI + WebSocket;路由清单与鉴权见 §6.16 |
| WebUI 前端(dashboard) | `dashboard/` | React 19 + Vite + TanStack Router/Query + Tailwind 4,页面清单见 §6.17 |
| MCP 客户端 | `src/mcp_module/` | stdio/streamable_http/sse 三种传输;工具/提示/资源挂入统一工具层;Host 侧 Sampling 桥(见 §6.18) |
| Prompt 体系 | `prompts/zh-CN/*.prompt`, `src/prompt/`, `src/common/prompt_i18n.py` | **提示词文件原样照抄**,模板变量渲染机制对齐 |
| 回复效果观察(可选) | `src/maisaka/reply_effect/` | 回复后随访评审(实验性,放最后) |
| 观察事件流(monitor) | `src/maisaka/monitor/events.py` | Planner 快照/消息摄入/发送等事件广播,WebUI 监控页数据源(见 §6.16) |

### 1.2 丢弃(不重写)

| 模块 | 行数 | 理由 |
|---|---|---|
| 插件系统 | ~22,457 | 用户明确不要(含 plugin_runtime/hooks/HookSpec 全链路;MaiBot 代码里所有 `invoke_hook` 调用点在 Go 版直接跳过) |
| platform_io 多平台驱动框架 | ~3,201 | 只做 QQ 官方,直接实现单渠道 |
| maim_message 适配器协议 | 外部依赖 | 被 QQ 官方渠道直连替代 |
| cli / 遥测 / news_server 外部拉取 | — | 非核心(news 列表页数据源简化,见 §6.17) |
| Legacy 迁移 / ConfigUpgradeHook | — | 全新实现,无历史包袱 |

### 1.3 从 Diana 抄什么(Go 实现参考)

| 内容 | Diana 位置 | 用途 |
|---|---|---|
| QQ 官方渠道完整实现 | `model/assistant/qq_official*.go` (~1900 行) | token 获取、WS 网关、心跳/RESUME、REFIDX 引用索引、富媒体分片上传。**注意:其「群里主动消息能力已被平台关闭(40034105)」的注释假设已过时,本项目的发送层按 §6.12 双模实现,不照抄该假设;全量事件 `GROUP_MESSAGE_CREATE` 解析与主动额度跟踪需按官方文档自行实现** |
| OpenAI 兼容客户端骨架 | `model/llm/openai_compatible.go`, `types.go` | 请求/响应/工具调用/usage 结构 |
| SQLite 存储模式 | `model/storage/sqlite_store.go` | modernc.org/sqlite(纯 Go 无 CGO)、WAL、读写分离 |
| 打字延迟 | `model/assistant/typing_delay.go` | 按字数模拟打字(与 MaiBot calculate_typing_time 对齐后择一) |
| 中文分词 | go.mod 中 `go-ego/gse` | 错别字生成器与 BM25 分词的分词依赖替代 jieba |
| 通道状态/断线重连模式 | `qq_official.go` Connect/runSession | 稳定性 |
| MCP Go SDK | `github.com/modelcontextprotocol/go-sdk`(Diana 同款) | stdio/HTTP/SSE 客户端传输栈直接用 SDK,不手写协议 |
| gin + SPA 静态服务 | `cmd/webui/main.go`(gin 路由 + NoRoute spaHandler) | WebUI 后端路由与前端静态托管模式参考 |
| Vue/React 均可的单页前端 | Diana `frontend-next/`(Vue3) | 仅参考构建/部署形态;本项目前端照 MaiBot 用 React |

> 注意:Diana 与 MaiBot 是**两种不同的智能体设计**(Diana = 单轮 agent 直出回复;MaiBot = Planner 决策 + reply 工具 + replyer 生成的双阶段心流)。
> **只抄 Diana 的工程实现(QQ 协议、LLM 客户端、存储),智能体行为一律以 MaiBot 为准。**

---

## 2. 核心架构(MaiBot 行为链路,必须复刻)

```
QQ 官方 WS 网关
   │ GROUP_MESSAGE_CREATE(全量,含未@)/ GROUP_AT_MESSAGE_CREATE(兼容)
   │ C2C_MESSAGE_CREATE(单聊);@ 靠 mentions[].is_you 识别
   ▼
qq.Channel ──► SessionMessage(组件化)
   │              session_id = md5(platform[|account:..|scope:..]_group_id | user_id_private)
   ▼
ChatBot.receive_message
   ├── 图片入站压缩(process_received_images_in_message)
   ├── ChatManager.get_or_create_session
   ├── MessageUtils.store_message_to_db (SQLite)
   ▼
HeartflowManager.get_or_create_heartflow_chat(session_id)   ← LRU(100) + 24h 保活
   ▼
MaisakaHeartFlowChatting.register_message
   ├── 消息入缓存 (message_cache, 200 上限)
   ├── 记录外部消息间隔(连发过滤 5s、空窗补偿统计)
   ├── @ 强制触发判定 (is_at / mentioned)
   ▼
MessageTurnScheduler.schedule_message_turn
   ├── focus_mode gate(可选)
   ├── WAIT 状态处理 / reply_frequency 静默判定
   ├── forced_turn(@ 必回)
   ├── IdleBackoff(连续不回复指数退避 15s→300s)
   └── Gate:
       ├── FrequencyThresholdTurnGate: pending ≥ ceil(1/频率) 阈值 → 触发;空窗补偿(平均间隔≥30s 下限)
       └── DynamicReplyTurnGate: reply_likelihood 逻辑回归(权重照抄) → 窗口动态阈值表(照抄) → 贴近目标回复频率
   ▼ (入队 internal_turn_queue)
ReasoningEngine.run_loop (每会话一个 goroutine)
   ├── _prepare_turn_start_context: 收集本批消息
   ├── 静默消费分支(频率=0)
   └── 内部轮次循环 (MAX_INTERNAL_ROUNDS = 10):
               │
        ▼     │
   MaisakaChatLoopService.chat_loop_step   ← Planner LLM 调用
        │  system prompt = prompts/zh-CN/maisaka_chat.prompt (或 focus 版)
        │  + 行为风格 + 群聊注意块 + query_memory 规则 + 工具说明
        │  历史 = 裁剪后的 LLMContextMessage 列表(上限 max_context_size=40)
        │  + 注入: 聊天回想 Reference / 启发式记忆 / 人物画像 / 黑话参考
        │  + 时间 user message + 末尾提醒 (PLANNER_FINAL_USER_REMINDER)
        ▼
   响应处理 _handle_planner_response_actions
        ├── 有 tool_calls → 逐个执行 ToolRegistry.invoke:
        │      reply        → 走 Replyer 生成 → 后处理 → 发送 (核心!)
        │      wait         → 进入 WAIT 状态(受 max_consecutive_wait_count=3 限制)
        │      query_memory / send_emoji / view_forward_message / ...
        │   → 工具结果入上下文 → 继续下一轮内部思考
        ├── 无 tool_calls → planner_no_tool_count++ → 注入 no-tool 提示重试 → 超限结束
        └── 新消息打断 (PlannerInterruptController, 上限可配) → 重跑
   ▼
reply 工具内部:
   1. 校验 msg_id(目标消息必须在上下文/缓存中)、set_quote、attach_* 权限
   2. 表达方式选择 (MaisakaExpressionSelector):
        向量召回(use_vector_expression) 或 随机候选 → 子代理 LLM 按情境选择(≤5 条)
   3. Replyer 生成 (BaseMaisakaReplyGenerator):
        system = prompts/zh-CN/maisaka_replyer.prompt
                 {identity} 人设(名字+别名+人格+情绪特点后缀)
                 {reply_style} 说话风格 (+multiple_reply_style 概率替换)
                 {group_chat_attention_block} 群聊/私聊提示词
        history = 过滤后真实聊天上下文(不含 Reference/ToolResult/回想)
        + expression_habits user message
        + 临时风格 / reply_reference / 末条 final user message(时间+目标消息块)
        → replyer 任务模型生成
   4. 后处理 (process_llm_response_segments):
        颜文字保护 → 去(中文)括号内容 → 超长降级默认回复
        → 断句(mode=rule 正则+概率合并 / mode=llm JSON 断句)
        → 错别字注入(拼音替换+整词替换+声调错误)+ 纠正消息(可引用)
        → 超过 max_sentence_num 降级 → 压缩到 max_split_num 条
   5. 逐条发送: 第一条引用目标消息,后续直接发;
        每条之间按 calculate_typing_time(中文 0.3s/字, 英文 0.15s/字) sleep
        每条经 send_service 构造 SessionMessage 存库 + 写回运行时历史(guided_reply)
   6. 回执给 Planner: "麦麦已向 xxx 发送了回复 ..."
        → record_reply() 供动态门控统计;reply_effect 观察注册
```

**QQ 官方渠道的关键约束(影响行为设计):**

- **全量消息接收**:管理端开启「接收所有消息」+ 群设置「机器人可获取的群聊消息范围 = 获取群内全部消息」后,群内**每条消息**都会以 `GROUP_MESSAGE_CREATE` 推送(intent `GROUP_AND_C2C_EVENT = 1<<25`,与 @ 事件共用);@ 机器人的消息靠 payload `mentions[].is_you == true` 识别。未开启全量能力时退回只收 `GROUP_AT_MESSAGE_CREATE`。这正是 MaiBot「收全量、决定何时说」设计的完整前提,触发器/空闲退避/wait 语义与 Python 版**完全对齐,无需收敛**。
- **主动消息**:开启「机器人主动在群聊内发言」能力后,发送接口**不带 msg_id** 即为主动消息;限额:机器人维度 60/30 qpm(按认证)、单群 20/qpm、**每群每天 1000 条**;无权限时返回 40034105。MaiBot 的「主动搭话」能力由该额度承载,发送层必须做额度跟踪与降级(见 §6.12)。
- **被动回复**:带触发消息的 `msg_id`(5 分钟内有效,每条消息最多回 5 次,同 msg_id 多条靠递增 `msg_seq`,重复会命中 40054005 去重;超限返回 40034128)。发送层**优先被动、超窗降级主动**。
- **富媒体**(表情包/图片):先分片上传拿 `file_info`,再发 `msg_type 7`。
- **引用回复**:`message_reference.message_id` 引用别人用入站 `message_scene.ext.msg_idx`,引用自己用发送响应 `ext_info.ref_idx`(REFIDX 台账,见 §6.12)。
- **wait 的额度语义**:wait 挂起后醒来若走主动消息,消耗每日主动额度;额度耗尽时 Planner 的 wait 拒绝策略不变,发送层拒绝并回执错误给模型(「主动消息额度不足」),由 Planner 自行决定沉默。

---

## 3. Go 技术选型

| 领域 | 选型 | 备注 |
|---|---|---|
| 语言 | Go ≥ 1.25 | 与 Diana 一致 |
| SQLite | `modernc.org/sqlite` | 纯 Go 无 CGO,Windows 友好(Diana 同款);含 FTS5(记忆内核全文索引) |
| WebSocket | `gorilla/websocket` | Diana 同款;QQ 网关 + WebUI 事件流共用 |
| HTTP 路由 | `net/http` 标准库(或 gin) | WebUI 后端;Diana 用 gin,标准库也可胜任 |
| TOML | `pelletier/go-toml/v2` | Diana 同款 |
| JSON | 标准库 + 必要处 `gjson` | |
| 日志 | `log/slog` | 彩色控制台可后加 |
| 分词 | `go-ego/gse` | 替代 jieba(错别字/词频 + BM25 分词) |
| 拼音 | `mozillazg/go-pinyin` | 替代 pypinyin(错别字生成) |
| MCP | `github.com/modelcontextprotocol/go-sdk` | Diana 同款;stdio/HTTP/SSE 客户端栈 |
| 向量运算 | 手写(切片 + 余弦/内积 + L2 归一化) | 替代 numpy/faiss;表达索引(≤数千)与记忆段落向量均纯 Go 遍历;百万级再考虑量化/ANN |
| 图算法 | 手写(PageRank 幂迭代/Aho-Corasick/BM25) | 替代 scipy/networkx;规模可控 |
| 图像 | `golang.org/x/image` + `image/png/jpeg/gif` | 拼图合成替代 PIL |
| 前端 | React 19 + Vite + TanStack Router/Query + Tailwind 4 | fork MaiBot dashboard 改造(§6.17) |
| 测试 | `go test -race ./...` | 对齐 Diana 的测试纪律 |

---

## 4. 目标目录结构

```
oh-my-qqbot/
├── cmd/bot/main.go              # 入口:装配、启动、优雅关停(对齐 src/main.py)
├── go.mod
├── config/
│   ├── bot_config.toml          # 模板(生成),字段与 MaiBot 对齐(裁剪后)
│   └── model_config.toml        # 模板
├── prompts/zh-CN/               # 原样复制 MaiBot prompts/zh-CN(全部 .prompt + .meta.toml)
├── depends-data/
│   └── char_frequency.json      # 错别字字频表(随仓库带,对齐 MaiBot)
├── data/                        # 运行时:db/emoji/images/expression_selection
└── internal/
    ├── config/                  # 配置定义/加载/版本升级/热重载
    │   ├── config.go            # Config 总类(对齐 official_configs.py 裁剪版)
    │   ├── model_config.go      # ModelConfig/APIProvider/ModelInfo/TaskConfig
    │   ├── loader.go            # TOML 读写、缺省合成、热重载 watcher
    │   └── version.go
    ├── database/
    │   ├── db.go                # 连接/WAL/读写池
    │   ├── migrations.go        # 建表
    │   └── models.go            # 与 MaiBot 表结构对齐(§6.1)
    ├── message/                 # 消息模型
    │   ├── components.go        # Text/Image/Emoji/Voice/At/Reply/Forward/Dict
    │   ├── session_message.go   # SessionMessage/UserInfo/GroupInfo
    │   ├── sequence.go          # MessageSequence 序列化(JSON)
    │   └── session_id.go        # SessionUtils.calculate_session_id
    ├── llm/                     # LLM 层
    │   ├── openai_client.go     # OpenAI 兼容(chat completions + 工具调用 + 流式可选)
    │   ├── context_item.go      # ContextItem(system/user/assistant/tool + image parts)
    │   ├── orchestrator.go      # 任务路由: task→model_list→随机→超时→重试→下一个模型
    │   ├── usage.go             # ModelUsage 记账(对齐 database_model.ModelUsage)
    │   └── embedding.go         # embedding 客户端
    ├── qq/                      # QQ 官方渠道(结构参考 Diana qq_official*.go,按官方文档实现)
    │   ├── channel.go           # WS 网关、心跳、RESUME、事件分发
    │   │                        # 事件: GROUP_MESSAGE_CREATE(全量)+GROUP_AT_MESSAGE_CREATE(兼容)
    │   │                        #       C2C_MESSAGE_CREATE;@识别靠 mentions[].is_you
    │   ├── api.go               # REST、token 缓存、msg_seq、40054005 去重重试
    │   ├── proactive.go         # 主动消息额度台账:单群 20/qpm 滑窗 + 每群每日 1000 + 全局 qpm
    │   │                        # 发送策略:被动优先(带触发 msg_id)→超窗(5min)/超次数(5次)降级主动
    │   ├── media.go             # 分片上传 upload_prepare/parts
    │   ├── ledger.go            # REFIDX 引用索引、自发消息识别(全量模式下回推自己也要过滤)
    │   └── types.go             # 事件/出站结构 → 转换为 message.SessionMessage
    │                            # (mentions/msg_elements/message_scene.ext 全量字段解析)
    ├── chat/
    │   ├── manager/             # ChatManager/BotChatSession/ChatSession 持久化
    │   ├── heartflow/           # HeartflowManager(LRU+保活)
    │   ├── trigger/
    │   │   ├── scheduler.go     # MessageTurnScheduler
    │   │   ├── gates.go         # FrequencyThreshold + Dynamic 双门控
    │   │   ├── likelihood.go    # 回复概率逻辑回归(权重照抄)
    │   │   ├── dynamic_gate.go  # 窗口/虚拟轮次/静态阈值表(照抄)
    │   │   └── idle_backoff.go
    │   ├── runtime/             # MaisakaHeartFlowChatting(状态机/打断/等待)
    │   │   ├── runtime.go
    │   │   ├── engine.go        # run_loop 内部轮次
    │   │   ├── planner.go       # Planner 请求组装(对齐 chat_loop_service)
    │   │   └── interrupt.go
    │   ├── context/             # LLMContextMessage 抽象 + 裁剪 + 工具对齐
    │   │   ├── messages.go      # SessionBacked/Reference/ModelOutput/ToolResult
    │   │   ├── history.go       # 裁剪、孤儿工具结果清理、mid-term 折叠
    │   │   └── adapter.go       # 可见文本 [HH:MM:SS][msg_id:N][名字]内容
    │   ├── tools/               # 内置工具(对齐 builtin_tool/)
    │   │   ├── registry.go      # ToolSpec/ToolInvocation/ToolRegistry
    │   │   ├── reply.go         # 核心 reply
    │   │   ├── wait.go / query_memory.go / send_emoji.go / show_emoji_list.go
    │   │   └── view_forward_message.go / fetch_history.go / tool_search.go
    │   ├── replyer/
    │   │   ├── generator.go     # 人设/风格/请求组装/重试(对齐 maisaka_generator_base)
    │   │   ├── expression_selector.go
    │   │   ├── postprocess.go   # 断句/括号/默认回复/压缩(对齐 utils.py)
    │   │   ├── typo.go          # 错别字生成器(gse+go-pinyin 复刻)
    │   │   ├── splitter_llm.go  # LLM 断句
    │   │   ├── kaomoji.go       # 颜文字保护
    │   │   └── typing_time.go
    │   └── sender/              # send_service:出站构造→渠道→存库→回写历史
    ├── learners/
    │   ├── expression.go        # 表达学习(含 review/并发闸门)
    │   ├── vector_index.go      # 表达向量索引(JSON+npz→JSON,余弦+聚类+MMR)
    │   ├── jargon.go            # 黑话学习/解释
    │   ├── jargon_miner.go      # 黑话挖掘
    │   └── behavior.go          # 行为学习(实验,后期)
    ├── memory/
    │   ├── midterm.go           # 聊天回想(压缩/召回/注入)
    │   ├── flow.go              # 记忆自动写回(fact/summary/image 三 worker,对齐 memory_flow_service)
    │   ├── heuristic.go         # 启发式记忆拉起
    │   ├── profile.go           # 人物画像注入
    │   └── service.go           # MemoryService 门面(对齐 services/memory_service.py 接口面)
    ├── memorix/                 # A_memorix 内核 Go 重写(对齐 src/A_memorix,§6.14)
    │   ├── kernel.go            # SDKMemoryKernel 接口面(IngestSummary/SearchMemory/GetPersonProfile/admin)
    │   ├── storage/
    │   │   ├── metadata.go      # paragraphs/entities/relations/episodes/profiles + 链接表
    │   │   ├── fts.go           # FTS5 全文(段落+关系)
    │   │   ├── vector.go        # 向量表 + 内存 flat 内积索引(L2 归一化)
    │   │   └── graph.go         # 图存储(节点/边/度表)
    │   ├── retrieval/
    │   │   ├── dual_path.go     # 双路检索融合(策略枚举/常数对齐)
    │   │   ├── bm25.go          # gse 分词 + BM25
    │   │   ├── ac_matcher.go    # Aho-Corasick 实体匹配
    │   │   ├── pagerank.go      # Personalized PageRank(幂迭代)
    │   │   └── graph_recall.go  # 图召回 + 可靠性加权 + 后验门
    │   ├── extract.go           # 实体/关系/段落抽取(LLM + JSON 修复)
    │   └── embed.go             # 嵌入接入/profile 缓存/降级
    ├── webui/                   # WebUI 后端(§6.16)
    │   ├── server.go            # HTTP 服务器/静态托管/SPA fallback
    │   ├── auth.go              # TokenManager 语义 + WS 握手临时 token
    │   ├── wshub.go             # WS 统一通道(域/主题订阅)
    │   └── routers/             # config/model/system/logs/memory/person/expression/
    │                            # jargon/emoji/behavior/reasoning_process/statistics/
    │                            # reply_effects/chat/mcp/data_transfer/news/user_emoji
    ├── monitor/                 # 观察事件 bus(对齐 maisaka/monitor/events.py 的 emit_* 面)
    ├── mcp/                     # MCP 客户端(§6.18, go-sdk)
    │   ├── service.go           # 进程级单例/签名热切换/lease 退休
    │   ├── provider.go          # ToolProvider 适配进统一 ToolRegistry
    │   └── sampling.go          # Host Sampling 桥(服务器回调→planner LLM)
    ├── emoji/                   # EmojiManager(注册/描述/情绪/偷表情/拼图)
    ├── image/                   # ImageManager(保存/VLM 描述/清理)
    ├── person/                  # PersonInfo/person_id/群名片
    └── prompt/
        ├── loader.go            # .prompt 加载、locale、custom_prompts 覆盖
        ├── template.go          # {var} 渲染 + {{}} 转义(对齐 0xFDE9 机制)
        └── meta.go              # .meta.toml
└── web/
    └── dashboard/               # 前端:fork MaiBot dashboard(React19+Vite+TanStack+Tailwind4),§6.17
```

---

## 5. 配置设计(字段级对齐 MaiBot,裁剪后)

`config.Config`(bot_config.toml)保留这些节:

- **bot**: nickname(麦麦)/alias_names/qq_account
- **personality**: personality / behavior_style / reply_style / multiple_reply_style / multiple_probability
- **chat**:
  - enable_reply_at / max_context_size(40) / max_private_context_size(60) / enable_context_optimization
  - mid_term_memory(true) / mid_term_memory_lenth(10)(保留 MaiBot 原字段名拼写)
  - **reply_timing**: talk_value(1)/private_talk_value(1)/mentioned_bot_reply/inevitable_at_reply/reply_trigger_mode(frequency|dynamic)/planner_interrupt_max_consecutive_count(0)/max_consecutive_wait_count(3)/no_action_backoff_base_seconds(15)/cap(300)/start_count(2)/bypass_pending_count(6)/enable_talk_value_rules+talk_value_rules
  - **reply_style**: enable_reply_quote / group_chat_prompt(默认文案照抄) / private_chat_prompts / chat_prompts
- **experimental**(实验开关照抄,默认关): replyer_retro_prompt / emotion_trait(rational_calm|neutral|sentimental + PERSONALITY_EMOTION_SUFFIXES 文案照抄) / attention_drift / focus_mode 系列 / enable_behavior_learning
- **visual**: max_image_num(64) / wait_image_recognize_max_time(32) / handle_oversized_images / max_image_size_mb(16) / oversized_image_handle_method
- **expression**: expression_checked_only / expression_self_reflect / use_vector_expression / expression_vector_index_path / expression_vector_candidate_pool_size(50) / max_expression_learner(3) / learning_list / expression_groups
- **jargon**: learning_list / jargon_groups
- **message_receive**: image_parse_threshold(5) / ban_words / ban_msgs_regex
- **emoji**: use_new_send_logic / emoji_send_num(25) / max_reg_num(64) / do_replace / check_interval / steal_emoji / max_emoji_size_mb / content_filtration / cache_cleanup / keywords / regex / reaction(关键词反应)
- **response_post_process**: enable / typing_speed(1.0)
- **chinese_typo**: enable / error_rate / min_freq / tone_error_rate / word_replace_rate / enable_correction_quote / correction_quote_probability
- **response_splitter**: mode(rule|llm) / enable / max_length(512) / max_sentence_num(8) / max_split_num / enable_overflow_return_all / enable_kaomoji_protection
- **a_memorix**: enabled / storage.data_dir / embedding(model_name=auto/dimension/batch_size/enable_cache/quantization/fallback)/retrieval(策略/top_k/PPR/阈值) / integration(enable_memory_query_tool/memory_query_default_limit/enable_person_profile_query_tool/enable_person_profile_injection)(对齐 `AMemorixConfig` 核心面,裁剪 web_import/bundle 次要面)
- **mcp**: enable / client(client_name/enable_roots/sampling 等) / servers[] (name/enabled/transport[stdio|streamable_http|sse]/command/args/env/url/headers/timeouts/authorization/allow_users/allow_chats/permission)(对齐 `MCPConfig`)
- **webui**: enabled / host(list)/port(8001)/mode/webui_style
- **log / debug**

`model_config.toml`(ModelConfig):

- api_providers: name/base_url/api_key/client_type(openai)/max_retry/timeout/retry_interval
- models: model_identifier/name/api_provider/price_in/price_out/send_temperature/visual/extra_params
- model_task_config: **replyer / planner / utils / learner / fast_model / emoji / vlm / embedding**(+memory/mid_memory/image_embedding 可选),每任务 model_list/max_tokens/temperature/selection_strategy(random)/hard_timeout
- 默认模板、任务回退链(mid_memory→fast_model→utils 等)照抄 `default_model_config.py` 的语义

---

## 6. 关键实现规格(逐模块)

### 6.1 数据库表(对齐 `database_model.py`,SQLite)

- `mai_messages`: message_id/ts/platform/user_id/user_nickname/user_cardname/group_id/group_name/is_mentioned/is_at/session_id/reply_to/is_emoji/is_picture/is_notify/**raw_content(原 msgpack→改为 JSON 序列化的 MessageSequence)**/processed_plain_text/additional_config/reply_frequency
- `llm_usage`: 对齐 ModelUsage(含 prompt_cache_* 字段)
- `images`: image_hash/description/full_path/image_type(emoji|image)/query_count/is_registered/is_banned/no_file_flag/record_time/register_time/last_used_time/vlm_processed
- `tool_records`: tool_id/timestamp/session_id/tool_name/tool_reasoning/tool_data
- `expressions`: 对齐 Expression 表(situation/style/count/weight/banned/…)
- `jargons`: 对齐 Jargon 表
- `person_info`: person_id/platform/user_id/person_name/group_info/nickname/…/know_times/attitude 等
- `chat_sessions`: session_id/platform/user_id/group_id/…/created/last_active
- `behavior_*`(实验): behavior_action/behavior_outcome/scene cluster
- A_memorix 内核表在独立库 `data/a-memorix/a_memorix.db`(§6.14):paragraphs/entities/relations/episodes/profiles + 链接表 + FTS5 虚表 + vectors 表

### 6.2 消息与 session_id

- `session_id = md5(join(platform, [account:x], [scope:y], group_id 或 (user_id,"private")))` — 精确照抄 `utils_session.calculate_session_id`。
- 入站时间戳、message_id:渠道层生成(短随机 id `m{i}{rand}`,对齐 assign_message_ids)或用平台回执 id(发送的消息用平台返回 id)。

### 6.3 触发器(照抄权重与常数)

- `reply_likelihood`:截距 -1.2005 及 8 个特征权重**原样照抄**,特征提取(mention/at_other/question/placeholder_only/recent_self_ratio/recent_message_count/log1p(seconds)/pending_count)语义一致。
- `dynamic_gate`:3600s 窗口、0.91 强制触发均值、20 样本阈值、40s 虚拟轮次、11 档静态阈值表**原样照抄**。
- `frequency` 模式:阈值 = ceil(1/talk_value)(对齐 runtime `_get_message_trigger_threshold`),空窗补偿参数照抄。
- 空闲退避:`min(cap, base * 2^(count - start))`,只对群聊生效,被 bypass_pending_count 打断。

### 6.4 运行时状态机

- `_STATE_RUNNING / _STATE_WAIT / _STATE_STOP`;WAIT 由 wait 工具进入(带 tool_call_id),新消息或超时唤醒;连续 wait 上限 3。
- 打断:Planner 请求可被打断(`context.Context` 取消),`planner_interrupt_max_consecutive_count=0` 默认不限制关闭打断,>0 时打断后带新消息重跑。
- 内部轮次上限 10;每轮之间收集新到消息合入本轮(`_collect_pending_messages_before_next_round`)。

### 6.5 上下文裁剪(对齐 maisaka/context)

- 上限 max_context_size 条;裁剪时保留工具调用/结果成对(孤儿清理);回想消息折叠为单条 mid_term_memory;`count_in_context=false` 的参考消息不计数。
- 可见文本格式:`HH:MM:SS[msg_id:别名][名字]内容`(msg_id 显示用别名表,对齐 message_id_alias)。

### 6.6 Planner prompt(照抄模板文件)

- `maisaka_chat.prompt` / `maisaka_chat_focus.prompt`,变量:bot_name/behavior_style/group_chat_attention_block/query_memory_rule/(focus: planner_idle_focus_rule)。
- 群聊注意块 = "在该聊天中的注意事项:\n" + group_chat_prompt(配置默认文案照抄)。
- query_memory 规则按群/私两套文案照抄(chat_loop_service `_build_query_memory_rule`)。
- 每轮末尾追加 `PLANNER_FINAL_USER_REMINDER`("你需要输出对{bot_name}发言的分析…")。
- 工具定义注入可见工具 + deferred 工具列表说明(无插件时 deferred 集合只含 view_forward_message 等)。

### 6.7 reply 工具(最核心,严格对齐 reply.py)

- 参数:msg_id(必填)/set_quote(true)/reply_reference/reply_style(简短表达|正常回复|长回复)/expression_intent(结构化,use_vector_expression 开启时)/attach_at/attach_emoji(use_new_send_logic 时)。
- 目标消息查不到 → 失败回执;重复回复同一目标 → 附加 `_duplicate_target_reply_reminder`("你刚刚已经回复过这条消息…")。
- `reply_style=简短表达` 切换 retro 轻量模板(retro 模式下)。
- 发送流程:第一条 quote 目标、其余顺序发;每条间隔 = calculate_typing_time × typing_speed;任何一条失败即停止并回执失败。
- 成功回执文案照抄:`"{bot_name}"已生成并向"{target_user_name}"发送了回复"..."`。
- 发送成功后:消息写库 → 运行时历史追加 `guided_reply` 来源的 SessionBackedMessage → scheduler.record_reply()。

### 6.8 回复后处理(逐算法照抄)

- `_split_into_sentence_segments`:分隔符集合 {，, 空格 。; \n}、引号内不切、冒号两侧不切、数字/英文字母间空格不切、换行强制切;长度<12/32 分级合并概率 0.2/0.6/0.7;段尾分隔符保留供压缩恢复。
- `random_remove_punctuation`:结尾句号 90% 删、逗号 5% 删 20% 转空格(在 LLM 断句路径与合并时使用)。
- 错别字:gse 分词 + 字频表(depends-data/char_frequency.json)+ go-pinyin 声调/同音替换、整词替换率、纠正消息生成(50% 概率出纠正,纠正可引用前一条)。
- LLM 断句 prompt(`_SPLITTER_PROMPT`)与 no_split 处理照抄。
- 超长(中文占比<0.1 且 >2×max_length)→ 随机默认回复("麦麦不知道哦"等 7 条照抄);括号含中文内容剥离;"呃呃" 兜底。

### 6.9 记忆系统(上层行为对齐,底层为 A_memorix Go 重写)

MaiBot 的记忆分两层:**上层注入/工具行为**(maisaka/memory + services/memory_service)和**底层内核**(A_memorix)。Go 版两者都实现,上层行为严格对齐,内核按 §6.14 重写:

- **聊天回想(mid_term)**:上下文裁剪掉的消息按 `mid_term_memory_summary.prompt` 压缩为 {summary, recall_cues[3-5]};新一批触发 Planner 时,用最近消息 embedding × recall_cues 相似度召回(阈值 0.8)注入 Reference 消息(`【聊天回想-内部参考】`)。摘要文本经 `memory_service.ingest_summary` 写入 A_memorix。
- **启发式记忆**:群聊按可配置间隔,用最近消息构造 `heuristic_memory_impression.prompt` 检索记忆内核,命中以 `【启发式记忆-内部参考】` 注入。
- **人物画像**:person facts 写回内核 profile 存储;`enable_person_profile_injection` 开启时注入 Planner,`query_person_profile` 工具走 `memory_service.get_person_profile`。
- **工具 `query_memory`**:五种模式 search/time/hybrid/episode/aggregate **全部保留**,直接映射到内核 `search_memory`(§6.14)。
- **记忆自动写回**见 §6.15。

### 6.10 表情包系统

- 入站表情:Hash→Images 表登记,VLM 生成描述+情绪标签(content_analysis/filtration prompt 照抄);steal_emoji 开启时从入站表情学习(达到 max_reg_num 后按 do_replace 用 `emoji_replace.prompt` LLM 决策替换)。
- 出站(默认旧逻辑):send_emoji 工具 → 按情绪标签相似度 Top10 随机选一个发送。
- 新逻辑(use_new_send_logic):show_emoji_list 拼图(256px 格 + 64px 序号角标,x/image 合成)进入上下文,reply 的 attach_emoji 序号引用发送。
- send_emoji 子代理路径:候选拼图 + `emoji_selection.prompt`(JSON {emoji_index, reason})。

### 6.11 学习系统调度

- 表达学习:批次闸门(会话互斥 + 全局 max_expression_learner=3);从新回复/消息抽取(situation/style)候选 → self_reflect 检查 → 入库 → 向量索引增量更新。
- 表达选择(replyer 前):向量召回 pool(50) → 子代理按 `expression_select.prompt` 选 ≤5 → 渲染 expression_habits block(格式照抄 maisaka_expression_selector)。
- 黑话:miner 挖候选(共现+频次)→ `learn_jargon` 解释入库 → 上下文匹配器把命中黑话以参考消息注入 Planner。
- 行为学习(实验,最后做):scenario analyzer → 候选抽取 → 反馈评分 → `behavior_style_prompt` 注入 Planner。

### 6.12 QQ 官方发送层(双模发送 + 额度台账)

这是与 Diana 实现差异最大的地方,按官方文档实现:

- **全量事件**:优先订阅 `GROUP_MESSAGE_CREATE`(全量);`GROUP_AT_MESSAGE_CREATE` 仍需兼容(部分机器人未开全量)。两者字段一致,统一转 `SessionMessage`:
  - `mentions[].is_you == true` → `is_at=true`;文本中包含 bot 名/别名 → `is_mentioned=true`(对齐 `is_mentioned_bot_in_message` 语义)
  - `message_scene.ext.msg_idx` 登记 REFIDX;`msg_elements` 中 `message_type=103`(引用)按 ref_msg_idx 反查回填 `ReplyComponent`
  - attachments: content_type 区分 image/jpeg|png|gif / voice(+asr_refer_text) / video / file
- **发送决策(每条出站消息)**:
  1. 有触发 msg_id 且 5 分钟内且该 msg_id 已回复次数 < 5 → **被动回复**(msg_id + 自增 msg_seq)
  2. 否则 → **主动消息**(不带 msg_id),先过额度台账;被 40034100/40034105 拒时回执失败
  3. reply 工具的一次多段回复:第一段尽量被动回复目标消息,后续段超过被动次数上限后自动降级为主动消息发送
- **主动额度台账(内存 + 可选持久化)**:
  - 单群滑动窗口 20/qpm、每群每日计数 1000(每日 0 点重置)、机器人全局 60/30 qpm(按配置选)
  - 发送前检查,额度不足直接返回可识别错误(不发),并计入日志
  - 额度状态在发送成功后原子更新(避免并发超发)
- **重试**:40054005(去重)换 msg_seq 重试一次;40034004(转存失败)重试上传;401 重新换 token
- **自发消息过滤**:全量模式下平台会回推机器人自己的发言,靠「发送台账(2min TTL)+author.id」识别并丢弃,不进触发器但对齐 MaiBot 的 outbound_send 历史(发送成功时已主动写历史,回推的直接丢)

### 6.13 Prompt 渲染器

- `.prompt` 文件原样复制(只保留 zh-CN;en/ja 暂不搬)。
- 渲染语义对齐:`{var}` 替换;用户内容中的 `{`/`}` 在渲染前用私有占位符(0xFDE9/0xFDEA)保护再还原,防止聊天内容破坏模板。
- `.meta.toml` 读取 display_name/description(WebUI 没有也可留作文档)。
- `data/custom_prompts/zh-CN/*.prompt` 覆盖机制保留。

---

### 6.14 长期记忆内核(A_memorix → Go)

Python 版 7.1 万行,依赖 numpy/faiss/scipy。Go 版**保图 schema、接口语义与检索策略,重写实现**:

- **存储层**(SQLite,modernc.org/sqlite,单文件 `data/a-memorix/a_memorix.db`):
  - `paragraphs`(段落:hash/content/chat_id/时间/类型) + `entities`/`relations`(三元组:subject/predicate/object + hash 去重) + `episodes` + `profiles`
  - `paragraph_links` / `entity_links`:段落↔实体/关系关联
  - FTS5 虚表:paragraphs 与 relations 全文索引(modernc.org/sqlite 含 FTS5)
  - 向量:`vectors` 表(id/dim/量化/浮点 BLOB),内存中建 flat 内积索引(L2 归一化后余弦);规模 ≤ 百万级,无需 faiss,纯 Go 遍历 + SIMD 可后续优化
- **嵌入层**:走本项目的 embedding 任务配置(`model_task_config.embedding`);实现 Python 版语义:维度探测、profile 缓存、失败降级(允许 metadata-only 写入)
- **检索层(dual_path)**:
  - 策略:`paragraph_only` / `relation_only` / `dual_path`(默认),与 Python 版枚举一致
  - 段落路:embedding 余弦 TopK + Sparse BM25(gse 分词 + BM25 打分,替代 scipy) + 时间窗口过滤
  - 关系路:Aho-Corasick 实体匹配(纯 Go 实现较简单)→ 图召回 → Personalized PageRank(稀疏幂迭代,自实现)→ 图可靠性加权(grounding 因子 0.78/0.55/0.4/0.3 等常数照抄)
  - 融合:分数归一化 + `fuse_score_maps` 语义对齐;后验图门(`apply_posterior_graph_gate`)阈值语义对齐
  - 模式路由:search(语义)/time(时间窗)/hybrid(语义+时间)/episode/aggregate,与 `memory_search_service.py` 行为一致
- **写回层**:段落抽取(`a_memorix_chat_summary.prompt` 之外的底层抽取用 Python 版内置逻辑对齐)、实体/关系抽取(LLM + JSON 修复)、去重(compute_relation_hash)
- **内核接口(Go interface,对齐 SDKMemoryKernel 面向 MaiBot 暴露的面)**:`IngestSummary/IngestText/SearchMemory/GetPersonProfile/MaintainMemory/ImageMemory` + admin 面(图/源/episode/profile/fact/bundle 管理,供 WebUI)
- **配置**:`[a_memorix]` 节并入 bot_config.toml(`storage.data_dir`/`embedding`/`retrieval` 等),裁剪 Python 版 3900 行配置定义中的 Web 采集/导入等次要面,核心项(维度/量化/回退/检索阈值/PPR 参数)全保留
- **暂时不迁移**:web_import(网页导入)、bundle 打包导入、知识图谱可视化后端计算(前端图谱页只读展示已入库数据)

### 6.15 记忆自动写回(memory_flow_service)

三个后台 worker,Go 用 goroutine + 带 buffer 的 channel(maxsize 256 对齐)复刻:

- **PersonFactWritebackService**:消息发送后入队;抽取人物事实 → `person_fact_verifier` 校验(LLM)→ 写内核 profile;带历史事实重验循环(游标持久化到数据目录)
- **ChatSummaryWritebackService**:按累积消息量触发;`a_memorix_chat_summary.prompt` 总结 → ingest_summary
- **ImageMemoryWritebackService**:入站图片描述完成后写内核图片记忆(image_writeback_journal 保证重投递安全)
- 触发点对齐:`on_incoming_message` → image 写回;`on_message_sent` → fact + summary 写回

### 6.16 WebUI 后端(internal/webui)

- **技术**:Go `net/http`(或 gin)+ `gorilla/websocket`;API 形态对齐 FastAPI 版(REST + 统一 WS 通道)
- **鉴权**:对齐 `TokenManager` 语义——启动生成/读取 access token,日志打印一次,`Authorization` 头 + WS 握手临时 token(60s 有效期);支持配置固定 token 与重新生成
- **静态托管**:embed 前端 dist(golang `embed`)或磁盘目录,SPA fallback(Diana `spaHandler` 模式)
- **REST 路由**(对齐 `src/webui/routers/`,按优先级分批):
  - 核心:config(读写配置+schema)、model、system(状态/日志/公告)、logs(WS 流)
  - 资源:memory(对接内核 admin 面,Python 版 5400 行是最大单路由;按内核能力分批)、person、expression、jargon、emoji、behavior
  - 监控:reasoning_process(历史请求预览/思考链回放)、statistics、reply_effects
  - 聊天:chat(会话列表/消息序列/WebUI 内置聊天平台——若需要从网页与麦麦对话,对齐 `WEBUI_CHAT_PLATFORM` 机制,可作为虚拟平台接入 ChatManager,这是 WebUI 与聊天主链路的唯一交点)
  - 其他:mcp(服务器状态/热切换)、data_transfer(导出导入)、news(拉取公告列表,数据源 URL 配置化)、user_emoji
- **WS 事件**:对齐 `websocket/unified.py` 域模型:logs / maisaka_monitor(planner 快照、阶段状态、消息摄入/发送)/ chat 事件流;事件载荷对齐 `maisaka/monitor/events.py` 的 emit_* 系列字段(planner.progress/session_start/stage_status/message_ingested/message_sent/llm_retry/llm_error)
- **观察事件总线**:`internal/monitor` 提供 Go 事件 bus,聊天运行时调用 emit 函数,WebUI WS hub 订阅;无订阅者时零开销

### 6.17 WebUI 前端(web/dashboard)

- **技术栈对齐 MaiBot**:React 19 + Vite + TanStack Router/Query/Virtual + Tailwind 4 + shadcn 风格组件;**直接 fork `dashboard/` 源码改造**(MIT? 核实:dashboard 目录含独立 LICENSE 文件,改造时保留版权声明;GPLv3 主项目已覆盖)。改造点:
  - API client 对齐新后端路由(路径/字段尽量同形,减小改造量)
  - 移除插件系统页面(plugin-config/plugins/plugin-mirrors/plugin-webui)
  - Electron 打包分支按需保留或移除(建议先移除,纯 Web)
  - i18n 对齐(zh-CN 为主)
- **页面清单**(对齐 `dashboard/src/routes/`):首页概览、聊天(chat 页 + 消息渲染)、聊天管理、监控(maisaka/planner/replier monitor)、资源(表达/黑话/表情/行为/知识库/知识图谱)、人物、日志、MCP 设置、模型配置、配置中心、统计、数据传输、回复效果、WebUI 登录/初始化向导
- **news 页**:保留 UI,后端代理拉取远程公告(默认 URL 照抄,可配置)

### 6.18 MCP 客户端(internal/mcp)

- **SDK**:`github.com/modelcontextprotocol/go-sdk`(Diana 同款);stdio / streamable_http / sse 三种传输直接用 SDK 客户端能力,替代手写 connection.py(726 行)
- **管理器语义对齐 MCPService/Manager**:
  - 进程级单例持有连接,聊天 runtime 经 Provider 访问共享连接(不每会话起子进程)
  - 配置签名(`config_signature`)比对:配置热重载时仅在签名变化时重启连接;活跃 lease 退休机制(切换时旧 manager 等引用归零后关闭)
  - 状态快照(initialized/server_count/tool_count/servers)供 WebUI
- **工具接入**:MCP 工具注册到统一 ToolRegistry(provider_type="mcp"),与内置工具同台;工具 schema 透传;权限(allow_users/allow_chats/permission)语义照抄
- **高级能力**:Host 侧 Sampling 桥(服务器回调 sampling 请求 → 转发到 `planner` 任务 LLM,对齐 host_llm_bridge.py)、roots、elicitation——第一版实现 sampling,roots/elicitation 后补
- **deferred 集成**:MCP 工具默认进 deferred 列表,经内置 `tool_search` 工具发现后激活(与 view_forward_message 同机制)——这是 Planner prompt 中 tool_search 规则的真实用武之地,没有 MCP 时该规则也自然无害

## 7. 里程碑与验收

| 阶段 | 内容 | 验收标准 |
|---|---|---|
| **M0 骨架** | go.mod、目录、配置双文件加载/热重载、slog、SQLite 打开/WAL | `go build`;配置缺省合成正确 |
| **M1 渠道** | qq/ 全套:token、WS、心跳、全量+@事件→SessionMessage、被动回复+msg_seq、主动消息+额度台账、媒体上传 | 真机收到群内全部消息并记录;@ 消息被动回复;超窗后走主动消息;发一张图片 |
| **M2 LLM 层** | ContextItem、OpenAI 兼容客户端(工具调用)、任务路由/超时/重试、usage 记账 | 给定 model_task_config 正确路由;工具调用往返;usage 入库 |
| **M3 消息链路** | ChatManager、session_id、消息入库、HeartflowManager、runtime 骨架(只做日志) | 群里任意消息后日志出现「[群名]昵称:内容」;DB 有记录;@ 与提及被正确标记 |
| **M4 最小智能体闭环** | 触发器(frequency)→Planner(maisaka_chat.prompt)→reply 工具→replyer(maisaka_replyer.prompt,无表达/无后处理直出)→发送 | **非 @ 的群聊消息也能触发回复**(MaiBot 全量语义);不回复/等待由 wait 决定;@ 必回 |
| **M5 回复后处理** | 断句(规则+LLM)、错别字、颜文字保护、默认回复降级、打字间隔逐条发送 | 回复呈多条短消息、节奏拟人;被动次数超限后后续段降级主动;单元测试覆盖切分边界 |
| **M6 表达系统** | 表达学习 + 向量索引 + 选择器 + habits 注入 | 学到风格影响回复措辞;索引持久化可重建 |
| **M7 A_memorix 内核** | 存储层(段落/图/FTS5/向量)→ 嵌入接入 → 双路检索(BM25/AC/PPR/融合)→ 抽取写回 → search/time/hybrid 模式 | 写入对话后能按语义/时间检索命中;单测对齐融合常数 |
| **M8 记忆接入** | query_memory 五模式工具、启发式拉起、人物画像注入、memory_flow 三 worker 写回、聊天回想接入内核 | 长对话被裁后能想起早前话题;自动写回的摘要/事实可被检索 |
| **M9 表情包** | 入站登记/VLM 描述/偷表情;send_emoji(旧逻辑)→(新逻辑拼图) | 会发表情包;能偷表情并按情绪匹配 |
| **M10 黑话+人物** | jargon 学习/注入;person facts | 群黑话能被解释使用;能查到某人信息 |
| **M11 dynamic 模式** | reply_likelihood + 动态门控 + 空闲退避完整照抄;打断 | 回复频率趋近 talk_value;单测校验权重输出 |
| **M12 MCP** | go-sdk 三传输、Manager 签名热切换、工具进 ToolRegistry + deferred、Sampling 桥 | 挂载一个 stdio 与一个 HTTP 服务器,Planner 能经 tool_search 发现并调用 |
| **M13 WebUI 后端** | HTTP 服务器、TokenManager 鉴权、WS hub + monitor 事件 bus、config/model/system/logs 路由 | 浏览器登录后可看/改配置、看实时日志与 Planner 快照 |
| **M14 WebUI 前端** | fork dashboard、API 对齐改造、移除插件页、嵌入静态托管 | 全页面可用;聊天页可从网页与麦麦对话(WEBUI_CHAT_PLATFORM) |
| **M15 打磨** | memory/jargon/emoji/behavior 资源页接内核 admin、知识图谱页、统计、reply_effect、数据导出导入 | 配置开关行为正确;资源页 CRUD 生效 |

顺序说明:M7/M8(记忆)与 M12(MCP)、M13/M14(WebUI)可并行推进;M13 建议在 M4 闭环后就先搭起来(日志/监控页对调试心流极有用)。

每个阶段:先写核心实现,再补 go test(gates/likelihood/splitter/typo/session_id/render 是纯函数,重点测)。

---

## 8. 需要复刻的"照抄清单"(防走样)

- `prompts/zh-CN/*.prompt` 全部文件 + `.meta.toml`
- `reply_likelihood.py` 权重、`dynamic_gate.py` 阈值表与常数
- `utils.py` 断句/合并/标点/默认回复/typing time 的参数与分支
- `typo_generator.py` 的错误率语义(单字/声调/整词/频差)
- 配置默认值与字段名(含 `mid_term_memory_lenth` 的拼写,方便用户迁移配置)
- reply 工具回执文案、重复回复提醒文案、no-tool 重试提示
- 人设拼接:`你的名字是{bot_name}，也有人叫你{aliases}。` + 人格 + 情绪后缀
- 群/私聊默认 group_chat_prompt / private_chat_prompts 文案
- LLM 断句 prompt、mid_term summary prompt 的输出格式约束
- A_memorix 双路检索常数:grounding 因子(0.78/0.55/0.4/0.3)、图可靠性权重(0.65/0.10/0.25)、PPR alpha、融合/校准语义
- monitor 事件字段(planner.progress/session_start/stage_status/message_ingested/message_sent/llm_retry)与 WS 域/主题模型(logs/maisaka_monitor/chat)
- MCP 配置 schema(servers: name/enabled/transport/command/args/env/url/headers/authorization/allow_*)与配置签名热切换语义

---

## 9. 风险与注意事项

1. **许可证**:MaiBot 主程序为 **GPLv3 + EULA**。照抄其提示词与算法行为并分发,本项目需以 GPLv3 兼容方式发布,并在 README 注明衍生来源;Diana 是 Limited Redistribution License,**只参考其公开仓库中的技术实现思路,不复制其源码文件文本**(QQ 官方协议本身是公开 API,按官方文档自行实现即可)。
2. **QQ 官方平台限制**:被动回复限 5 分钟内、每条消息最多 5 次;主动消息受频控(单群 20/qpm、每群每日 1000 条、全局 60/30 qpm)且需在管理端开启「主动发言」与「接收所有消息」能力;上线前用沙箱环境(`Sandbox: true`)联调。
3. **并发模型**:MaiBot 每会话一个 asyncio 任务 → Go 每会话一个 goroutine + channel 队列;SQLite 单写,沿用 Diana 的写串行化/读连接池经验;所有 LLM 调用带 hard_timeout。
4. **numpy/faiss → 手写向量**:表达索引聚类(kmeans)与 MMR 多样性采样、A_memorix 向量检索需自行实现并测试数值稳定性;表达索引规模小(≤数千)可控;记忆段落向量按 L2 归一化 + flat 内积遍历,十万级以下延迟可接受(毫秒级),更大规模再引入量化或 ANN 索引。
5. **jieba/pypinyin → gse/go-pinyin**:分词粒度差异会轻微改变错别字分布与 BM25 切词,可接受;字频表直接复用 MaiBot 的 char_frequency.json。
6. **msgpack → JSON**:raw_content 编码改为 JSON,不迁移旧库(全新部署)。
7. **A_memorix 范围控制**:Python 版 7.1 万行是本项目最大的单块工作;重写聚焦「MaiBot 宿主实际调用面」(ingest/search/profile/image/admin 核心动作)与「检索质量常数」;web_import/bundle 导入/图谱可视化后端计算不迁移;**内核行为验收以 memory_service 门面接口的语义对齐为准**,不追求内部代码结构一一对应。
8. **dashboard 许可核实**:dashboard/ 目录带独立 LICENSE 文件,fork 前先读,确认与 GPLv3 主项目的关系及版权声明保留要求。
9. **WebUI 安全**:默认仅监听本机(127.0.0.1/::1);access token 首启打印一次;公网部署必须在文档中强调反代 + HTTPS + 改 token。
10. **风险—主动额度**:每群每日 1000 条主动消息;高频群 + talk_value 偏高时可能触顶。发送层台账要提前在额度将尽时打 warning 日志;触发器本身(MaiBot 频率门控)天然会把实际发言频率控制在 talk_value 附近,talk_value=1(每条都想插话)的极端配置会快速烧完额度——README 里要提示用户按额度设置 talk_value。
11. **行为对齐验证**:M4/M5/M11 阶段各做一次与 Python 版的对照测试(同输入消息序列,比较触发决策与 prompt 渲染文本 diff);记忆检索质量用同一组对话语料在 Python/Go 两版跑 search 对比 TopK 重合率。

---

## 10. 首批要落地的文件清单(M0+M1)

1. `go.mod`、`cmd/bot/main.go`
2. `internal/config/*` + `config/*.toml` 模板
3. `internal/database/*`
4. `internal/message/*`
5. `internal/qq/*`(按官方 API 文档实现,结构参考 §1.3)
6. `internal/prompt/*` + `prompts/zh-CN/`(复制)
7. `internal/webui/` 最小骨架(server/auth/静态托管 + config/system 路由)——尽早可浏览器访问,后续路由按里程碑逐个补
8. Makefile(build/test/run + dashboard build 同步)
