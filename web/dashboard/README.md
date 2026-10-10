# oh-my-qqbot Dashboard

> oh-my-qqbot 的 WebUI 前端 — 为 Go 后端（`internal/webui`）全新设计与实现。

## 技术栈

- **React 19** + **TypeScript 5.9**（strict）
- **Vite 7** 构建；`@tanstack/react-router` 路由；`@tanstack/react-query` 数据层
- **lucide-react** 图标
- **零 UI 框架**：设计系统为手写 CSS（`src/index.css`），黑白灰线条风
  （亮色 = 纸白底 + 墨黑字；暗色 = 墨黑底 + 纸白字；语义色仅 ok/warn/err 小面积使用）
- **vitest** 单测（`npm test`）

## 目录结构

```
src/
├── api/          # 后端 API 封装（与 internal/webui/routers 一一对应）
│   ├── auth.ts       # 登录/密码/向导（auth.go）
│   ├── system.ts     # 系统状态/统计/资讯（system.go, statistics.go）
│   ├── config.ts     # bot/model 配置读写（config.go）
│   ├── models.ts     # 模型/服务商/任务路由（model.go）
│   ├── chat.ts       # WebUI 虚拟平台聊天（chat.go）
│   ├── resources.ts  # 人物/表达/黑话/表情（resources.go）
│   └── misc.ts       # 日志/记忆/MCP/数据传输/推理快照
├── lib/          # 基础设施
│   ├── http.ts       # fetch 客户端（Bearer + 401 处理 + success 包络校验）
│   ├── token.ts      # token 存取 + 401 广播
│   ├── ws.ts         # 统一 WebSocket（自动重连 + 订阅恢复）
│   ├── format.ts     # 纯函数格式化（时间/字节/数字）
│   ├── theme.ts      # 亮/暗/跟随系统主题
│   └── toast.ts      # 通知 store
├── components/   # Layout（侧栏+顶栏）、ui.tsx（Modal/Menu/字段组件等原语）、ErrorBoundary
├── pages/        # 每个路由一个文件（与 router.tsx 一一对应）
└── index.css     # 设计系统（token + 组件类）
```

## 设计原则

1. **只调后端真实存在的 API** — 前端不臆造端点；每个 api/*.ts 文件头部标注对齐的 Go 路由文件。
2. **简约至上** — 无 UI 框架、无动画库、无图表库（图表用纯 div 条形图）；层次靠 1px 边框与留白。
3. **黑白灰线条风** — 禁止彩色装饰、渐变、玻璃拟态、大阴影；语义色仅状态点/徽章/提示条。
4. **中文注释**，文案直接中文（无 i18n 层）。

## 开发

```bash
npm install
npm run dev        # http://127.0.0.1:7999（代理 /api → 127.0.0.1:8001）
npm test           # vitest 单测
npm run typecheck  # tsc 双工程检查
npm run lint       # eslint
npm run build      # 产出 dist/，由 Go 后端静态托管
```

## 与后端的鉴权约定

- 登录 = `POST /api/webui/auth/verify`（body `{token}`，默认密码 `admin123`），
  成功后 token 存 localStorage（key `ohmyqqbot-webui-token`）；
- 后续 REST 请求经 `Authorization: Bearer <token>` 头鉴权；401 统一清 token 跳 `/auth`；
- WebSocket 握手经 `GET /api/webui/ws-token` 换取一次性临时 token（60s 有效）。

## 许可

GPLv3（与主仓库一致）。本前端为全新实现，不再包含 MaiBot dashboard 的代码；
设计风格参考 Buddy-2API-Go 管理后台（仅视觉思路，未复制代码）。
