# 前端页面开发工作包（子代理契约）

> 仓库：`D:/Coding/oh-my-qqbot`，前端目录 `web/dashboard/`。
> 设计规范：黑白灰线条风（参考 `D:/Coding/Buddy-2API-Go/internal/web/assets/app.css` 的克制风格）。
> 铁律：**只调用后端真实存在的 API**（见下方清单）；**不改** `src/index.css`、`src/lib/**`、`src/api/**`、`src/components/ui.tsx`、`src/components/Layout.tsx`、`src/router.tsx`、`src/main.tsx`。如需新工具函数，写在页面文件内部或 `src/pages/<Page>/helpers.ts`。

## 已有基础设施（直接用，不要重复造轮子）

- 请求：`import { api } from '@/lib/http'`（Bearer + 401 自动跳登录 + success 包络校验）；匿名用 `anonApi`。错误统一 `ApiError`。
- 格式化：`@/lib/format`：`formatBytes/compactNumber/formatNumber/formatUptime/formatTime/formatDateTime/formatRelative/truncate/jsonEqual/sessionTitle/parseBackendTime`。
- 通知：`import { toast } from '@/lib/toast'`；`toast.ok('...') / toast.err('...') / toast.info('...')`。
- UI 原语 `@/components/ui`：`Modal, ConfirmModal, MenuProvider, useMenu (openMenu(e, items)), Field, TextInput, TextArea, SelectInput, Toggle, Banner(kind info|warn|err|ok), Pagination(page,pageSize,total,onPage), Empty({icon,text}), Loading({text})`。
- classnames：`import { cn } from '@/lib/cn'`。
- 数据获取：`@tanstack/react-query` 的 `useQuery/useMutation/useQueryClient`（client 已全局配置）。
- 路由：`@tanstack/react-router` 的 `Link/useNavigate/useLocation`；路由已在 `src/router.tsx` 注册，不要新加路由。
- 图标：`lucide-react`（14~16px）。
- WS 事件：`import { unifiedWs } from '@/lib/ws'`；`unifiedWs.subscribe(domain, topic)` 返回退订函数；`unifiedWs.onEvent(env => ...)` 收事件（`env.domain/event/data`）。协议见 `internal/webui/wshub.go`。
- CSS：全部用 `src/index.css` 已定义的类：`page/page-head/page-actions/page-wide/card/card-head/card-body/card-body-flush/grid/grid-metrics/grid-2/metric/metric-head/metric-label/metric-value/metric-detail/table-wrap/btn/btn-secondary/btn-ghost/btn-danger/btn-sm/btn-lg/btn-block/btn-icon/badge/badge-ok/badge-warn/badge-err/badge-solid/dot/dot-ok/dot-warn/dot-err/empty/loading-block/kv-grid/kv/k/.v/banner/banner-err/banner-warn/banner-ok/label/input/select/textarea/input-err/field-hint/modal/overlay/tabs/tab/menu/menu-item/pagination/progress/progress-fill/code-block/disclosure/chat-*/log-*`。布局骨架类：`shell/sidebar/sidenav/nav-item/nav-group-title/topbar/main/content`。间距/小样式用内联 style。
- 页面骨架模板：

```tsx
export function XxxPage() {
  return (
    <div className="page">
      <div className="page-head">
        <div>
          <h1>标题</h1>
          <p className="page-sub">说明</p>
        </div>
        <div className="page-actions">{/* 操作按钮 */}</div>
      </div>
      {/* 卡片/表格/网格 */}
    </div>
  )
}
```

## 后端 API 清单（全部前缀 `/api/webui`，响应含 `success/message` 包络，http.ts 已解包）

### 认证（api/auth.ts，不要直接调，用封装）
- POST `/auth/verify` {token} → {valid,message,is_first_setup}
- GET `/auth/check`（带 Bearer）→ {authenticated,token_source,requires_custom_token}
- GET `/setup/status`（匿名可访问）→ {is_first_setup,token_source,requires_custom_token,authenticated}
- POST `/auth/update` {new_token} → {success,message}（密码规则：≥10位，含大小写+特殊符号 `!@#$%^&*()_+-=[]{}|;:,.<>?/`）
- POST `/auth/regenerate` → {success,token,message}
- POST `/setup/complete` / POST `/setup/reset`

### 系统/统计（api/system.ts）
- GET `/system/status` → {running,uptime,version,start_time,go_version,memory:{heap_alloc_bytes,heap_sys_bytes,sys_bytes,num_goroutine,num_cpu,gc_count}}
- GET `/system/news`、GET `/news` → {items:[{id,title,content,url,datetime}]}
- POST `/system/reload-config`（热重载配置）
- GET `/statistics/summary?hours=N` → {hours,request_count,prompt_tokens,completion_tokens,total_tokens,cache_hit_tokens,cache_miss_tokens,total_cost,avg_response_time}
- GET `/statistics/models?hours=N` → {models:[{model_name,request_count,total_cost,input_tokens,output_tokens,total_tokens,cache_hit_tokens,cache_miss_tokens,avg_response_time}]}
- GET `/statistics/daily?days=N` → {days:[{day,requests,tokens,cost}]}

### 配置（api/config.ts）
- GET `/config/bot` → {config:{...TOML键名字典}}（节：bot/personality/chat/experimental/visual/expression/jargon/a_memorix/message_receive/emoji/keyword_reaction/response_post_process/chinese_typo/response_splitter/log/debug/mcp/webui/database）
- POST `/config/bot` {config:{...整份}}
- GET `/config/model` → {config:{api_providers:[{name,base_url,api_key,client_type,max_retry,timeout,retry_interval}],models:[{name,model_identifier,api_provider,price_in,price_out,visual,force_stream_mode,extra_params}],model_task_config:{replyer:{model_list,...},...}}}
- POST `/config/model` {config:{...整份}}
- GET `/config/schema/bot` → {schema:{properties:{节名:{title,type}}}}
- 便捷封装：`getBotSection(section)` / `saveBotSection(section, data)`（读整份覆盖一节后整份保存）

