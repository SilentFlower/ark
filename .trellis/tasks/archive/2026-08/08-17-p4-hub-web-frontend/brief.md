# Brief — P4-3 ark-hub 前端 web/（合并 P4-4 打包接入）

## Goal

- 用 Vue 3 + Vite + TS + Pinia + Tailwind 写出 ark-hub 控制台，并把产物 `go:embed` 进
  `ark-hub` 单二进制——替换二进制重启服务后，浏览器直接打开真实控制台，部署端不需要 node。

## Scope

- `web/` 前端工程：总览、主机详情、告警、操作四块页面，恢复三段式交互（预检 → 确认 → 执行）。
- `internal/hub/webui/` 新包：只做 `go:embed` 前端产物 + 静态资源 handler。
- hub 路由接入：`GET /` 返回嵌入 index.html；静态资源由 catch-all 鉴权后分流统一提供
  （`/api/` 仍 404 JSON，其余 SPA fallback）。
- CSP 从「禁止一切脚本」放宽为 `script-src 'self'` 等一组同源策略；带 hash 的静态资源
  改 `immutable` 缓存，`index.html` 与 API 保持 `no-store`。
- `hostSummaryResponse` 增 `last_backup_bytes` 与 `recent_backup_sizes`（最多 14 点），
  复用 `projectHosts` 已取的 hostRuns，不新增查询。
- Makefile 增 `web-install` / `web-check` / `web-build` / `hub`；`make check` 保持纯 Go。
- 同步 `hub-guidelines.md`、新增 `.trellis/spec/frontend/`、更新 `directory-structure.md`、
  `docs/roadmap.md`、`docs/design.md` §9、`README.md`。

## Non-Goals

- P4-5 的钉钉告警、告警静默期、外部心跳死人开关。
- 告警历史流水（后端 `/api/alerts` 本身就是实时投影，不存历史）。
- 全局跨机 `/runs` 时间线页面（客户端方法保留，不做页面）。
- 多用户、角色、OIDC、二次验证。
- 深色模式、i18n、移动端专门适配。

## Key Decisions

- **合并 roadmap 的 P4-3 与 P4-4**，一次做到单二进制可打开。排除了「临时 `--web-dir`
  从磁盘 serve」的中间方案——那是 P4-4 必然要删的一次性代码。
- **恢复模式三种全暴露**：`isolate`（默认）、`normal`、`force`。`force` 三重闸门：
  完整冲突清单 → 手动输入目标主机名精确匹配 → 红色二次确认弹窗明写将覆盖生产数据。
- **登录与退出不动**：仍走后端现有表单 + 登录 CSRF + 限流路径，不新增 JSON 登录端点，
  已严格验收的鉴权契约零改动；SPA 收到 401 直接跳 `/login`。
- **静态资源要求登录**：未认证者拿不到控制台 JS，也就拿不到 API 形状；代价是登录页
  不能共用同一份 CSS，因此保持服务端渲染 + 内联样式（`/login.css` 实现期取消，
  见 design.md §8）。
- **`style-src` 保留 `'unsafe-inline'`**：Vue 的 `:style` 绑定与过渡会写 style 属性，
  静态嵌入的 HTML 无法注入 per-request nonce。在 `script-src 'self'` + `default-src 'none'`
  已堵死脚本注入的前提下接受此残余风险。这是取舍，不是遗漏。
- **总览大小趋势走后端摘要字段**而不是前端拉 N 份完整详情——后端已持有 hostRuns，加字段近乎零成本。
- **提交占位 `internal/hub/webui/dist/PLACEHOLDER`**：否则 `go:embed dist` 会让 `go build ./...`
  直接编译失败。Vite 因此也不能直接输出到该目录（`emptyOutDir` 会删掉它），
  产物由 `make web-build` 从 `web/dist` 同步。
- 不引入图表库，趋势用手写 SVG sparkline。

## Key Context

- 恢复确认 token **只在预检 operation 首次变为 `ok` 的那一次 GET 下发**，`issued` 置位后
  永不重发（`internal/hub/operation.go:284-313`）。前端轮询必须每次响应即时捕获，
  漏掉就只能重新预检；token 绑 session、10 分钟、单次消费。
- operation result 按 kind 有字段白名单（`internal/hub/operation.go:516-536`），
  前端 TS 类型须逐字段对齐。冲突项结构见 `internal/restore/execute.go:55-62`。
- `/api/alerts` 是实时投影不是历史（`internal/hub/health.go:136-162`），`created_at` 为投影时刻。
- 同一 Hub 进程只允许一个活动手工操作，冲突 `409`（`internal/hub/api_action.go:199-201`）。
- 高风险文件：`internal/hub/http.go`（CSP 与鉴权分流同处一地，改错会让未认证者拿到
  index.html 或让 CSP 失效）、`internal/hub/health.go`（摘要聚合与告警投影共用同一函数）。
- 本机 Node v22.21.1、pnpm 10.23.0；`.trellis/spec/` 目前没有前端规范，本轮新建。

## Risks / Deferred

- CSP 放宽是本轮唯一实质扩大的攻击面，必须在测试里对 CSP 字符串做精确断言，
  防止后续被无意收紧（前端白屏）或放宽（失去防护）。
- SPA fallback 若写错，未认证者可能拿到 index.html——需要专门的负向测试。
- 用 `go build` 而非 `make hub` 构建会得到占位界面的二进制；占位页会写明，
  并需在发布记录里写死「发布必须走 `make hub`」。
- Tailwind v4 已确认可用；TypeScript 退到 6.0.3，因为 `typescript-eslint` 8.67.0
  的 peer 上界是 `<6.1.0`，与 registry 上的 TS 7.0.2 不兼容。
- P3-3 遗留的 `CHK-001` / `FBK-001` 已接受风险不在本轮范围内，不因本任务改变。

## Acceptance

- `go test ./internal/hub ./internal/hub/webui ./internal/systemd -race -count=10` 通过，
  新增覆盖：`/assets/*` 未登录被拒、登录后可取、Content-Type 与 `immutable` 缓存头；
  登录后未知非 API 路径返回 index.html 而 `/api/` 未知路径仍 404 JSON；
  **未认证时 fallback 不泄露 index.html**；新 CSP 字符串精确断言；
  登录 CSRF Cookie 复用同值并续期。
- 摘要新字段覆盖无成功 run、只有失败 run、超过 14 次成功 run、warn 计入、多 target 求和。
- `make check` 全绿且不依赖 node；未构建前端时 `go build` 与 `CGO_ENABLED=0 go build` 仍成功。
- `pnpm -C web run lint|typecheck|test` 全绿，前端单测覆盖 401 跳转、
  **首次 ok 响应捕获 token**、倒计时过期禁用执行、409 提示、轮询退避与上限、sparkline 边界。
- `make hub` 产出的二进制里是真实界面而非占位页。
- 真机：未登录跳登录页；总览显示主机卡片、健康度、大小趋势、下次计划；主机详情完整；
  页面发起 backup 可见 running → 终态；`isolate` 恢复预检并执行成功且生产资源无变化；
  刷新后确认失效并提示重新预检；浏览器控制台无 CSP 报错；
  `systemctl stop ark-hub` 后备份与演练 timer 不受影响。

## Next Step

- 实现与验证已完成（含真机验收）；下一步按 Check-All 报告的处置结论决定修复范围。
