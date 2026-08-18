# P4-3 执行计划

## 前置检查

- [x] `pnpm -C web install` 能访问 npm registry（本机 Node v22.21.1 / pnpm 10.23.0）。
- [x] 用临时 `ark-hub` 运行环境完成真实数据联调与验收（临时凭证、状态库与示例清单，
      验收后已清理）。
- [x] Tailwind 采用 v4（`@tailwindcss/vite`，CSS-first 配置）。
      **TypeScript 退到 6.0.3**：`typescript-eslint` 8.67.0 的 peer 上界是 `<6.1.0`，
      与 registry 上的 TS 7.0.2 不兼容。

## 实现顺序

顺序的原则是**先让后端能承载前端，再写前端**，避免前端写完才发现 CSP 或路由拦着。

### 第 1 步：前端工程骨架

- [x] `web/` 初始化：`package.json`、`pnpm-lock.yaml`、`vite.config.ts`、
      `tsconfig.json`、`eslint.config.js`、`index.html`、`src/main.ts`、`src/App.vue`。
- [x] `vite.config.ts`：`build.outDir = 'dist'`（即 `web/dist`）、`emptyOutDir: true`；
      dev proxy 把 `/api`、`/login`、`/logout`、`/healthz` 转发到 `127.0.0.1:8080`。
      产物由 `make web-build` 同步到嵌入目录，见 design.md §4.1。
- [x] 产物文件名带内容 hash，全部落在 `assets/` 下；确认没有内联 `<script>`、
      没有 CDN 引用、没有 `eval`（CSP 兼容前提）。
- [x] 占位产物：提交 `internal/hub/webui/dist/PLACEHOLDER` 保证目录非空，
      包目录下另提交 `placeholder.html` 作为回落页；`.gitignore` 忽略 dist 其它文件。

### 第 2 步：`internal/hub/webui/` 嵌入包

- [x] `webui.go`：`//go:embed dist`，暴露 `NewHandler()`、`Assets()`、
      `IndexHTML()` 与 `Built()`。
- [x] 静态资源设 `Cache-Control: public, max-age=31536000, immutable`；
      `index.html` 保持 `no-store`。
- [x] 路径穿越防护：只服务 `dist` 子树，拒绝 `..`；未命中回落入口 HTML。
- [x] `webui_test.go`：占位回落、Content-Type 正确、缓存头、穿越路径被拒。

### 第 3 步：hub 路由与安全头接入

- [x] `internal/hub/http.go`：`GET /` 改为返回嵌入 `index.html`（保持鉴权）。
- [x] 静态资源不单独枚举路由，由 catch-all 鉴权后分流统一提供（见 design.md §8）。
- [x] `handleProtectedNotFound` 改为鉴权后分流：`/api/` 前缀 404 JSON，其余交给 webui。
      **未认证行为保持不变**（HTML 重定向、`/api/` 401 JSON）。
- [x] `securityHeaders` 更新 CSP 为 design.md §2.2 的新值。
- [x] 登录页保持服务端渲染 + 内联样式，只同步配色；不新增 `/login.css`（design.md §8）。
- [x] 删除 `shellPageTemplate` 及其模板装配（`newApplication` 里的 `shellTemplate`）。
- [x] 修复 `renderLogin` 的登录 CSRF Cookie 语义：复用合法 token 值并每次续期
      （真机验证中发现的既有缺陷，见 design.md §9）。

### 第 4 步：摘要 DTO 补大小字段

- [x] `internal/hub/query.go`：`hostSummaryResponse` 增 `last_backup_bytes`、
      `recent_backup_sizes`，新增 `backupSizePoint`。
- [x] `internal/hub/health.go`：在已有的 `hostRuns` 上聚合，只取 `ok|warn` 的 run，
      按时间正序最多 14 点，每点为该 run 全部 target 字节之和。
- [x] 边界：无成功 run → `last_backup_bytes` 为 `null`、`recent_backup_sizes` 为空数组
      （不是 `null`，与现有 `diagnostics` 的空数组风格一致）。

### 第 5 步：前端 API 客户端与 store

- [x] `src/api/types.ts`：与 Go DTO 一一对应，包括四类 operation result 的字段白名单
      （`internal/hub/operation.go:516-536`）。
- [x] `src/api/client.ts`：统一 `X-CSRF-Token`、`Content-Type: application/json`、
      401 跳登录、`{error:{code,message}}` 解析与错误码映射。
- [x] `stores/session.ts`、`stores/hosts.ts`、`stores/alerts.ts`。
- [x] `stores/operations.ts`：轮询状态机（2s ×10 → 5s，上限 30 分钟）；
      **每次响应即时捕获 `confirmation_token`**；10 分钟倒计时；409/422 错误处理。

### 第 6 步：四个页面

