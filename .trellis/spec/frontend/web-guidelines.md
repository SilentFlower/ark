# Web Guidelines

> `web/` 的目录划分、状态管理、API 契约、恢复确认、CSP 约束与测试要求。

---

## Scenario: 修改 ark-hub 控制台前端

### 1. Scope / Trigger

以下改动必须遵守本规约：

- 修改 `web/` 下的任何源码、配置或依赖；
- 新增或调整消费 `ark-hub` HTTP API 的类型、store 与页面；
- 修改手工操作的发起、轮询或恢复确认交互；
- 修改构建配置、产物路径或 `make hub` / `make web-check` 相关目标。

前端只认 HTTP DTO，不认 SQLite、restic、SSH 或清单文件结构。
需要新数据时先在 `internal/hub` 加投影字段，不要让页面去别处拼。

### 2. Directory Layout

```
web/
├── index.html            SPA 入口
├── vite.config.ts        outDir 为 web/dist，含 dev proxy
├── eslint.config.js
├── public/favicon.svg
└── src/
```

产物同步由 `make web-build` 负责：Vite 写 `web/dist`，Make 清掉
`internal/hub/webui/dist` 的旧产物（保留 `PLACEHOLDER`）再拷贝进去。
**不要把 `outDir` 改成嵌入目录**——Vite 的 `emptyOutDir` 会删掉 `PLACEHOLDER`，
而那个文件是 `go:embed dist` 在没跑过前端构建的机器上能编译的唯一保证。

```
src/
    ├── main.ts / App.vue      应用装配与外层布局
    ├── api/types.ts           与 Go DTO 一一对应的类型
    ├── api/client.ts          fetch 封装：CSRF、401 跳转、错误码映射
    ├── lib/                   纯函数：格式化、轮询状态机、sparkline 几何
    ├── stores/                Pinia：session / hosts / alerts / operations
    ├── components/            可复用展示组件
    └── views/                 四个页面：总览、主机详情、告警、操作
```

职责边界：

- `lib/` 只放**纯函数**，不碰网络、不碰 store。轮询与 sparkline 的边界逻辑都在这里，
  因为它们最需要被单测覆盖，而组件里的同样逻辑测不动。
- `api/` 只负责传输与错误翻译，不做业务判断。
- `stores/` 持有状态与流程，页面只做展示与事件转发。
- 组件不直接 `fetch`。

### 3. Contracts

#### API 类型必须与 Go DTO 逐字段对齐

`src/api/types.ts` 是跨语言契约的前端一侧。后端加字段时同步加，删字段时同步删。
可空性要照抄：Go 的 `*int64` / `*string` 对应 TS 的 `| null`，不能图省事写成可选属性。

operation 的 `result` 按 kind 有字段白名单（`internal/hub/operation.go` 的
`operationResultFields`），前端按 kind 断言即可，不需要逐字段防御——后端已经拒绝了
不合法的结果。

#### 会话与 CSRF

- 启动读 `GET /api/session` 取 `username` 与 `csrf_token`。
- 任何请求收到 `401` 一律 `location.assign('/login')`，**不在 SPA 内自建登录表单**。
  登录与退出走后端既有的表单路径，鉴权契约不因前端而改动。
- 业务 POST 必须带 `X-CSRF-Token` 与 `Content-Type: application/json`。

#### 恢复确认 token（最容易写错的一段）

后端只在恢复预检 operation **首次变为 `ok`** 的那一次 `GET /api/operations/{id}`
响应里下发 `confirmation_token`，`issued` 置位后永不重发
（`internal/hub/operation.go` 的 `issueConfirmation`）。因此：

- 轮询的**每一次**响应都要检查并就地保存 token，不能等轮询结束再回头取，
  也不能因为组件卸载就丢弃这次响应；
- token 只存内存。**不得写入 localStorage、sessionStorage 或 URL**——
  它代表「覆盖生产数据」的授权，一旦离开内存就无法保证只被消费一次；
- 界面必须显式告诉用户「刷新或关闭页面会作废本次确认」；
- 执行后无论成败都立即丢弃本地 token，它在后端也已被消费。

#### 时间敏感的值不要用 computed

Vue 的 `computed` 只在依赖变化时重算。**「确认是否过期」的依赖是时间本身**，
写成 computed 会让一个已过期的 token 一直报告为有效。这类判断要么写成接受
当前时间的函数，要么让调用方持有一个按秒推进的 ref 作为显式依赖。

#### 全局状态在按实体展示的页面必须过滤

`operations.active` 这类"当前操作"是整个应用共享的一份状态，而主机详情页是按
host 展示的。直接渲染全局状态会出现「在 web-01 发起备份，切到 db-01 看到
web-01 的结果」，让人误以为 db-01 上跑过任务。过滤要放在 store 里
（如 `activeFor(host)`）而不是模板条件，这样能被单测覆盖。

#### 加载失败不能让"查不到"看起来像"没有"

启动阶段的多个加载动作必须互不阻塞。`/api/session` 在 auth 存储运行期故障时返回
`503`（不是 401），不会触发登录跳转；如果 `session.load()` 直接抛出，紧随其后的
告警加载会被跳过，侧栏告警数停在 0——**「没有告警」和「没查到告警」在这个产品里
是完全不同的两件事**。所有 store 的加载方法都吞掉异常并记录 `error`，由页面展示。

