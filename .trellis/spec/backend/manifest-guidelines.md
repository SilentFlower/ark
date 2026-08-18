# Manifest Guidelines

> ark 备份清单（`ark.yaml`）的模型演进与校验规约。

---

## Overview

清单是 ark **唯一的用户输入契约**：hub 的一切行为都由它决定，而写它的人
通常在写完之后三个月才会再想起它。因此这里的规矩都指向同一个目标——
**任何配错都必须在 `ark validate` 那一刻暴露**，而不是在某个凌晨四点，
由一个无人值守的定时任务替你发现。

清单模型定义在 `internal/config/config.go`，格式版本为 `SchemaVersion = 2`
（v1 是每机 agent 时代的单机清单，架构改为 hub 集中编排后不再兼容，见 ADR-002）。

---

## Scenario: 修改清单模型或新增校验

### 1. Scope / Trigger

触发本规约的改动：

- 新增、删除或重命名清单字段；
- 升级 `SchemaVersion`；
- 新增一条校验规则；
- 新增可被 `defaults` 继承的配置项；
- 修改 `monitoring.env_file`、监控秘密键或出站 URL 安全边界。

### 2. Signatures

```go
const SchemaVersion = 2

func Load(path string) (*Config, error)            // 解析 + 补默认值，不校验
func LoadAndValidate(path string) (*Config, error) // 调用方通常用这个
func (c *Config) Validate() error                  // 静态语义校验，不碰文件系统
func (c *Config) ScheduleFor(h *Host) Schedule     // 生效值：host > defaults > 常量
func (c *Config) RetentionFor(h *Host) Retention
func (s SSH) EffectiveHostKeyPolicy() SSHHostKeyPolicy

type Monitoring struct {
    EnvFile string `yaml:"env_file"`
}

// internal/monitoring
type Settings struct {
    DingTalk  *DingTalkSettings
    Heartbeat *HeartbeatSettings
}

func monitoring.Load(path string) (monitoring.Settings, error)
func monitoring.SendDingTalk(
    ctx context.Context,
    settings monitoring.DingTalkSettings,
    message monitoring.MarkdownMessage,
) error
func monitoring.SendHeartbeat(
    ctx context.Context,
    settings monitoring.HeartbeatSettings,
    failed bool,
) error
```

### 3. Contracts

| 层级 | 字段 | 约束 |
|---|---|---|
| 顶层 | `version` | 必填且必须等于 `SchemaVersion`，不接受缺省 |
| 顶层 | `repo` | 全局唯一仓库，URL 不按机器分路径（ADR-009） |
| 顶层 | `defaults` | 可选，`schedule` / `retention` 均为指针 |
| 顶层 | `monitoring` | 可选；只含绝对路径 `env_file`，不保存 webhook、签名密钥或心跳 URL |
| 顶层 | `hosts` | 至少一台 |
| host | `host` | 匹配 `^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`，**全局唯一**（它是 restic tag 和恢复检索键） |
| host | `local` / `ssh` | 二选一，互斥且不可都缺 |
| host | `schedule` / `retention` | 指针，`nil` 表示套用 `defaults` |
| ssh | `address` | `host:port`，host 段非空，port 为 1–65535 的数字 |
| ssh | `identity_file` / `known_hosts_file` | 绝对路径，`known_hosts_file` 必填 |
| ssh | `host_key_policy` | 可选；空值等同 `accept-new`，也可显式写 `strict`；无跳过校验取值 |

环境依赖：无。`Validate` 全程不读文件系统、不连网络（ADR-008）——
文件存在性与权限属于 `doctor` 的职责。

监控秘密文件只允许以下键，未知键和非法组合必须拒绝：

```text
ARK_DINGTALK_WEBHOOK_URL
ARK_DINGTALK_SECRET
ARK_HEARTBEAT_SUCCESS_URL
ARK_HEARTBEAT_FAILURE_URL
```

- `ARK_DINGTALK_SECRET` 只能与 webhook 同时出现；两个 heartbeat URL 必须同时存在或同时缺失，允许相同。
- 文件必须是当前进程 effective UID 所有的普通文件、权限不超过 `0600`，并以 `O_NOFOLLOW` 打开；符号链接失败关闭。
- URL 禁止 userinfo、fragment 和空 host。默认只允许 HTTPS；HTTP 仅允许 `localhost` 或 loopback IP。
- HTTP 客户端固定 5 秒超时、64 KiB 响应上限和 3 次尝试；只重试网络错误、`429` 与 `5xx`。
- 禁止跟随重定向。端点只在秘密文件加载时校验一次，跟随 3xx 会绕过 HTTPS/loopback 边界。

### 4. Validation & Error Matrix

