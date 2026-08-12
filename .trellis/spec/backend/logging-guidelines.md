# Logging Guidelines

> ark 的输出通道、结构化上报与密钥红线。

---

## Overview

**ark 目前没有引入任何日志库**，也没有 debug/info/warn/error 分级。
这不是遗漏，是当前形态下的合适选择：

`ark` 是 oneshot 进程，由 hub 上的 systemd timer 触发，跑完退出（ADR-005）。
它的 stdout/stderr 直接进 systemd journal，时间戳、单元名、PID
都由 journald 补齐。再套一层 `logrus`/`zap` 只会得到重复的时间戳前缀。

真正需要被程序消费的运行结果，走的是**结构化状态库**而不是日志文本
（见下文 Structured Reporting）。

在 `ark-hub`（P4）引入之前不要加日志框架。如果某天需要，先在 `docs/design.md`
里补一条 ADR 说明是什么需求逼出了这个依赖。

---

## Output Channels

### 人类可读输出一律走 cmd.OutOrStdout()

**不要用 `fmt.Println` 或 `fmt.Printf` 直接写 stdout。**
所有面向用户的正常输出都通过 cobra 命令对象拿到 writer：

```go
// internal/cli/root.go:96
fmt.Fprintf(cmd.OutOrStdout(),
    "清单校验通过: %s\n  主机 %s / 项目 %s / %d 个备份目标\n",
    cfg.Path(), cfg.Host, cfg.Project.Name, len(cfg.Targets))
```

理由是可测性：测试里可以 `cmd.SetOut(&buf)` 捕获输出做断言。
直接写 `os.Stdout` 的代码没法在不劫持全局状态的前提下测试。

### 错误输出走 stderr，且只在一个地方

错误信息只在 `Execute` 里打印一次（`internal/cli/root.go:44`）：

```go
fmt.Fprintln(os.Stderr, "错误:", err)
```

各层函数只负责返回带上下文的 error，**不要自己再打印一遍**。
否则同一个问题会在屏幕上出现两三次，反而让人怀疑是不是出了多个故障。

### 格式化约定

`doctor` 的报告用固定宽度对齐，让几十行检查结果可以竖着扫：

```go
// internal/cli/root.go:145
fmt.Fprintf(out, "%s %-28s %s\n", statusSymbol(c.Status), c.Name, c.Detail)
```

状态符号用 `✓` / `!` / `✗`（`statusSymbol`，`internal/cli/root.go:152`），
不用颜色转义码——输出经常被重定向进文件或 journal，
转义码在那里只是乱码。

---

## Structured Reporting

需要被机器消费的输出走 JSON，与人类可读输出是两条独立通道，不要混排。

### --json flag

`doctor` 提供 `--json`（`internal/cli/root.go:137`），供监控采集：

```go
enc := json.NewEncoder(cmd.OutOrStdout())
enc.SetIndent("", "  ")
```

开了 `--json` 就**只输出 JSON**，不要再夹带人类可读的进度行——
调用方通常直接 `| jq`，任何多余的行都会让解析失败。

新增命令时，只要输出可能被脚本消费，就照此加 `--json`。

### 状态库是主要的可观测面

每次运行结束后把结果写进本地 SQLite 状态库 `/var/lib/ark/ark.db`
（P2-1，`internal/store/`）。`ark`（oneshot）写，`ark-hub`（常驻）读，WAL 模式下并发安全。
需要长期留存、跨机聚合的信息应该进状态库，而不是指望有人去翻 journal。

早期设计里的 `_status/<host>.json` 对象存储上报**已取消**——hub 自己就是执行者，
不需要绕对象存储传状态（design.md §9）。

结构化数据的字段用 snake_case 的 json tag，与现有类型一致：

```go
// internal/doctor/doctor.go:41
type Check struct {
    Name   string `json:"name"`
    Status Status `json:"status"`
    Detail string `json:"detail"`
}
```

---

## What to Log

按「三个月后有人来查这次备份为什么不对」的标准来判断该输出什么：

- 每个 target 的执行结果、耗时、产出大小、restic snapshot ID。
- 外部命令失败时，**执行的是哪条命令**（脱敏后的 argv）。
  只说「docker 命令失败」等于没说。
- 降级和跳过：哪一项因为什么前提不满足而没有执行。
  静默跳过是备份工具最危险的行为。
- 版本信息（`ark version` 输出的 Version/Commit/Date，构建时注入，见 Makefile）。
  排查历史问题时需要知道当时跑的是哪个二进制。
- 下一次计划运行时间。`doctor` 已经从 `systemd-analyze calendar`
  的输出里提取 `Next elapse` 展示（`internal/doctor/doctor.go:190`）——
  比复述表达式有用得多。
- SSH 主机密钥刷新可以输出算法名和 `SHA256:` 指纹，供管理员带外核对；
  必须同时明确扫描结果不是身份验证，且预览状态下未修改信任库。

---

## What NOT to Log

这一节是硬红线。stdout/stderr 会进 systemd journal，
而 journal 的读取权限（`systemd-journal` 组）比 `0600` 的密钥文件宽得多。
**写进日志的密钥等于降权了的密钥。**

绝对不允许出现在任何输出里：

- `repo.password_file` 的**内容**（restic 仓库密码）。
  它一旦泄漏，加上对象存储的读权限就能解密全部备份。
- `repo.env_file` 的**内容**（对象存储 AK/SK）。
- `project.env_file` 的内容（应用的数据库密码、加密密钥）。
- 数据库转储的任何片段。它就是全量业务数据。
- 传给外部命令的环境变量集合。打印 `cmd.Env` 会把凭证整个吐出来。
- `known_hosts` 原始公钥行、身份私钥内容或扫描得到的原始 key blob。刷新命令只输出
  算法名与 SHA256 指纹；JSON 输出也遵守同一边界。

只输出**路径**，不输出内容：

```go
// ✅ internal/doctor/doctor.go:180
r.add(name, StatusOK, "%s", path)

// ✅ 权限问题只报权限位，不碰内容
r.add(name, StatusFail, "%s 权限过宽（当前 %04o，应为 0600）", path, perm)

// ❌ 绝不
r.add(name, StatusOK, "密码已读取: %s", string(content))
```

### 外部命令输出要当作可能含密钥来处理

`runCommand`（`internal/doctor/doctor.go:286`）在失败时会把
`CombinedOutput` 拼进错误信息。这对 `docker --version` 无害，
但对注入了凭证环境的 restic 调用未必安全——
某些失败路径下工具会回显配置。往 restic 封装里加错误处理时，
先确认它的错误输出不含 `AWS_SECRET_ACCESS_KEY` 之类的值，
详见 `external-command-guidelines.md`。

### 仓库 URL 可以输出，但要意识到它含账号标识

`s3:https://<account>.r2.cloudflarestorage.com/<bucket>/<host>` 里
带着对象存储账号 ID。它不是密钥，输出到 journal 可以接受，
但不要往公开的 issue、截图或外部服务里贴。
`.gitignore` 已经排除真实清单 `/ark.yaml`，只提交 `examples/ark.yaml`。