同理，写请求在 `csrfToken` 为空时必须**在发出前**拦截并说明「会话未就绪」。
带空头发出去只会换来后端的 `403 invalid_request`，界面显示「请求参数无效」，
把会话问题伪装成参数问题，用户会一直重试而不知道该刷新。

#### 错误码而不是错误文案

后端错误码是稳定契约，`message` 会随措辞调整。分支一律基于
`ApiError.code`：`conflict` / `confirmation_required` / `confirmation_expired` /
`operation_failed` / `service_unavailable` / `invalid_request` / `not_found`。
未知码归入 `unknown`，不要把后端新增的码当成已知语义处理。

#### 不能粉饰「无法判断」

- `health === 'unknown'` 用中性色，不能显示成绿色。
- `diagnostics` 含 `schedule_unavailable` 时必须显示「计划不可解析」，
  不得把状态库里的最后已知 `next_run_at` 当作真实的下次计划时间展示。
- 没有成功备份时显示「从未」，不是 0 或空白。
- `/api/alerts` 是**实时投影不是历史流水**，`created_at` 是投影时刻。
  页面上要说清这一点，不做时间线，也不假装能回溯。

#### 破坏性操作的闸门

恢复默认 `isolate`。`force` 会覆盖生产数据，必须同时满足三重闸门才允许提交：
完整展示预检冲突清单 → 用户手动输入目标主机名且精确匹配 → 红色二次确认弹窗。
少任何一环都不行。

### 4. CSP 约束

后端 CSP 固定为 `script-src 'self'`，因此前端产物：

- 不得有内联 `<script>`、不得用 `eval` 或 `new Function`；
- 不得引用任何 CDN、外部字体或外部图片；
- 所有资源必须由 Vite 打包进 `assets/` 并带内容 hash。

`style-src` 保留 `'unsafe-inline'`，所以 Vue 的 `:style` 绑定和过渡动画可用；
但这不是放宽脚本的理由。改 CSP 必须同时改
`internal/hub/http.go` 的 `contentSecurityPolicy` 与断言它的测试。

### 5. Forbidden Patterns

| 禁止 | 原因 |
|---|---|
| 把 confirmation token 存进 localStorage / URL | 它是覆盖生产数据的一次性授权，离开内存就无法保证单次消费 |
| 用 `computed` 判断 token 是否过期 | 依赖是时间本身，缓存会让过期 token 一直显示为有效 |
| 在按实体展示的页面直接渲染全局"当前操作" | 切换主机会看到上一台的操作结果，状态失去可信度 |
| 启动阶段串行 await 且不捕获异常 | 前一个失败会跳过后面的加载，告警数静默归零 |
| `csrfToken` 为空时仍发出写请求 | 后端回 `403 invalid_request`，把会话问题伪装成参数问题 |
| 按 `message` 文案做错误分支 | 文案会调整，错误码才是契约 |
| 在 SPA 内自建登录表单或新增 JSON 登录端点 | 鉴权契约已严格验收，前端不改动它 |
| 引入图表库画一条 sparkline | 手写 SVG 十几行就够，省一个依赖和一份 CSP 麻烦 |
| 组件里直接 `fetch` | 绕过 CSRF、401 处理与错误码映射 |
| 用 `any` 绕过 DTO 类型 | 跨语言契约的漂移会安静地少显示一块信息 |
| 把 `unknown` 健康度显示成正常 | 「无法判断」不等于「通过」，与后端 doctor 的三态原则一致 |

### 6. Tests Required

`make web-check` 必须全绿。单测（Vitest）至少覆盖：

- API 客户端：错误码映射、未知码归入 unknown、非 JSON 错误响应、401 跳转、
  CSRF 头与 body 结构；
- 轮询状态机：终态立即返回、`interrupted` 是终态、每次响应都回调、
  前 10 次快间隔后退避、总时长上限、拉取错误向上传播；
- 恢复确认：**首次 ok 响应捕获 token**、过期后拒绝执行、执行后丢弃、
  发起新预检作废旧确认、`409` 冲突提示；
- 状态作用域与失败降级：`activeFor(host)` 只返回本主机的操作；
  `session.load()` 失败时不抛出且记录 error；`csrfToken` 为空时写请求被前置拦截；
- 纯函数：字节/时长/时间/相对时间格式化，sparkline 在 0 点、1 点、
  全等值和 14 点下的几何。

判断标准与后端一致：**这段逻辑出错时，错误会不会安静地留到需要恢复那天**。
会，就必须有测试。

### 7. Wrong vs Correct

#### Wrong

```ts
// 轮询结束后再去取 token
const final = await pollOperation({ fetchOnce, onUpdate: () => {} })
if (final.confirmation_token) {
  save(final.confirmation_token)
}
```

后端在 operation 首次变为 ok 的那一次读取里就把 token 发掉了。
最后这一次 `fetchOnce` 拿到的响应里已经没有它，用户只能重新预检。

#### Correct

```ts
// 每次响应都就地捕获
await pollOperation({
  fetchOnce,
  onUpdate: (operation) => {
    if (operation.confirmation_token) {
      save(operation.confirmation_token)
    }
  },
})
```

```ts
// 错误：computed 缓存了时间判断，过期 token 永远显示为有效
const valid = computed(() => confirmation.value !== null && confirmation.value.expiresAt > Date.now())

// 正确：把当前时间作为显式输入
function isConfirmationValid(now: number = Date.now()): boolean {
  const current = confirmation.value
  return current !== null && current.expiresAt > now
}
```