| 条件 | 错误 |
|---|---|
| `version` 缺失 | `version: 未声明。v2 清单必须显式写 version: 2` |
| `version: 1` | `version: 检测到 v1 单机清单…参考 examples/ark.yaml` |
| `version` 为其它值 | `version: 不支持的版本 N，当前支持 2` |
| `hosts` 为空 | `hosts: 至少需要一台机器` |
| host 重名 | `hosts[i](name).host: %q 与 hosts[j] 重复，host 名称必须全局唯一` |
| `local: true` 且有 `ssh` | `hosts[i](name): local 为 true…不能同时配置 ssh` |
| 非 local 且无 `ssh` | `hosts[i](name).ssh: 远程机器必填…` |
| `address` 无端口 | `…不是合法的 host:port，端口必须显式写出` |
| `address` 无 host（`:22`） | `…缺少主机名或 IP` |
| `address` 端口非数字（`h:ssh`） | `…的端口必须是 1-65535 之间的数字` |
| 缺 `known_hosts_file` | `…不能为空。生产数据会流经这条连接，不校验主机密钥意味着中间人既能窃取数据、也能在恢复时投毒` |
| `host_key_policy` 不是 `accept-new` / `strict` | `…非法，只允许 "accept-new" 或 "strict"` |
| `retention` 三项同时为 0 | `…三项不能同时为 0，否则清理时会删光所有快照` |
| `monitoring` 存在但 `env_file` 为空或是相对路径 | `Validate` 返回 `monitoring.env_file` 字段级错误，不访问文件系统 |
| 监控文件不存在、不是普通文件、owner 不匹配、权限过宽或是 symlink | `monitoring.Load` fail closed，错误只包含路径和失败类别 |
| 监控文件含未知键、孤立签名密钥或单边 heartbeat URL | `monitoring.Load` 拒绝，不猜测配置意图 |
| URL 为非 loopback HTTP、含 userinfo/fragment 或不是绝对 URL | `monitoring.Load` 拒绝，不发起网络请求 |
| 端点返回 3xx | 不跟随 `Location`，按非 2xx 错误处理 |
| 端点返回 400 | 立即失败，不重试 |
| 端点返回 429 或 5xx | 最多重试到 3 次；最终错误不包含完整 URL 或秘密 |

### 5. Good/Base/Bad Cases

- **Good**：3 台机器（含一台 `local: true`），`defaults` 写全，个别机器覆盖
  `schedule` / `retention`——见 `examples/ark.yaml`。
- **Base**：只写必填字段，`defaults` 整段省略，全部套用内置常量。
- **Base**：省略 `monitoring`，钉钉和 heartbeat 都关闭，所有命令保持零新增网络请求。
- **Good**：`monitoring.env_file` 指向 root-only 文件，钉钉与 heartbeat 可独立启用，两个 heartbeat URL 可以相同。
- **Bad**：`address: :22`（能解析、连不上）；两台机器同名（快照混在一个 tag 下，
  恢复时无法区分）；`local: true` 同时写 `ssh`（意图不明，不猜）。
- **Bad**：把 webhook/token 直接写进 YAML，或允许已校验的 HTTPS URL 通过 307 跳转到非 loopback HTTP。

### 6. Tests Required

`internal/config/config_test.go` 用一份 `validManifest` 作基准，
各用例在它上面做最小改动。**每新增一条校验规则，必须同时新增一条
断言其错误信息子串的用例**（表驱动 + `wantSub`）。

监控清单和出站边界至少运行：

```bash
go test ./internal/config ./internal/envfile ./internal/monitoring ./internal/doctor -race -count=10
```

必须有断言点的路径：

- 每种版本错配（缺失 / v1 / 未知）各一条；
- 每条互斥或必填规则各一条；
- 默认值继承：未覆盖的机器拿到 `defaults`、覆盖的机器拿到自己的值、
  显式写 `retention: {daily: 3}` 时 `weekly` / `monthly` 保持 0；
- 主机密钥策略覆盖空值默认 `accept-new`、显式 `strict` 和非法值拒绝；
- 错误信息包含出错机器的 host 名（多机清单里没有 host 名的错误无法定位）。
- `monitoring.env_file` 的空值、相对路径、旧清单兼容与严格未知字段拒绝；
- 监控文件的 owner/mode/symlink、未知键、键组合、loopback HTTP 与不安全 URL；
- 钉钉 400 不重试、429/5xx 受限重试、真实超时、64 KiB 上限、业务 JSON/错误码、固定签名向量；
- 307/308 不到达重定向目标，所有超时、网络、HTTP 与业务错误都不包含 URL query 或 secret。

### 7. Wrong vs Correct

#### Wrong

```go
type Monitoring struct {
    DingTalkWebhookURL string `yaml:"dingtalk_webhook_url"`
    DingTalkSecret     string `yaml:"dingtalk_secret"`
}
```

把秘密放进清单会让它进入示例、备份、错误上下文和普通配置审阅流程。

#### Correct

```go
type Monitoring struct {
    EnvFile string `yaml:"env_file"`
}

settings, err := monitoring.Load(cfg.Monitoring.EnvFile)
if err != nil {
    return err
}
```

YAML 只记录绝对路径；文件安全、允许键、URL 与出站协议由 `internal/monitoring` 在同一边界统一校验。

---

## Convention: 版本判定必须先于严格解析