### 模型（api/models.ts）
- GET `/models/list` → {models:[{name,model_identifier,api_provider,base_url,price_in,price_out,visual,force_stream_mode,extra_params,tasks:[...]}]}
- GET `/models/providers` → {providers:[{name,base_url,client_type,max_retry,timeout,retry_interval,has_api_key}]}
- GET `/models/task-config` → {tasks:[{task,model_list,max_tokens,temperature,selection_strategy,hard_timeout,fallback_chain}]}
- 任务固定顺序：replyer/planner/memory/mid_memory/utils/learner/fast_model/emoji/vlm/voice/embedding/image_embedding

### 聊天（api/chat.ts）
- GET `/chat/sessions?limit=N` → {sessions:[{session_id,platform,is_group,group_id,group_name,user_id,user_nickname,created_at,last_active_at}]}
- GET `/chat/history?session_id=X&limit=N` → {messages:[{message_id,time,platform,user_id,user_nickname,group_id,session_id,is_emoji,is_picture,is_mentioned,is_at,reply_to,text,raw_content}]}
- POST `/chat/send` {content,user_name} → {accepted,platform,time}
- GET `/chat/platforms` → {platforms:[{platform,name,type,enabled}]}
- 注意：`/chat/send` 需要装配层注入 ChatInjector，未注入时返回 503「WebUI 聊天平台未接线」— 页面必须优雅处理（Banner 提示而非崩溃）。

### 资源（api/resources.ts）
- GET `/person/list?limit&offset` → {items:[{person_id,person_name,user_nickname,platform,user_id,know_times,memory_points,is_known,last_known}]}
- GET `/person/stats/summary` → {total,known}
- DELETE `/person/{person_id}`
- GET `/expression/list?limit&offset` → {items:[{id,situation,expression,style,count,checked,modified_by,created_by}]}
- DELETE `/expression/{id}`；PUT `/expression/{id}/check` {checked:boolean}
- GET `/jargon/list?limit&offset` → {items:[{id,jargon,meaning,explanation,count,is_jargon,created_by}]}
- PUT `/jargon/{id}` {is_jargon:boolean,meaning?:string}；DELETE `/jargon/{id}`
- GET `/emoji/list?limit&offset` → {items:[{hash,description,full_path,query_count,is_registered,is_banned,no_file,vlm_processed,register_time}]}
- GET `/emoji/{hash}` → {emoji:{...}}；DELETE `/emoji/{hash}`；PUT `/emoji/{hash}/ban` {banned:boolean}
- 注意：表情包没有图片预览端点，full_path 是服务器本地路径 — 列表页只展示文本信息（描述/状态/次数），不要尝试加载图片。

### 日志/记忆/MCP/数据传输/推理（api/misc.ts）
- GET `/logs/recent?limit&level` → {logs:[{id,timestamp,level,module,message}],total}
- GET `/logs/level` → {level}；PUT `/logs/level` {level:"DEBUG"|"INFO"|"WARN"|"ERROR"}
- WS：`/api/webui/ws` 统一通道（domain=`logs` topic=`main`，事件 `entry`/`snapshot`；`maisaka_monitor` 域事件 `planner.progress`/`message.ingested`/`message.sent`/`llm.retry`/`llm.error` 等）。ws.ts 已封装自动重连+订阅恢复。
- POST `/memory/search` {query,chat_id,top_k} → {hits:[...]}
- GET `/memory/profiles` → {profiles:[...]}；GET `/memory/profiles/{person_id}`
- GET `/mcp/status` → {enabled,initialized,server_count,tool_count,servers:[...]}
- POST `/data-transfer/export` {include_bot,include_model} → JSON 附件（用 `exportConfig()` 封装）
- POST `/data-transfer/import`（payload=导出文件 JSON，用 `importConfig(file)` 封装）
- GET `/reasoning-process/snapshots?session_id&limit` → {snapshots:[{seq,kind,session_id,time,payload}]}

## 通用要求

1. TypeScript strict 全绿：`npx tsc -p tsconfig.app.json --noEmit`（在 web/dashboard 下）必须 0 错误。
2. `npx eslint src/pages/<你的文件>` 0 error。
3. 所有列表页：Loading 态（`<Loading />`）、空态（`<Empty text="..." />`）、错误态（`<Banner kind="err">`）三态齐全；删除/危险操作用 `ConfirmModal`；操作结果用 `toast.ok/err` 反馈。
4. 分页用 `Pagination`（后端 limit/offset）。
5. 注释风格：中文，说明「对齐哪个后端路由」。不写 i18n，文案直接中文。
6. 时间显示用 `formatTime/formatDateTime/formatRelative`；数字用 `formatNumber/compactNumber`。
7. 危险按钮 `btn-danger`；主操作 `btn`；次操作 `btn-secondary`；轻操作 `btn-ghost`。
8. 不要引入任何新依赖。不要用 alert/confirm。不要写行内 onChange 未受控 input。
9. 性能：列表默认 limit=100；轮询间隔 ≥5s 且页面不可见时暂停（useQuery refetchIntervalInBackground=false 默认即可）；事件订阅在 useEffect cleanup 中退订。
10. 安全：不渲染后端返回的原始 HTML；用户内容直接作为文本节点输出（React 默认转义）。
