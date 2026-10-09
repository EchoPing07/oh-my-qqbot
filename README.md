# oh-my-qqbot

**MaiBot 的 Go 重写版** —— 面向 QQ 官方开放平台的群聊智能体（心流/Planner + replyer 双阶段架构）。

本项目是 [MaiBot](https://github.com/Mai-with-u/MaiBot)（麦麦）聊天智能体的 Go 语言重写实现：
对话行为（触发器频率门控、Planner 决策、replyer 生成、断句/错别字后处理、表达学习、
黑话、记忆检索、表情包、回复效果观察）严格对齐 MaiBot 的算法与提示词文案，
工程实现（QQ 官方渠道、OpenAI 兼容 LLM 客户端、SQLite 存储、WebUI）参照
[Diana](https://github.com/Mai-with-u/Diana) 的公开技术思路独立实现。

---

## 目录

- [开源声明与衍生来源](#开源声明与衍生来源)
- [功能概览](#功能概览)
- [快速开始](#快速开始)
- [模块结构](#模块结构)
- [⚠️ 主动消息额度提示（务必阅读）](#-主动消息额度提示务必阅读)
- [QQ 官方平台能力要求](#qq-官方平台能力要求)
- [WebUI](#webui)
- [开发与测试](#开发与测试)

---

## 开源声明与衍生来源

- 本项目以 **GPLv3 兼容方式** 发布（见仓库根 `LICENSE`）。MaiBot 主程序为
  **GPLv3 + EULA** 双许可；本项目的提示词文件、算法行为与配置字段名大量源自 MaiBot，
  因此**任何再分发必须保持 GPLv3 兼容**，并保留原始版权声明。
- **衍生来源（务必注明）**：
  - 对话行为、提示词（`prompts/zh-CN/*.prompt`）、配置字段名与默认值、
    触发器权重、断句/错别字/默认回复参数、回复效果观察系统 —
    均衍生自 **MaiBot**（麦麦，<https://github.com/Mai-with-u/MaiBot>，GPLv3 + EULA）。
  - WebUI 前端（`web/dashboard/`）fork 自 MaiBot 的 `dashboard/`（**AGPLv3**，
    见 `web/dashboard/LICENSE`），改造时保留其版权声明；前端整体受 AGPLv3 约束。
  - QQ 官方开放平台渠道、OpenAI 兼容客户端骨架、SQLite「单写多读 + WAL」模式的
    工程思路参考 **Diana**（<https://github.com/Mai-with-u/Diana>，Limited Redistribution
    License）。本项目**未复制其源码文本**，QQ 协议按腾讯官方公开文档自行实现。
  - 错别字字频表 `depends-data/char_frequency.json` 随 MaiBot 仓库带入。
- 本项目与 MaiBot 官方、腾讯 QQ 官方均无隶属关系，为社区独立重写实现。

---

## 功能概览

| 能力 | 说明 |
|---|---|
| QQ 官方渠道 | WS 网关（心跳/RESUME）、**全量群消息** `GROUP_MESSAGE_CREATE` 与 @ 消息、C2C 私聊、被动回复 + 主动消息双模发送、REFIDX 引用解析、富媒体分片上传 |
| 主动额度台账 | 单群 20/qpm 滑动窗口、每群每日 1000 条、机器人全局 qpm，原子扣减，额度将尽打 warning |
| 心流（Planner） | 每会话一个 goroutine + 消息队列；Planner 决策 → `reply`/`wait`/工具 → replyer 生成 → 后处理 → 逐条发送 |
| 触发器 | 频率门控（talk_value）+ dynamic 动态门控（回复概率逻辑回归、空闲退避、打断）双模 |
| 回复生成 | 人设/风格拼接、表达方式向量选择、LLM 断句、中文错别字、颜文字保护、默认回复降级、拟人打字延迟 |
| 记忆 | mid-term 聊天回想、启发式记忆拉起、人物画像注入、memory_flow 三 worker 自动写回；底层为 A_memorix 内核 Go 重写（FTS5 + 向量 + BM25/AC/PPR 双路融合） |
| 学习系统 | 表达学习（含聚类 + MMR 多样性）、黑话挖掘与解释、行为学习（实验开关，默认关） |
| 表情包 | 入站登记、VLM 描述、偷表情、按情绪 Top10 选择（旧逻辑）/ 拼图选择（新逻辑） |
| 工具与 MCP | 内置工具（reply/wait/query_memory/send_emoji/view_forward_message/fetch_history/tool_search…）+ MCP 三传输（stdio/streamable_http/sse）客户端与 Sampling 桥 |
| 回复效果观察 | **实验性（默认关）**：回复发送后随访 30 分钟，用 LLM 评审后续反馈并打分落库 |
| WebUI | 后端 REST + WebSocket（日志/监控/聊天域）+ 配置中心；前端 React 19 + Vite + TanStack |
| 数据迁移 | 全量业务数据（消息/人物/表达/黑话/图片描述）JSON 打包导出与导入（幂等、可选择性覆盖） |

---

## 快速开始

### 1. 环境要求

- Go **1.26+**（`go.mod` 的 go 指令）
- 一个 QQ 开放平台的**机器人应用**（AppID + AppSecret），且已在管理端开启
  「接收所有消息」「主动发言」能力（详见下文）
- 一个 OpenAI 兼容的 LLM 服务（官方 API、中转或本地部署均可）
- 可选：Node.js 20+（仅在需要重新构建 WebUI 前端时）

### 2. 获取代码与构建

```bash
# 克隆（仓库根目录含 go.mod）
git clone <repo-url> oh-my-qqbot
cd oh-my-qqbot

# 编译全部包（当前仓库的模块实现已完整，可 go build 全绿）
go build ./...

# 主程序入口 cmd/bot/main.go（装配层：把 config/qq/llm/chat/webui 接起来）
# 由项目收尾步骤落地；落地后：
go build -o bin/bot ./cmd/bot

# 也可先只跑测试验证模块行为
go test ./internal/...
```

> 若 `cmd/bot` 尚无入口文件，说明装配层尚未落地：此时可直接读单测与
> `docs/REWRITE_PLAN.md` 了解各模块行为，各包之间均以最小接口解耦、可单独测试。

### 3. 配置

首次启动会在 `config/` 目录**自动生成默认配置模板**（缺失即生成，与 MaiBot 行为一致）：

```
config/
├── bot_config.toml      # 机器人行为配置（人设/触发器/表情包/日志/WebUI…）
└── model_config.toml    # LLM 供应商、模型与任务路由配置
```

需要手工填写的关键项：

**`config/bot_config.toml`**

```toml
[bot]
nickname = "麦麦"          # 机器人自称，用于判断是否被 @ / 提及
alias_names = []            # 别名列表

[chat.reply_timing]
talk_value = 0.3            # 主动插话频率：1 = 每条消息都想回复（⚠️ 见额度提示）
private_talk_value = 0.5
mentioned_bot_reply = false # 被提及时是否必回
reply_trigger_mode = "frequency"  # frequency | dynamic

[chat.reply_style]
group_chat_prompt = "..."   # 群聊额外人设要求（默认文案沿用 MaiBot）

[webui]
enabled = true
host = ["127.0.0.1"]        # 默认仅监听本机，公网部署请务必反代 + HTTPS
port = 8001
```

**`config/model_config.toml`**

```toml
[[api_providers]]
name = "my-provider"
base_url = "https://api.example.com/v1"
api_key = "sk-xxxx"
client_type = "openai"

[[models]]
model_identifier = "gpt-4o-mini"
name = "默认模型"
api_provider = "my-provider"
send_temperature = 0.8

[model_task_config.replyer]
model_list = ["默认模型"]
[model_task_config.planner]
model_list = ["默认模型"]
[model_task_config.utils]      # 小任务：断句/摘要/黑话/评审（回复效果评审也走这里）
model_list = ["默认模型"]
```

**QQ 渠道凭据**：填入 `bot_config.toml`（字段名以装配层/maiBot 字段对齐为准），
或由装配层从环境变量读取；**不要把 AppSecret 提交到仓库**。凭据最终交给
`internal/qq.APIOptions{AppID, AppSecret, Sandbox}`。

### 4. 运行

```bash
# 装配层落地后：
./bin/bot
```

启动后：

- 控制台打印 WebUI 访问地址与**一次性 access token**（首次启动生成，日志只打印一次）；
- 浏览器打开 `http://127.0.0.1:8001`，用该 token 登录；
- 群里有人说话，日志出现 `[群名]昵称:内容`，即表示全量消息链路已通。

### 5. 建议的首次联调顺序

1. 用 `Sandbox: true`（沙箱环境）配置联调，避免误发到正式群；
2. 确认「接收所有消息」已开启 —— 否则只能收到 @ 消息，触发器无法工作；
3. 先用 `talk_value = 0.1` 左右观察回复节奏，再按群活跃度调整；
4. 打开 WebUI 的「监控」页看 Planner 快照，「日志」页看实时日志。

---

## 模块结构

| 路径 | 职责 |
|---|---|
| `cmd/bot/` | 主程序入口：装配、启动、优雅关停（收尾步骤落地） |
| `internal/config/` | `bot_config.toml` / `model_config.toml` 双文件加载、缺省合成、热重载（fsnotify） |
| `internal/database/` | SQLite（`modernc.org/sqlite`，纯 Go 无 CGO）；WAL、单写多读、建表迁移 |
| `internal/message/` | 组件化消息模型（Text/Image/Emoji/Voice/At/Reply/Forward…）、`session_id` 计算、JSON 序列化 |
| `internal/llm/` | OpenAI 兼容客户端、任务路由（replyer/planner/utils/…）、超时重试、usage 记账、embedding |
| `internal/qq/` | QQ 官方渠道：token、WS 网关、REFIDX、媒体分片上传、被动/主动双模发送、主动额度台账 |
| `internal/chat/manager/` | ChatManager / ChatSession 持久化、消息入库 |
| `internal/chat/heartflow/` | HeartflowManager（LRU + 保活） |
| `internal/chat/trigger/` | 频率门控 + dynamic 门控（回复概率、空闲退避）、MessageTurnScheduler |
| `internal/chat/runtime/` | 心流状态机：Planner 请求组装、打断、等待、引擎循环 |
| `internal/chat/context/` | LLM 上下文抽象、裁剪、孤儿工具结果清理、mid-term 折叠、可见文本渲染 |
| `internal/chat/tools/` | 内置工具（`reply`/`wait`/`query_memory`/`send_emoji`/`tool_search`…）、工具注册表 |
| `internal/chat/replyer/` | 回复生成器、表达选择、后处理（断句/错别字/颜文字）、拟人打字延迟 |
| `internal/chat/sender/` | send_service：出站构造 → 渠道发送 → 存库 → 历史回写 |
| **`internal/chat/replyeffect/`** | **回复效果观察**（实验）：注册 → 随访 → LLM 评审 → 打分落库 `maisaka_reply_effects` |
| **`internal/chat/statistics/`** | **统计聚合**：消息量/回复量/触发率（按会话、按天、按小时）+ LLM 用量摘要/模型/时间序列 |
| `internal/learners/` | 表达学习、向量索引（余弦 + 聚类 + MMR）、黑话挖掘、行为学习（实验） |
| `internal/memory/` | mid-term 回想、启发式拉起、人物画像、memory_flow 三 worker |
| `internal/memorix/` | A_memorix 内核 Go 重写：存储/FTS5/向量/BM25/AC/PPR/双路融合 |
| `internal/emoji/`, `internal/image/` | 表情包管理与拼图、图片保存与 VLM 描述 |
| `internal/person/` | 人物信息、`person_id`（md5）、群名片 |
| `internal/monitor/` | 观察事件 bus（Planner 快照/消息摄入/发送），零订阅零开销 |
| `internal/mcp/` | MCP 客户端（go-sdk 三传输）、签名热切换、工具接入与 Sampling 桥 |
| `internal/webui/` | HTTP 服务器、TokenManager 鉴权、WS hub、SPA 静态托管 |
| `internal/webui/routers/` | REST 路由：config/model/system/logs/memory/person/expression/jargon/emoji/statistics/reply_effects/chat/mcp/data_transfer/news… |
| **`internal/transfer/`** | **全量数据导出/导入**：消息/人物/表达/黑话/图片描述 JSON 打包（幂等导入） |
| `internal/prompt/` | `.prompt` 加载（zh-CN）、`{var}` 模板渲染、`.meta.toml` |
| `prompts/zh-CN/` | 提示词文件（原样对齐 MaiBot） |
| `depends-data/` | 错别字字频表等随仓库数据 |
| `web/dashboard/` | WebUI 前端（React 19 + Vite + TanStack Router/Query + Tailwind 4） |
| `docs/REWRITE_PLAN.md` | 重写计划：模块规格、字段级对齐清单、里程碑验收 |

---

## ⚠️ 主动消息额度提示（务必阅读）

QQ 开放平台对**主动消息**（不带触发 `msg_id` 的发送）有硬性频控，超额会被平台拒绝
（错误码 `40034100` / `40034105`）：

| 限制 | 数值 |
|---|---|
| **每群每日主动消息上限** | **1000 条**（每日 0 点重置） |
| 单群主动消息频率 | 20 条 / 分钟（滑动窗口） |
| 机器人全局主动消息频率 | 60 条 / 30 分钟（按配置选档） |
| 被动回复窗口 | 触发消息后 **5 分钟内**，最多 5 次 |

**这意味着：一个活跃群里的机器人，每天最多只能主动发言 1000 次。**

### 怎么设置才安全

`talk_value` 是「每条群消息想插话的概率」的上限。实测经验（按日均 2000 条消息的活跃群估算）：

| `talk_value` | 大致主动发言量 | 风险 |
|---|---|---|
| `0.05` | 每日约 100 条 | ✅ 安全，适合高活跃大群 |
| `0.1` | 每日约 200 条 | ✅ 推荐起步值 |
| `0.3` | 每日约 600 条 | ⚠️ 大群需要观察额度余量 |
| `0.5` | 每日约 1000 条 | ⚠️ 可能触顶，建议密切观察日志 |
| `1.0` | 每条都想插话 | ❌ **会快速烧完额度**，当天后续主动消息全部失败 |

**建议**：

1. `talk_value` 从 **`0.1`** 起步，观察 WebUI 的额度/日志，再逐步上调；
2. 高活跃群（> 3000 条/日）不要超过 `0.2`；把不重要的群用 `chat_prompts` 或
   关键词反应降权；
3. 群内被 **@** 的回复走**被动回复**，**不消耗主动额度** —— 所以
   `mentioned_bot_reply = true` / `inevitable_at_reply = true` 是「保底可回」的选项；
4. 发送层会在额度将尽时打 `warning` 日志（关键词含额度/主动消息），请配置日志告警；
5. 一次性多段回复里，第 2 段之后超过被动次数上限的部分会**降级为主动消息**，
   也计入 1000 条/日 —— 长回复会更快消耗额度，`response_splitter.max_sentence_num` 别调太大。

> 参考：`docs/REWRITE_PLAN.md` §9.10「风险—主动额度」。

---

## QQ 官方平台能力要求

机器人应用必须在 **[QQ 开放平台](https://q.qq.com/)** 管理端开启以下能力，否则功能不完整：

| 能力 | 为什么需要 | 不开启的后果 |
|---|---|---|
| **接收所有消息**（群聊全量事件 `GROUP_MESSAGE_CREATE`） | MaiBot 的触发器基于「群里全部消息」判断是否插话 | 只能收到 @ 消息 → 机器人变成「只在被 @ 时回复」，主动插话完全不工作 |
| **主动发言**（发送主动消息） | 回复非 @ 消息、多段回复超出被动次数后降级、定时主动消息 | 只能被动回复 @ → 大部分回复发送失败（`40034105`） |
| **全量事件订阅开关**（事件推送配置中的全量消息/私聊消息） | 私聊 `C2C_MESSAGE_CREATE` 与群全量事件靠事件订阅下发 | 私聊消息收不到 |
| **群聊场景** + 机器人入群 | 群内交互前提 | 群里 @ 无响应 |
| **富媒体上传**（图片/表情包/语音） | 表情包系统与图片回复 | 只能发纯文本 |
| **沙箱环境**（联调阶段） | 沙箱里可以任意测试不打扰真实用户 | 调试期间打扰真实群成员 |

补充说明：

- 事件订阅请优先选择 **`GROUP_MESSAGE_CREATE`（全量）**；本项目同时兼容
  `GROUP_AT_MESSAGE_CREATE`，部分机器人未开全量时仍能工作（能力降级）；
- 机器人自己的发言在全量模式下会被平台**回推**，本项目按「发送台账 + 作者 id」识别并丢弃；
- 每次被动回复携带触发消息的 `msg_id` 与自增 `msg_seq`，平台据此判重与限次。

---

## WebUI

- 默认监听 `127.0.0.1:8001`，access token 首启打印一次（WebUI 配置页可自行修改）；
- **公网部署必须**：反向代理 + HTTPS + 修改默认 token + 限制来源 IP；
- 主要页面：概览、聊天（可直接与麦麦对话，走虚拟平台 `webui`）、监控（Planner 快照）、
  资源（表达/黑话/表情/行为/知识库/知识图谱）、人物、日志、模型配置、配置中心、
  统计、数据传输、回复效果、MCP 设置；
- 前端源码在 `web/dashboard/`（AGPLv3）；改完后 `npm install && npm run build`
  产出 `dist/`，由后端静态托管。

---

## 开发与测试

```bash
# 全量构建
go build ./...

# 全量测试（含 SQLite 临时库集成测试）
go test ./internal/...

# 静态检查
go vet ./...

# 只跑本次新增的模块
go test ./internal/chat/replyeffect/... ./internal/chat/statistics/... ./internal/transfer/...

# WebUI 前端
cd web/dashboard && npm install && npm run build
```

对齐验证（`docs/REWRITE_PLAN.md` §9.11）：M4/M5/M11 各阶段与 Python 版做对照测试
（同输入消息序列比较触发决策与 prompt 渲染 diff）；记忆检索用同一语料对比 TopK 重合率。

---

## 许可

GPLv3（见 `LICENSE`）。衍生自 MaiBot（GPLv3 + EULA）；WebUI 前端 fork 自 MaiBot
`dashboard/`（AGPLv3）。再分发时请保留全部原始版权声明与来源说明。
