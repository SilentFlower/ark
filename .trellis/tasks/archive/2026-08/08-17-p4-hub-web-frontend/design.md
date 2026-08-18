# P4-3 技术设计

## 1. 架构与边界

本任务按用户决策合并了 roadmap 的 P4-3 与 P4-4，交付形态是：
`systemctl start ark-hub` 之后浏览器直接打开真实控制台，不需要 node 环境。

```
web/                         Vue 3 + Vite + TS + Pinia + Tailwind 源码
  └─ pnpm build ─► internal/hub/webui/dist/    构建产物（Vite outDir 直接落在这里）
                        │
                        └─ go:embed ─► ark-hub 单二进制
```

包边界沿用 `.trellis/spec/backend/directory-structure.md`：

- 新增 `internal/hub/webui/`：**只做一件事**——用 `go:embed` 持有前端产物并暴露
  `fs.FS` 与一个 `http.Handler`。不含任何业务逻辑、不依赖 `store` / `config`。
- `internal/hub` 装配路由时引用 `webui`，不直接碰 `embed.FS`。
- 前端不引入后端概念：`web/` 只认 HTTP DTO，不认 SQLite、不认 restic。

### 为什么不新起一个静态文件服务包

`webui` 的全部职责就是「把嵌入的 dist 以正确的 Content-Type 和缓存策略吐出去」，
它能独立解释自己在解决什么问题，符合目录规范里「按职责边界分包」的判据。
放进 `internal/hub` 会让本已 5000 行的包再背一个不相关的关注点。

---

## 2. 后端接入改动

### 2.1 路由

`internal/hub/http.go:145-164` 的 mux 增加与调整：

| 路由 | 变化 | 行为 |
|---|---|---|
| `GET /` | 改 | 不再渲染 Go 占位模板，返回嵌入的 `index.html` |
| `/`（catch-all） | 改 | 鉴权后：`/api/` 前缀保持 404 JSON；其它路径交给 webui——命中嵌入文件就返回该文件（`/assets/*`、`/favicon.svg`），否则返回 `index.html`（SPA history fallback） |

**实现期修正**：原计划枚举 `GET /assets/{path...}` 与 `GET /favicon.ico` 两条路由，
实际改为由 catch-all 统一分流。理由是 webui 本来就要按路径查嵌入产物，枚举路由等于
把同一件事写两遍，还要在两处各写一遍鉴权；统一分流后「鉴权 → API 分流 → 产物查找 →
fallback」是一条直线，也让「未认证不得拿到任何产物」只需要在一个地方保证。

**实现期修正（放弃 `/login.css`）**：原计划新增一个公开的 `/login.css`，好让 CSP 用
`style-src 'self'`。但为了 Vue 的 `:style` 绑定，`style-src` 最终仍需保留
`'unsafe-inline'`（见 2.2），`/login.css` 因此失去唯一的技术理由，只剩「视觉统一」
这个用内联样式同样能达成的弱理由。多一个未鉴权可达的端点却不换来任何安全收益，
所以取消：登录页保持服务端渲染 + 内联样式，配色与控制台对齐。

静态资源要求登录是刻意的：未认证者拿不到控制台的 JS，也就拿不到 API 形状。

`handleProtectedNotFound` 语义变为「鉴权后按请求路径分流」：`/api/` 走 404 JSON，
其余走 SPA fallback。**未认证的行为完全不变**（HTML 重定向登录、`/api/` 返回 401 JSON），
这条是安全契约，不能因为 fallback 而放松。

### 2.2 CSP

现值（`internal/hub/http.go:447`）禁止一切脚本，必须放宽。新值：

```
default-src 'none';
script-src 'self';
style-src 'self' 'unsafe-inline';
img-src 'self' data:;
font-src 'self';
connect-src 'self';
form-action 'self';
base-uri 'none';
frame-ancestors 'none'
```

**`style-src` 保留 `'unsafe-inline'` 是一个明确的权衡，不是疏忽。**
Vue 的 `:style` 绑定和过渡动画会写 `style` 属性，CSP3 下没有 `'unsafe-inline'`
就会被拦掉；改用 nonce 需要给每次请求的 HTML 注入 nonce，而我们的 HTML 是静态嵌入文件。
在 `script-src 'self'` + `default-src 'none'` 已经堵死脚本注入的前提下，
内联 style 的剩余风险是数据泄露类的 CSS 注入，对一个单管理员、内网监听的运维台可以接受。
登录页现有的内联 `<style>` 因此不必改结构，只同步配色与控制台对齐。

`connect-src 'self'` 足够：前端只调本源 API，不访问任何外部服务。
登录页因此不必改结构，仍是服务端渲染 + 内联样式。

### 2.3 缓存

