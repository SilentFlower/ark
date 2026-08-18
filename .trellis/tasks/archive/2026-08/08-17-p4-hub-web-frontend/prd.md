# P4-3 ark-hub 前端 web/（合并 P4-4 打包接入）

## Goal

在 `web/` 下用 Vue 3 + Vite + TypeScript + Pinia + Tailwind 实现 ark-hub 控制台，
并把构建产物 `go:embed` 进 `ark-hub` 单二进制。交付形态是：**替换二进制并重启
`ark-hub.service` 之后，浏览器直接打开真实控制台**，部署环境不需要 node。

管理员由此获得：一眼看出所有机器的备份健康度，并能从页面发起备份、演练和恢复。

对应 `docs/roadmap.md:581-592`（P4-3 + P4-4）与 `docs/design.md` §9。

## Background

### 后端已交付的契约（P4-1 / P4-2，均已归档验收）

路由（`internal/hub/http.go:145-164`）：`GET /healthz`（公开）、
`GET|POST /login`（公开，表单 + 登录 CSRF Cookie + 限流，成功后 303 到 `/`）、
`GET /`（需登录，当前是 Go 占位模板 `internal/hub/http.go:49-66`）、
`POST /logout`（表单 + session CSRF）、`GET /api/session`、
`GET /api/hosts`、`GET /api/hosts/{host}`、`GET /api/runs`、`GET /api/alerts`、
`GET /api/operations`、`GET /api/operations/{id}`、
`POST /api/hosts/{host}/{backup|verify|restore}`（需 CSRF，异步 202）。

DTO 事实（`internal/hub/query.go`、`internal/hub/health.go`）：

- `hostSummaryResponse` 含 `host / local / project / target_count / schedule /
  last_backup_status / last_successful_backup_at / last_verification_status /
  next_run_at / diagnostics / health`，**不含任何字节数字段**。
- `hostDetailResponse` = `summary + targets[] + runs[] + doctor + verifications[]`；
  `runs[]` 来自 `ListHostRuns(host, 100)`（`internal/hub/health.go:53`），
  含 `targets[].bytes / duration_ms / snapshot_id`。
- `health ∈ {ok, warn, fail, unknown}`；`diagnostics` 目前只有 `schedule_unavailable`。
- `alertResponse` 是**实时投影而非历史流水**（`internal/hub/health.go:136-162`），
  `severity` 恒为 `fail`，`created_at` 是投影时刻；稳定 kind 为
  `backup_overdue`、`backup_consecutive_failures`、`verification_failed`。
- 列表统一 `{items, next_cursor}`，`limit` 1–100 默认 50（`internal/hub/api.go:293-302`）；
  错误统一 `{"error":{"code","message"}}`（`internal/hub/api.go:28-35`）。
- operation result 按 kind 有字段白名单（`internal/hub/operation.go:516-536`）：
  backup `run_id/status/manifest/manifest_snapshot_id/error`；
  verify `manifest_snapshot_id/status/results/error`；
  restore_preview `plan/force/resume/destructive/conflicts/digest`；
  restore `manifest_snapshot_id/run_id/source_host/destination_host/status/steps/manual_checks/error/isolation`。
- 冲突项结构见 `internal/restore/execute.go:55-62`：`resource / detail / force_allowed`。

关键行为约束：

- **恢复确认 token 只在预检 operation 首次变为 `ok` 的那一次 GET 下发**，
  `issued` 置位后永不重发（`internal/hub/operation.go:284-313`）；绑定当前 session、
  10 分钟有效、只能消费一次。
- 同一 Hub 进程只允许一个活动手工操作，冲突返回 `409`（`internal/hub/api_action.go:199-201`）。
- 当前 CSP 为 `default-src 'none'; style-src 'unsafe-inline'; ...`
  （`internal/hub/http.go:447`），**禁止一切脚本**；没有静态资源路由；
  `/` 之外的路径鉴权后一律 404（`internal/hub/http.go:162`）。

### 仓库现状

- 尚无 `web/` 目录与任何前端工具链；`.trellis/spec/` 只有 `backend/` 与 `guides/`。
- `Makefile` 只构建 `./cmd/ark`。本机 Node v22.21.1、pnpm 10.23.0 可用。
- `.trellis/spec/backend/directory-structure.md:61` 已登记 `web/` 属于 P4-3 / P4-4。

### 已确认的范围决策

- **合并 P4-4**：本任务同时完成前端、`go:embed`、CSP 放宽、静态资源路由、
  SPA history fallback 与 `make hub`，一次做到单二进制可打开。
  排除了「临时 `--web-dir` 从磁盘 serve」的中间方案——那是 P4-4 必然删除的一次性代码。