- [x] `OverviewView.vue` + `HealthBadge` / `Sparkline` / `StatusPill` 组件。
- [x] `HostDetailView.vue`：摘要、target、备份历史（可展开 target 明细）、doctor、演练、操作区。
- [x] `AlertsView.vue`：当前告警（标注为实时投影，非历史流水）。
- [x] `OperationsView.vue`：keyset 分页列表 + 按 kind 结构化的结果详情。
- [x] 恢复交互：预检表单 → 预检结果（steps / manual_checks / conflicts / destructive / resume）
      → force 模式输入主机名解锁 + 红色二次确认 → execute → 结果与人工确认清单。

### 第 7 步：构建接入

- [x] Makefile 增加 `web-install`、`web-build`、`web-lint`、`web-typecheck`、
      `web-test`、`web-check`、`hub` 目标。
- [x] 确认 `make check` 仍然不依赖 node。
- [x] `make hub` 产出的二进制里是真实界面而不是占位页。

### 第 8 步：规范与文档

- [x] `hub-guidelines.md`：路由表、CSP、缓存、静态资源鉴权、DTO 新字段、测试要求。
- [x] 新增 `.trellis/spec/frontend/web-guidelines.md` 与 `.trellis/spec/frontend/index.md`。
- [x] `directory-structure.md` 登记 `internal/hub/webui/` 与 `web/`。
- [x] `docs/roadmap.md`：P4-2 改为已完成；P4-3/P4-4 合并并记录理由。
- [x] `docs/design.md` §9、`README.md` 状态表与 `make hub` 用法。

## 验证命令

后端（每次后端改动后）：

```bash
go test ./internal/hub ./internal/hub/webui ./internal/systemd -race -count=10
make check
go build -trimpath -o bin/ark-hub ./cmd/ark-hub
CGO_ENABLED=0 go build -trimpath -o bin/ark-hub-nocgo ./cmd/ark-hub
go mod verify
git diff --check
```

前端：

```bash
pnpm -C web install --frozen-lockfile
pnpm -C web run lint
pnpm -C web run typecheck    # vue-tsc --noEmit
pnpm -C web run test         # vitest run
make hub
```

后端测试必须新增覆盖：

- `/assets/*` 未登录被拒、登录后可取、Content-Type 与 `immutable` 缓存头；
- SPA fallback：登录后未知非 API 路径返回 `index.html`；`/api/` 未知路径仍 404 JSON；
  **未认证时 fallback 不得泄露 index.html**；
- 新 CSP 字符串精确断言（防止后续被无意收紧或放宽）；
- 摘要新字段：无成功 run、只有失败 run、超过 14 次成功 run、warn 状态计入、
  多 target 字节求和。

前端单测覆盖：

- API 客户端的错误码映射与 401 跳转；
- 恢复 token 状态机：**首次 ok 响应捕获 token**、倒计时过期禁用执行、
  409 冲突提示、轮询退避与上限；
- 字节/时长/时间格式化；
- sparkline 在 0 点、1 点、14 点下的渲染不崩。

## 真实环境验收

在有数据的 hub 上：

1. `make hub` 后替换二进制并重启 `ark-hub.service`。
2. 浏览器打开监听地址：未登录跳转登录页 → 登录后进入总览。
3. 总览能看到全部主机卡片、健康度、最近备份大小与趋势、下次计划时间。
4. 主机详情能看到 target、备份历史、doctor 报告、演练记录。
5. 从页面发起一次 backup，能看到 running → 终态，结果可查。
6. 发起一次 `isolate` 恢复预检，确认 token 正常下发、执行成功、生产资源无变化。
7. 刷新页面后预检确认失效，提示重新预检（验证 token 一次性语义）。
8. `systemctl stop ark-hub` 后 `systemctl list-timers` 里的备份/演练 timer 不受影响。
9. 浏览器控制台无 CSP 拦截报错。

## 风险文件与回滚点

| 文件 | 风险 | 说明 |
|---|---|---|
| `internal/hub/http.go` | **高** | CSP 与鉴权分流在同一处；改错会让未认证者拿到 index.html 或让 CSP 失效 |
| `internal/hub/health.go` | 中 | 摘要聚合与告警投影共用同一函数，改错会污染健康判定 |
| `internal/hub/webui/webui.go` | 中 | 路径穿越与缓存头 |
| `Makefile` | 低 | 构建顺序错会把占位产物打进二进制 |

回滚点：第 3 步（路由与 CSP）与第 4 步（DTO）各自独立可回退；
前端各页面之间无耦合，可逐页回退。整体回滚就是换回旧 `ark-hub` 二进制。

## 已知取舍（实现时不要"顺手修正"）

- `style-src` 保留 `'unsafe-inline'`：理由见 design.md §2.2，不是遗漏。
- 登录仍走后端表单而不是 SPA 内的 JSON 登录：刻意不动已严格验收的鉴权契约。
- 不做全局 `/runs` 页面：YAGNI，客户端方法保留。
- 占位 `dist/PLACEHOLDER` 必须提交：否则 `go:embed dist` 让 `go build ./...` 直接编译失败。
  Vite 也不能直接输出到该目录，`emptyOutDir` 会把它删掉。