现在所有响应都是 `Cache-Control: no-store`（`internal/hub/http.go:451`）。
带内容 hash 的静态资源改为 `public, max-age=31536000, immutable`；
`index.html` 与所有 API 保持 `no-store`。这样升级 hub 后不会吃到旧 JS。
实现方式是在 `securityHeaders` 之后、由 `webui` handler 覆写自己的 `Cache-Control`。

### 2.4 摘要 DTO 增加大小字段

总览卡片要展示「大小趋势」（`docs/roadmap.md:585`、`docs/design.md` §9），
而 `hostSummaryResponse` 目前没有任何字节字段。

`projectHosts` 已经为每个 host 取了 `ListHostRuns(host, 100)`（`internal/hub/health.go:53`），
数据就在手上，因此在摘要里补两个字段，避免总览页为了一个数字去拉 N 份完整详情：

```go
type hostSummaryResponse struct {
    // ...现有字段不变...
    LastBackupBytes   *int64              `json:"last_backup_bytes"`
    RecentBackupSizes []backupSizePoint   `json:"recent_backup_sizes"`
}

type backupSizePoint struct {
    RunID      string `json:"run_id"`
    FinishedAt string `json:"finished_at"`
    Bytes      int64  `json:"bytes"`
}
```

规则：

- 只统计 `status ∈ {ok, warn}` 的 run（失败 run 的字节数没有可比性）。
- 每个点的 `bytes` 是该 run 内**全部 target 字节之和**。
- 按时间正序，最多 14 个点；不足 14 个就有几个给几个，前端不补零。
- `LastBackupBytes` 取最后一个点的值；一次成功 run 都没有时为 `null`。
- 只增字段，不改任何现有字段，对既有 API 消费者向后兼容。

---

## 3. 前端设计

### 3.1 目录

```
web/
├── index.html
├── package.json / pnpm-lock.yaml
├── vite.config.ts            outDir 指向 ../internal/hub/webui/dist，含 dev proxy
├── tsconfig.json / tsconfig.node.json
├── eslint.config.js
└── src/
    ├── main.ts
    ├── App.vue               外层布局：侧栏 + 顶部当前告警条
    ├── api/
    │   ├── client.ts         fetch 封装：CSRF、401 跳转、错误码映射
    │   └── types.ts          与 Go DTO 一一对应的 TS 类型
    ├── stores/
    │   ├── session.ts        username / csrf_token
    │   ├── hosts.ts          主机摘要与详情
    │   ├── alerts.ts         当前告警
    │   └── operations.ts     手工操作 + 轮询 + 恢复确认 token
    ├── router/index.ts       history 模式
    ├── components/           HealthBadge / Sparkline / StatusPill / ConfirmDialog ...
    └── views/
        ├── OverviewView.vue
        ├── HostDetailView.vue
        ├── AlertsView.vue
        └── OperationsView.vue
```

### 3.2 页面

**总览 `/`**：每台机器一张卡片——health 徽章、最近备份状态与时间、最近备份大小 +
14 点 sparkline、下次计划时间、target 数、`local` 标记、`diagnostics`
（`schedule_unavailable` 时明确显示「计划不可解析」而不是伪造一个时间）。
顶部横幅汇总当前告警数。

**主机详情 `/hosts/:host`**：摘要区 + target 清单 + 备份历史（每条可展开看 target
明细：状态、字节、耗时、snapshot id、错误）+ 最新 doctor 报告（check 名称/状态/详情）
+ 演练记录 + 操作区。

**告警 `/alerts`**：当前告警列表。这里要向用户说清楚语义——
`/api/alerts` 是**实时投影**而不是历史流水（`internal/hub/health.go:136-162`），
`created_at` 是投影时刻，所以页面上标注为「当前告警」，不做时间线展示。

**操作 `/operations`**：手工操作分页列表（keyset），点开看单条详情，
按 `kind` 结构化展示 `result`（backup 看 run/manifest snapshot，verify 看结果集，
restore 看步骤与人工确认清单）。

不做全局 `/runs` 页面：主机详情已覆盖单机历史，跨机时间线目前没有明确用途（YAGNI）。
`/api/runs` 的客户端方法仍然实现，留给后续。

### 3.3 会话与 CSRF

- 应用启动先 `GET /api/session`，拿 `username` 与 `csrf_token` 存 Pinia。
- 任何请求收到 `401` → `location.assign('/login')`，不在 SPA 内自建登录表单。
  登录仍走后端现有的表单 + 登录 CSRF Cookie + 限流路径，**鉴权契约零改动**。
- 退出复用现有 `POST /logout`（form-urlencoded + 重定向）：前端提交一个隐藏 form，
  而不是新增 JSON 端点。`form-action 'self'` 允许这个做法。
- 所有业务 POST 带 `X-CSRF-Token` 与 `Content-Type: application/json`。

