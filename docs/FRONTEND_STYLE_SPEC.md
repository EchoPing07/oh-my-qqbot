# WebUI 前端设计规范 — 黑白灰 + 辅红 + 线条风

> 本文档是 web/dashboard 前端视觉重写的唯一规范。设计基座(tokens/index.css/Layout/auth/Logo)已重写完成并提交；
> 页面层代码在 `web/dashboard/src/routes/**`， fork 自 MaiBot dashboard，本规范用于约束页面层的清理与合规。

## 1. 设计语言

- **主色**: 黑白灰。亮色模式 = 纸白背景 `0 0% 97%` + 墨黑文字 `0 0% 10%` + 灰阶边框 `0 0% 87%`；暗色模式 = 墨黑背景 `0 0% 7%` + 纸白文字 `0 0% 92%`。
- **辅色**: 红。`--primary`（亮 `0 72% 46%`，暗 `0 70% 56%`），**只用于**：主操作按钮、激活态指示、关键数据强调、链接。禁止大面积红底色块。
- **线条风**: 层次由 1px 边框、细分隔线与留白承担。圆角小（2/4/6/8px），阴影极弱或无。
- **禁止**: 渐变彩色、玻璃拟态、大阴影、彩虹/spectrum 色带、米黄纸张色、齿轮/像素/键帽等装饰元素。

## 2. Token 使用规则（强制）

- 所有颜色一律走语义 token：`text-foreground` / `text-muted-foreground` / `bg-background` / `bg-card` / `bg-muted` / `border-border` / `text-primary` / `bg-primary` / `text-destructive` / `bg-destructive`。
- **严禁**以下写法（发现即改）：
  - `text-amber-*`、`text-sky-*`、`text-emerald-*`、`bg-orange-*` 等原始调色板色（信息提示用 `text-muted-foreground`，成功/进行中语义可用 `text-primary` 或保留 `text-emerald-*` 仅限状态点，按钮/徽章一律灰阶或红）
  - `bg-gradient-to-*`、`bg-[radial-gradient...]`（背景装饰一律删除或改灰阶网格线）
  - `backdrop-blur-*`（除 Header 已有的 `backdrop-blur-md`）
  - `shadow-2xl` / `shadow-xl`（用 `shadow-sm` 或无边框投影）
- 图表色：recharts 颜色引用 `var(--color-xxx)`（chart-1..5 已是灰阶+红序列），不要硬编码 hex。

## 3. MaiBot 残留清理（强制）

页面文件内（仅限你负责的文件集）：

1. **文案**: 硬编码中文里的「麦麦」「MaiBot」「MAIBOT」→「机器人」或中性表述（「主程序」「控制台」）。i18n key 的 value 已在 zh.json 清理过，页面内 t('...') 不用动 key。
2. **data 属性分支**: `themeConfig.dashboardStyle === 'future-retro'` / `=== 'millennium'` 的条件分支全部删除（dashboardStyle 类型现在只有 'modern'，这类比较会直接 TS 报错——遇到就删，保留非条件分支的那一侧）。
3. **主题装饰**: `data-home-version-spectrum`（彩虹线条 SVG）、`data-dashboard-logo-spectrum`、`maibot-activity-orbit`、`MaiRetroText/MaiRetroQuote` 字体引用、米黄/齿轮类样式 → 删除或换灰阶。
4. **retro/millennium CSS 引用**: `:root[data-dashboard-style='future-retro']` 等选择器引用不可能再命中，相关 className 逻辑简化。

## 4. 工作纪律

- **允许修改**: 你名下的 routes 文件集 + 这些文件引用的子组件（若有）。
- **禁止修改**: `src/components/ui/**`、`src/components/layout/**`、`src/lib/**`、`src/index.css`、`src/i18n/**`、`src/router.tsx`、`src/main.tsx`、其他代理名下的 routes 文件。发现问题记录到最终报告。
- **每完成一批文件**: 运行 `npx tsc -p tsconfig.app.json --noEmit` 确认 0 错误（在 `web/dashboard` 目录下）。**你收工时必须保证全量 tsc 0 错误**——其他代理在并行改别的文件，若报错来自他人文件区则忽略并在报告里注明。
- **不改业务逻辑**: 只做视觉/文案/死分支清理。功能代码、API 调用、状态管理、事件处理一律不动。删除死分支后保持语义等价。
- 注释风格保持中文，与现有代码一致。

## 5. 验收

- `npx tsc -p tsconfig.app.json --noEmit` → 你名下文件 0 错误
- `npx eslint <你的文件>` → 0 error（warning 可留）
- `grep -rn "麦麦\|MaiBot\|MAIBOT\|future-retro\|millennium" <你的文件>` → 0 命中（i18n key 名除外）
