# Error Handling

> ark 的错误传播、聚合与对外表达方式。

---

## Overview

ark 是一个 oneshot 命令行进程：出错时没有重试队列，没有人盯着日志，
只有 systemd 记下的一个退出码和一份可能没人看的输出。
因此错误处理的目标只有一个——**让失败在发生的那一刻就无法被误认为成功**。

三条贯穿全项目的规则：

1. 返回 `error`，不 `panic`。目前代码里没有任何 `panic`，也不要新增。
   唯一的进程终止点是 `cmd/ark/main.go` 里的 `os.Exit(cli.Execute())`。
2. 每一层包装错误时都补上「当时在做什么」的上下文，用 `%w` 保留错误链。
3. 能一次说清的问题，不要让用户跑三遍才看全。

---

## Error Types

### 普通错误：直接用 fmt.Errorf 包装

绝大多数场景不需要自定义类型。带中文上下文包装即可：

```go
// internal/config/config.go:156
f, err := os.Open(path)
if err != nil {
    return nil, fmt.Errorf("打开清单失败: %w", err)
}
```

错误消息约定：

- **用中文**，与项目其他用户可见文本一致。
- **不以句号结尾**，因为它常被嵌进更长的句子里。
- **带上出问题的值**，用 `%q` 包裹字符串，让空格和空字符串可见：
  `add("host: %q 非法，只允许小写字母、数字和中划线", c.Host)`。
- **说清期望**，不只说「非法」。对比
  `"version: 期望 %d，实际 %d"` 与 `"version 错误"`。

### 哨兵错误：只在需要被上层识别时定义

目前全项目只有一个，它的存在是为了区分退出码：

```go
// internal/cli/root.go:32
// errChecksFailed 表示 doctor 发现了必须修复的问题。
var errChecksFailed = errors.New("环境检查未通过")
```

判断时一律用 `errors.Is`，不要比较字符串，也不要用 `==`
（`==` 在错误被包装后就失效了）：

```go
case errors.Is(err, errChecksFailed):
    return 2
```

新增哨兵错误前先问：**上层真的需要按类型分支吗？**
如果只是为了让消息好看一点，用 `fmt.Errorf` 就够了。

### 不要为了「规范」造错误类型体系

项目里没有 `AppError`、没有 error code 枚举、没有错误分类树。
Go 的错误链加上清楚的中文消息已经够用。在 hub（P3）出现之前，
不需要面向 API 的结构化错误。

---

## Error Handling Patterns

### 校验类错误一次性全收集

配置校验绝不能「改一个报一个」——那会让用户在一份 40 行的清单上往返十几次。
`Validate` 用一个闭包收集器加 `errors.Join` 聚合全部问题：

```go
// internal/config/config.go:269
func (c *Config) Validate() error {
    var errs []error
    add := func(format string, args ...any) {
        errs = append(errs, fmt.Errorf(format, args...))
    }

    // ... 各分项校验都往 add 里塞

    return errors.Join(errs...)
}
```

`errors.Join` 在 errs 为空时返回 nil，所以不需要手动判断长度。
拆分出的各个 `validateXxx` 方法统一接收 `add func(string, ...any)` 参数，
新增校验项时沿用这个签名。

注意其中一处刻意的例外（`internal/config/config.go:357`）：
target 类型未知时直接 `continue`，不再检查它的其他字段——
类型都不认识，后续针对该类型的检查没有意义，报出来只会是噪音。

### 「无法判断」不等于「有问题」

`doctor` 不返回 `error`，而是返回一份三态报告：

```go
// internal/doctor/doctor.go:31
StatusOK   Status = "ok"    // 检查通过
StatusWarn Status = "warn"  // 不影响本次备份，但需要关注
StatusFail Status = "fail"  // 备份或恢复会失败，必须修复
```

关键在于依赖处理（`internal/doctor/doctor.go:91` 的 `Run`）：
docker 不可用时，依赖 docker 的 compose 与 volume 检查降级为 **warn**，
而不是伪造成 fail：

```go
if composeOK {
    checkTargets(ctx, r, cfg)
} else {
    r.add("targets", StatusWarn, "docker compose 不可用，跳过目标存在性检查")
}
```

把「我没法检查」报告成「确定有问题」会让运维去修一个不存在的故障；
反过来报成 ok 则更糟——它会让人以为这项已经验证过了。
新增检查项时必须想清楚这三态各自对应什么，不要图省事只用 ok/fail。

### 单条检查失败不中断整轮

`checkTargets` 里每个 target 独立判断，失败后 `continue` 到下一个。
一次 `doctor` 应该告诉用户全部问题，而不是第一个问题。

### 显式忽略的错误要写出来

不关心的错误用 `_ =` 明确标注，让 reader 知道这是决定而非疏漏：

```go
defer func() { _ = f.Close() }()
```

不要写裸的 `defer f.Close()`（linter 会报），也不要真的去处理
只读文件的 Close 错误。

---

## Exit Codes

ark 的退出码是对 systemd 和监控脚本的契约，改动前想清楚兼容性：

| 退出码 | 含义 | 来源 |
|---|---|---|
| 0 | 成功 | `err == nil` |
| 1 | 工具本身出错（清单读不了、参数不对、内部错误） | default 分支 |
| 2 | 检查未通过（清单合法但环境有问题） | `errors.Is(err, errChecksFailed)` |

区分 1 和 2 的意义：监控脚本可以对「ark 挂了」和「环境需要人工修」
做不同处置。实现在 `internal/cli/root.go:35`：

```go
func Execute() int {
    err := newRootCmd().Execute()
    switch {
    case err == nil:
        return 0
    case errors.Is(err, errChecksFailed):
        // 检查报告已经打印过，这里不再重复输出错误信息。
        return 2
    default:
        fmt.Fprintln(os.Stderr, "错误:", err)
        return 1
    }
}
```

根命令设置了 `SilenceUsage: true` 和 `SilenceErrors: true`
（`internal/cli/root.go:58`），因为：

- `SilenceErrors` — 错误输出由 `Execute` 统一负责，否则 cobra 会再打一遍。
- `SilenceUsage` — 运行时错误（比如清单文件不存在）后跟着刷 80 行帮助文本，
  会把真正的错误信息顶出屏幕。用法错误 cobra 仍会正常提示。

---

## Common Mistakes

- **用 `==` 比较哨兵错误**。被 `%w` 包装一次就失效，一律用 `errors.Is`。
- **包装时丢掉 `%w`**，写成 `fmt.Errorf("xxx: %v", err)`，
  错误链断掉，上层再也无法用 `errors.Is` 判断。
- **校验时遇到第一个问题就 return**，用户被迫一轮一轮试错。
- **把「无法检查」报成 fail 或 ok**，见上文三态部分。
- **在错误信息里回显密钥文件的内容**。包装 `repo.env_file` /
  `repo.password_file` 相关错误时只能提路径，绝不能带内容——
  错误信息会进 systemd journal，而 journal 的读取权限比密钥文件宽得多。
  同理，外部命令失败时不要无条件回显它的完整输出，
  详见 `external-command-guidelines.md`。
- **给运行时错误保留 usage 输出**，真正的错误被帮助文本淹没。