- **恢复模式**：UI 暴露 `isolate`（默认）、`normal`、`force` 三种；
  `force` 必须先看完冲突清单、手动输入目标主机名解锁、再经红色二次确认弹窗。

## Requirements

### R1 前端工程

- R1.1 `web/` 使用 Vue 3 + Vite + TypeScript + Pinia + Tailwind，pnpm 管理依赖。
- R1.2 产物无内联 `<script>`、无 CDN 引用、无 `eval`，可在 `script-src 'self'` 下运行。
- R1.3 不引入图表库，大小趋势用手写 SVG sparkline。
- R1.4 不做深色模式与 i18n（单管理员、中文单语）。

### R2 页面

- R2.1 **总览**：每台机器一张卡片——health 徽章、最近备份状态与时间、最近备份大小 +
  最多 14 点趋势、下次计划时间、target 数、`local` 标记、`diagnostics`。
  `schedule_unavailable` 时显示「计划不可解析」，不伪造时间。
- R2.2 **主机详情**：摘要、target 清单、备份历史（可展开 target 明细：状态/字节/耗时/
  snapshot id/错误）、最新 doctor 报告、演练记录、操作区。
- R2.3 **告警**：当前告警列表，页面标注其为实时投影而非历史流水。
- R2.4 **操作**：keyset 分页列表 + 单条详情，按 kind 结构化展示 result。
- R2.5 不做全局 `/runs` 页面（YAGNI），但保留客户端方法。

### R3 会话与操作

- R3.1 启动读 `GET /api/session` 取 `username` 与 `csrf_token`；任何 `401` 跳 `/login`。
- R3.2 登录与退出仍走后端现有表单路径，**不新增 JSON 登录端点、不改鉴权契约**。
- R3.3 所有业务 POST 带 `X-CSRF-Token` 与 `Content-Type: application/json`。
- R3.4 操作轮询：2s ×10 次后退避到 5s，总时长上限 30 分钟。
- R3.5 **每次轮询响应都即时捕获 `confirmation_token`**，存内存并显示 10 分钟倒计时；
  UI 明确提示刷新页面会作废本次确认。
- R3.6 错误码映射：`409 conflict` → 已有手工任务在运行（给出跳转入口）；
  `confirmation_required` / `confirmation_expired` → 引导重新预检；
  `503` → 服务暂不可用（可能是清单损坏）。

### R4 恢复交互

- R4.1 预检表单：source（只读）、destination（下拉）、snapshot（默认 `latest`）、
  mode（默认 `isolate`）。
- R4.2 预检结果展示 `plan.steps`、`plan.manual_checks`、`conflicts[]`、
  `destructive`、`resume`。
- R4.3 `force` 三重闸门：完整冲突清单 → 输入目标主机名精确匹配 → 红色二次确认弹窗
  明写「将覆盖 `<host>` 上的生产数据」。
- R4.4 恢复完成后把 `manual_checks` 作为一等公民展示，不折叠进结果 JSON。

### R5 后端接入

- R5.1 新增 `internal/hub/webui/`：只负责 `go:embed` 前端产物并提供 handler，
  不依赖 `store` / `config`，不含业务逻辑。
- R5.2 `GET /` 返回嵌入 `index.html`；静态资源（`/assets/*`、`/favicon.svg`）
  由 catch-all 统一分流提供，同样要求登录。
  ~~新增公开的 `GET /login.css`~~ —— 实现期取消，见 design.md §8：
  `style-src` 既然要为 Vue 保留 `'unsafe-inline'`，这个公开端点就不再有技术理由，
  登录页保持服务端渲染 + 内联样式。
- R5.3 catch-all 改为鉴权后分流：`/api/` 前缀返回 404 JSON，其余交给 webui
  （命中嵌入文件返回该文件，否则返回 `index.html`）；
  **未认证行为完全不变**（HTML 重定向登录、`/api/` 返回 401 JSON）。
- R5.4 CSP 更新为 `default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline';
  img-src 'self' data:; font-src 'self'; connect-src 'self'; form-action 'self';
  base-uri 'none'; frame-ancestors 'none'`，并由测试精确断言整串。
- R5.5 带 hash 的静态资源 `Cache-Control: public, max-age=31536000, immutable`；
  `index.html` 与所有 API 保持 `no-store`。
- R5.6 静态资源禁止路径穿越，只服务 `dist` 子树。

### R6 摘要 DTO 扩展

- R6.1 `hostSummaryResponse` 增 `last_backup_bytes`（`*int64`）与
  `recent_backup_sizes`（`{run_id, finished_at, bytes}[]`）。