**What**：`Load` 先用一个只含 `version` 的结构宽松解析取版本号，
判定通过后才用 `KnownFields(true)` 严格解析完整结构。

**Why**：v1 清单的顶层是 `host` / `project` / `targets`，直接拿 v2 结构严格解析
只会得到一串「未知字段 host」，而用户真正需要知道的是「这份清单该迁移了」。
版本判定放在后面，等于把最有用的那条信息埋在噪音里。

```go
// 正确：宽松探测 → 版本判定 → 严格解析
var probe struct{ Version int `yaml:"version"` }
if err := yaml.Unmarshal(data, &probe); err == nil {
    if err := checkVersion(probe.Version); err != nil {
        return nil, err // 带迁移指引
    }
}
dec := yaml.NewDecoder(bytes.NewReader(data))
dec.KnownFields(true)
```

宽松解析自身失败时**跳过版本判定**，让严格解析报出带行号的 YAML 语法错误——
否则一个缩进错误会被报成「version 未声明」。

下次升级到 v3 时照此办理：先加 `case 2:` 的迁移提示，再改结构。

---

## Convention: 错误路径前缀带下标和 host 名

**What**：多机清单里的每条错误都以 `hosts[i](name).<字段路径>` 开头。

**Why**：一份十几台机器的清单，只报 `targets[2] 有问题` 会让用户自己去数第几台。
下标用于精确定位，host 名用于人眼确认。host 名为空时退化为 `hosts[i]`。

```go
prefix := fmt.Sprintf("hosts[%d]", i)
if h.Host != "" {
    prefix = fmt.Sprintf("hosts[%d](%s)", i, h.Host)
}
```

拆分出的各 `validateXxx` 统一接收 `prefix string` 和 `add func(string, ...any)`
两个参数，新增校验项时沿用这个签名。

---

## Convention: 可继承字段用指针 + 访问器，不在加载期归一化

**What**：可被 `defaults` 继承的字段在 `Host` 上声明为指针，`nil` 表示「没写」。
`applyDefaults` **只填 `Defaults` 自身**，不把值拷进各个 host；
生效值由 `Config.XxxFor(h)` 计算，优先级为 host > defaults > 内置常量。

**Why**：两条理由。

1. 指针精确表达「没写」，不需要靠「三项是否同时为 0」这类启发式去猜。
   用户写 `retention: {daily: 3}` 就是只想留 3 天日备，不该被补上周备月备。
2. 加载期归一化会丢掉「这个值是谁写的」。`on_calendar` 非法时错误会指向
   `hosts[2].schedule`，而用户其实写在 `defaults` 里——他会去改一个没写错的地方。

访问器保留一级常量兜底，让手工构造的 `Config`（测试里很常见）也能拿到合理值。

---

## Don't: 用「能解析」代替「能用」

**Problem**：

```go
// 放行了 ":22"（host 为空）和 "10.0.0.11:ssh"（服务名端口）
if _, port, err := net.SplitHostPort(s.Address); err != nil || port == "" {
    add("%s.address: 不是合法的 host:port", prefix)
}
```

**Why it's bad**：`net.SplitHostPort(":22")` 返回 `host="" port="22" err=nil`。
标准库只负责拆分语法，不负责判断语义。而 `ssh -p` 只接受数字端口，
空 host 更是连不上任何机器。这两种写法都会通过 `validate`，
把错误推迟到第一次真正执行备份的时候。

**Instead**：

```go
host, port, err := net.SplitHostPort(address)
if err != nil { /* 报错并 return */ }
if host == "" {
    add("%s: %q 缺少主机名或 IP", prefix, address)
}
if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
    add("%s: %q 的端口必须是 1-65535 之间的数字", prefix, address)
}
```

**推广**：任何「用标准库函数判断用户输入是否合法」的地方都要问一句——
**这个函数的宽松边界和 ark 的实际可用边界是同一条线吗？** 通常不是。
`url.Parse`、`time.ParseDuration`、`filepath.IsAbs` 都有类似的落差。

---

## Common Mistakes

### 在 `config.Validate` 里访问文件系统

**Symptom**：hub 上校验一份包含远程机器路径的清单时报「文件不存在」。

**Cause**：把 `os.Stat` 写进了 `Validate`。

**Fix**：移到 `internal/doctor`。`Validate` 是纯静态的，这样 hub 才能校验
任意一台机器的清单段落而不需要连上那台机器（ADR-008）。

**Prevention**：`internal/config` 不 import `os` 之外的环境相关包；
需要真实环境判断时，问自己「这条检查在一台没连上目标机的笔记本上还成立吗」。

### 新增字段忘了同步四处

新增一个清单字段时，下面四处必须一起改，漏一处就会出现
「文档说有、代码不认」或「代码认、没人知道」：

1. `internal/config/config.go` 的结构体与校验；
2. `internal/config/config_test.go` 的 `validManifest` 与失败用例；
3. `examples/ark.yaml`（带注释说明为什么需要它）；
4. `docs/design.md` §5 的清单样例。

改之前先 `grep -rn "<字段名>" .` 确认没有遗漏的引用点。
