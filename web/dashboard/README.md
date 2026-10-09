# oh-my-qqbot Dashboard

> oh-my-qqbot 的 WebUI 前端 — 基于 React 19 + TypeScript + Vite + TanStack Router/Query + Tailwind 4

<div align="center">

[![React](https://img.shields.io/badge/React-19.2-61DAFB?logo=react&logoColor=white)](https://react.dev/)
[![TypeScript](https://img.shields.io/badge/TypeScript-5.9-3178C6?logo=typescript&logoColor=white)](https://www.typescriptlang.org/)
[![Vite](https://img.shields.io/badge/Vite-7.2-646CFF?logo=vite&logoColor=white)](https://vitejs.dev/)
[![TailwindCSS](https://img.shields.io/badge/TailwindCSS-4.2-38B2AC?logo=tailwind-css&logoColor=white)](https://tailwindcss.com/)

</div>

## 来源与许可声明

本项目 **fork 自 [MaiBot](https://github.com/MaiM-with-u/MaiBot) 的 `dashboard/` 前端**
（原项目 v1.8.4），并做了如下改造以对接 oh-my-qqbot 的 Go 后端（`internal/webui`）：

- **鉴权方式**：HttpOnly Cookie → `Authorization: Bearer` 请求头 + `/api/webui/ws-token`
  换取 WS 握手临时 token；
- **API 前缀**：统一对齐 Go 后端 `/api/webui` 路由清单（`internal/webui/routers/registry.go`），
  字段差异逐个适配；
- **移除插件系统**：插件市场/插件配置/插件镜像/插件 WebUI 扩展页与相关 API 客户端全部删除；
- **移除 Electron 打包分支**：electron/ 目录、electron-vite 配置、主进程桥接代码；
- **i18n**：zh-CN 为主（fallback zh），保留 en。

原项目代码采用 **GNU Affero General Public License v3（AGPLv3）** 授权，
本目录保留其 [`LICENSE`](./LICENSE) 文件与全部版权声明；
按 AGPLv3 要求，本衍生作品同样以 AGPLv3 授权分发。

## 开发

```bash
npm install        # 安装依赖
npm run dev        # 开发服务器（127.0.0.1:7999，/api 代理到 http://127.0.0.1:8001）
npm run build      # 类型检查 + 生产构建（tsc -b && vite build）
npm run typecheck  # 仅类型检查
```

生产构建产物输出到 `dist/`，由 Go 后端（`internal/webui` 的静态托管 + SPA fallback）
直接服务；`node_modules/` 与 `dist/` 不入库（根 `.gitignore` 已配置）。

## 功能模块（保留页面）

- **首页概览** — 运行状态、存储占用、快捷操作、公告卡片
- **聊天** — WebUI 内置聊天（虚拟平台注入）+ 嵌入式聊天
- **聊天管理** — 会话列表浏览
- **监控** — 麦麦观察（maisaka monitor / WS `maisaka_monitor` 域）
- **资源** — 表情包 / 表达方式 / 黑话 / 行为学习 / 长期记忆（知识库 + 图谱）
- **人物管理**
- **日志** — 终端风格日志（WS `logs` 域实时流）、推理过程、详细统计
- **模型配置 / 机器人配置 / Prompt 管理** — 配置中心
- **数据传输** — 配置导出导入
- **回复效果**（实验开关控制）
- **登录 / 初始化向导 / 设置**

### 已移除（本 fork 范围裁定）

- 插件系统全部页面（plugin-config / plugins / plugin-mirrors / plugin-webui / 适配器管理）
- Electron 桌面壳与后端连接管理器
- 依赖远端插件统计服务的问卷 / 模型模板市场入口