- R6.2 只统计 `status ∈ {ok, warn}` 的 run；每点为该 run 全部 target 字节之和；
  按时间正序最多 14 点。
- R6.3 无成功 run 时 `last_backup_bytes` 为 `null`、`recent_backup_sizes` 为空数组。
- R6.4 只增字段，不改任何现有字段。

### R7 构建

- R7.1 Vite `build.outDir` 为 `web/dist`，由 `make web-build` 清理并同步到
  `internal/hub/webui/dist`。**不能让 Vite 直接输出到嵌入目录**：`emptyOutDir`
  会连保证 `go:embed` 可编译的 `PLACEHOLDER` 一起删掉。
- R7.2 提交 `internal/hub/webui/dist/PLACEHOLDER` 保证目录非空（`go:embed dist` 的前提），
  gitignore 该目录其它文件；真实产物缺失时回落到包目录下的 `placeholder.html`
  并提示 `make hub`，**不阻止服务启动**。保证 `go build ./...` 与 `go test ./...`
  在无 node 环境下照常工作。
- R7.3 Makefile 增加 `web-install` / `web-build` / `web-lint` / `web-typecheck` /
  `web-test` / `web-check` / `hub`。
- R7.4 `make check` 保持纯 Go，不依赖 node。

### R8 规范与文档同步

- R8.1 `hub-guidelines.md` 更新路由分流、CSP、缓存、静态资源鉴权、DTO 新字段、
  占位产物与测试要求。
- R8.2 新增 `.trellis/spec/frontend/`（`index.md` + `web-guidelines.md`）。
- R8.3 `directory-structure.md` 登记 `internal/hub/webui/` 与 `web/`，补依赖方向。
- R8.4 `docs/roadmap.md`：P4-2 改为已完成，P4-3 与原 P4-4 合并并记录理由，
  原 P4-5 顺延为 P4-4。
- R8.5 `docs/design.md` §9 与 `README.md` 同步前端接入形态与 `make hub` 用法。

## Acceptance Criteria

### 自动化验证

- [x] AC1（R5、R6）`go test ./internal/hub ./internal/hub/webui ./internal/systemd -race -count=10`
      通过，且新增覆盖：`/assets/*` 未登录被拒 / 登录后可取 / Content-Type 与 `immutable` 缓存头；
      登录后未知非 API 路径返回 `index.html`，`/api/` 未知路径仍 404 JSON，
      **未认证时 fallback 不泄露 index.html**；新 CSP 字符串精确断言；
      登录 CSRF Cookie 复用同值并续期。
- [x] AC2（R6）摘要新字段覆盖：无成功 run、只有失败 run、超过 14 次成功 run、
      warn 计入、多 target 字节求和。
- [x] AC3（R7）`make check` 全绿且不依赖 node；`go build`、`CGO_ENABLED=0 go build`
      在未构建前端时仍然成功。
- [x] AC4（R1、R3）`pnpm -C web run lint`、`typecheck`、`test` 全绿；前端单测覆盖
      API 错误码映射与 401 跳转、**首次 ok 响应捕获 token**、倒计时过期禁用执行、
      409 冲突提示、轮询退避与上限、格式化函数、sparkline 在 0/1/14 点下不崩。
- [x] AC5（R7）`make hub` 产出的二进制里是真实界面而非占位页。

### 真实环境验收（在有数据的 hub 上）

- [x] AC6（R2）未登录跳转登录页；登录后总览显示全部主机卡片、健康度、
      最近备份大小与趋势、下次计划时间。
- [x] AC7（R2.2）主机详情能看到 target、备份历史、doctor 报告、演练记录。
- [x] AC8（R3）从页面发起一次 backup，能看到 running → 终态，结果可查。
- [x] AC9（R4）发起 `isolate` 恢复预检并验证 operation 生命周期与结果展示；
      执行成功态未在本轮跑通（临时环境的示例清单指向不存在的 restic 仓库），
      恢复执行后端逻辑本轮未改动，由 P3 阶段实测覆盖。
- [x] AC10（R3.5）刷新页面后预检确认失效并提示重新预检。
- [x] AC11（R5.4）浏览器控制台无 CSP 拦截报错。
- [x] AC12 `systemctl stop ark-hub` 后备份与演练 timer 不受影响（ADR-005 未被破坏）。

## Out of Scope

- P4-5 的钉钉告警、告警静默期与外部心跳死人开关。
- 前端侧的告警历史流水（后端 `/api/alerts` 本身就是实时投影，不存历史）。
- 全局跨机 `/runs` 时间线页面。
- 多用户、角色、OIDC、二次验证（ADR 已定首版只有单本地管理员）。
- 深色模式、i18n、移动端专门适配。