### 3.4 操作轮询与恢复确认（最容易做错的一块）

`GET /api/operations/{id}` 只在预检 operation **首次**变为 `ok` 的那一次响应里下发
`confirmation_token`，`issued` 置位后永不重发（`internal/hub/operation.go:284-313`）。

因此 `operations` store 的轮询状态机必须：

1. 以 2s 间隔轮询，连续 10 次后退避到 5s，总时长上限 30 分钟。
2. **每次响应都检查 `confirmation_token` 是否非空，非空立刻写入 store**。
   不能等轮询结束再回头取，也不能因为组件卸载就丢弃这次响应。
3. token 只存在内存，并显示 10 分钟倒计时。UI 明确提示：
   **刷新或关闭页面会作废本次确认，需要重新预检**。
4. 倒计时归零后禁用执行按钮，引导重新预检。

其它错误码映射：`409 conflict` → 「已有手工任务正在运行」并给出跳转操作页的入口；
`409 confirmation_required` / `422 confirmation_expired` → 提示重新预检；
`503 service_unavailable` → 「服务暂不可用（可能是清单损坏）」。

### 3.5 恢复交互

主机详情页发起恢复：

1. 表单：source（当前 host，只读）、destination（下拉，来自 `/api/hosts`）、
   snapshot（默认 `latest`）、mode（**默认 `isolate`**）。
2. 提交 `action: "preview"` → 轮询 → 展示 `plan.steps`、`plan.manual_checks`、
   `conflicts[]`（resource / detail / force_allowed）、以及 `destructive`、`resume` 标记。
3. `mode: "force"` 的额外闸门（用户已确认的决策）：
   - 冲突清单必须先完整展示；
   - 用户手动**输入目标主机名**，与 destination 精确匹配才解锁；
   - 红色二次确认弹窗明写「将覆盖 `<host>` 上的生产数据」。
4. 确认后 `action: "execute"` 带 `preview_operation_id` + `confirmation_token`。
5. 恢复完成后把 `manual_checks`（DNS、证书、防火墙、`.env` 调整等）作为
   一等公民展示，而不是折叠在结果 JSON 里。

### 3.6 不引入的东西

- 不引入图表库：大小趋势是手写 SVG sparkline，十几行代码，省一个依赖和一份 CSP 麻烦。
- 不做深色模式、不做 i18n（单管理员、中文单语，YAGNI）。
- 不做前端侧的告警静默：静默期属于 P4-5 的钉钉通知，不是页面状态。

---

## 4. 构建与嵌入

### 4.1 产物路径与 `go build` 可用性

`go:embed` 只能嵌入本包目录下的文件，所以前端产物最终要落在
`internal/hub/webui/dist`。

由此产生一个必须解决的问题：**`dist` 不存在或为空时 `go build ./...` 会编译失败**。

**实现期修正（两轮）**：

原计划提交一份占位 `dist/index.html`，但那样会让每次前端构建都覆盖一个受版本控制的
文件，`git status` 永远是脏的，而且 `.gitignore` 无法既忽略构建产物又保留同名占位文件。
第二版方案改为两个文件：

- `internal/hub/webui/dist/PLACEHOLDER`：提交，唯一作用是让 `dist/` 非空。
  `.gitignore` 忽略该目录下其它一切。
- `internal/hub/webui/placeholder.html`：提交在**包目录**而非 dist 下，因此不会被构建
  覆盖。真实产物缺少 `index.html` 时 webui 回落到它并提示改用 `make hub`。

但第二版仍有缺陷，是跑通 `make hub` 之后才暴露的：Vite 的 `emptyOutDir` 会**清空整个
输出目录**，把 `PLACEHOLDER` 一起删掉。构建一次之后提交，干净 clone 上的
`go build ./...` 就会失败——而且失败发生在别人的机器上，本地看不出来。

最终方案是把 Vite 的 `outDir` 退回 `web/dist`，由 `make web-build` 负责同步：

```make
pnpm --dir web run build
find internal/hub/webui/dist -mindepth 1 ! -name PLACEHOLDER -exec rm -rf {} +
cp -R web/dist/. internal/hub/webui/dist/
```

多一步拷贝换来三件事：`PLACEHOLDER` 不可能被构建删除；旧的 hash 产物每次同步前被清掉，
不会在二进制里累积；直接跑 `pnpm build` 只影响 `web/dist`，不会让嵌入目录进入
「没走 make 却变了」的中间状态。

于是：

- `go build ./cmd/ark-hub` 与 `go test ./...` 永远能跑，不需要 node。
- `make hub` 产出真实界面。
- 误用 `go build` 发布得到的是一页明确的占位提示，而不是白屏。
- 前端缺失**不阻止服务启动**：它不影响 API、登录和备份调度，把整个 Hub 拦在门外
  是过度反应。`webui.Built()` 供需要时查询实际状态。

### 4.2 Makefile

```make
## web-install: 安装前端依赖
## web-check:   前端 lint + 类型检查 + 单测
## web-build:   构建前端产物到 internal/hub/webui/dist
## hub:         web-build 之后编译 ark-hub 单二进制
```

`make check` 保持纯 Go（vet + gofmt + go test），不依赖 node；
前端改动由 `make web-check` 覆盖。两者都必须绿才提交。

---

## 5. 兼容与迁移

- **无数据库改动**，schema 仍是 v2，不需要迁移。
- API 只增字段（`last_backup_bytes`、`recent_backup_sizes`），旧客户端不受影响。
- `ark-hub.service` 的参数与安装流程不变；`ark` 二进制不受影响。
- 升级路径就是替换 `ark-hub` 二进制并重启，无额外操作。
- 回滚就是换回旧二进制重启，无状态残留。

---

## 6. 运维影响

- 界面随二进制一起分发，目标机与 hub 都不需要 node。
- 静态资源要求登录，所以未登录时页面只有 Go 渲染的登录页，攻击面没有扩大。
- CSP 从「禁止一切脚本」放宽到「只允许同源脚本」，这是引入 SPA 的必然代价，
  已在 2.2 记录取舍理由，需同步进 `hub-guidelines.md`。

---

## 7. 需要同步更新的规范与文档

| 文件 | 更新内容 |
|---|---|
| `.trellis/spec/backend/hub-guidelines.md` | 路由分流、CSP、缓存、静态资源鉴权、摘要 DTO 新字段、占位产物、测试要求 |
| `.trellis/spec/frontend/index.md` + `web-guidelines.md`（新增） | 前端目录、状态管理、API 客户端、恢复 token 状态机、CSP 兼容约束、测试要求 |
| `.trellis/spec/backend/directory-structure.md` | 登记 `internal/hub/webui/` 与 `web/`，补依赖方向 |
| `docs/roadmap.md` | P4-2 标记为已完成；P4-3 与原 P4-4 合并并记录理由；原 P4-5 顺延为 P4-4 |
| `docs/design.md` §9 | 前端接入形态、CSP 取舍、静态资源鉴权、摘要大小字段 |
| `README.md` | 当前状态表、`make hub` / `make web-check` 用法、发布必须走 make hub |

---

## 8. 实现期偏离规划的三处修正

都已在上文对应小节展开，这里集中列出以便复核：

1. **不新增 `/login.css`**（§2.1）。`style-src` 既然要为 Vue 保留 `'unsafe-inline'`，
   这个公开端点就不再换来任何安全收益。
2. **不枚举 `/assets` 与 `/favicon.ico` 路由**（§2.1），改为 catch-all 统一分流，
   让鉴权与产物查找只有一条路径。
3. **占位方案拆成 `dist/PLACEHOLDER` + 包内 `placeholder.html`，且 Vite 不直接写嵌入目录**（§4.1）。
   前者避免构建产物与受版本控制的文件同名互相覆盖；后者是因为 `emptyOutDir` 会连
   `PLACEHOLDER` 一起删掉，而那个后果只会在别人的干净 clone 上暴露。

---

## 9. 真机验证中发现并修复的既有缺陷

用真实浏览器跑通登录时暴露了一个**规划外的既有缺陷**，它在 P4-1 就已存在，
但因为所有测试都走 httptest 而从未被发现：

**现象**：真实浏览器里登录稳定失败在 `403 CSRF 校验失败`，而同样的流程用 curl 完全正常。

**根因有两层**：

1. 浏览器打开登录页时会自动请求 `/favicon.ico`。该请求未认证，被 `handleProtectedNotFound`
   重定向到 `/login`，于是**又渲染了一次登录页**。原 `renderLogin` 每次渲染都签发新的
   CSRF Cookie，因此用户手里那个表单的 token 对应的 Cookie 已被覆盖。
2. 第一版修复（复用已有 Cookie 值、不重发 Set-Cookie）解决了覆盖，却引入了第二个问题：
   复用时没有刷新过期时间，用户拿到的是一个即将到期的 Cookie。页面停留几十秒再提交，
   Cookie 已被浏览器丢弃，请求**根本不带 Cookie**，同样是 403。

**最终修复**：复用合法形态的 token 值 + 每次渲染都重新下发 Cookie 续期。
两条缺一不可，已写入 `hub-guidelines.md` 的契约与错误矩阵，并补了专门的回归测试
（模拟 favicon 触发的二次渲染 + 时间推进 9 分钟后的续期断言）。

这个缺陷的教训值得记下来：**httptest 不会请求 favicon，也不会让 Cookie 真的过期。**
涉及浏览器实际行为的路径，必须至少用真浏览器走一遍。
