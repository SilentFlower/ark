# Quality Guidelines

> ark 的提交门槛、测试要求与注释标准。

---

## Overview

ark 的质量标准由一条现实决定：**它的失败是延迟暴露的**。
一个写错的备份工具会连续三个月「成功」运行，直到需要恢复的那天才暴露。
没有用户会来报 bug，因为在那之前没人能察觉。

所以这里的要求偏严：任何让「失败看起来像成功」的写法都是被禁止的，
哪怕它更简洁。

---

## Required Patterns

### 提交前必须跑 make check

```bash
make check   # = fmt + vet + test
```

三步的具体内容见 `Makefile`：

| 目标 | 命令 | 说明 |
|---|---|---|
| `fmt` | `gofmt -w .` 后校验 `gofmt -l` 为空 | 格式化并确认没有漏网文件 |
| `vet` | `go vet ./...` | 标准静态检查 |
| `test` | `go test ./... -race -count=1` | 竞态检测 + 禁用测试缓存 |

`-race` 不是可选项。ark 后续要并发执行多个 target
（roadmap P1-2），竞态问题在备份工具里表现为间歇性的数据损坏，
本地跑不出来、线上也难复现。

`-count=1` 用来禁用 Go 的测试结果缓存。改了外部依赖（比如装了新版 restic）
但没改 Go 代码时，缓存会让你以为测试跑过了。

目前项目没有引入 golangci-lint。`gofmt` + `go vet` 够用，
需要更多规则时先讨论，不要单方面加进 Makefile。

### 每个包都要有 package doc 注释

而且要写清**职责边界**，不是复述包名。参照 `internal/config/config.go:1`：

```go
// Package config 加载并校验 ark 的备份清单（ark.yaml）。
//
// 校验刻意分成两层：
//   - Validate 只做静态语义校验（字段是否齐全、路径是否绝对、ID 是否重复），
//     全程不碰文件系统。因此中心机可以校验任意一台机器的清单，而不需要
//     那台机器上的文件真的存在。
//   - 运行环境校验（文件是否存在、权限是否安全…）属于 doctor 包的职责。
package config
```

对比反例 `// Package config 处理配置。` —— 后者不提供任何信息。

### 导出标识符都要有文档注释

所有导出的类型、常量、函数、方法都写注释，以标识符名开头（Go 惯例）。
非导出的复杂函数同样要写。

### 注释解释「为什么」，不复述「做什么」

这是本项目注释密度偏高的原因，也是最有价值的部分。代码能表达 what，
表达不了 why。范例：

```go
// internal/config/config.go:30
// 各项默认值。默认备份窗口刻意避开整点和半点：
// 全世界的定时任务都挤在 0 分和 30 分，错开几分钟能显著降低
// 对象存储侧的并发冲突和限流概率。
const (
    DefaultOnCalendar = "*-*-* 04:17:00" // 每天 04:17，即 RPO 24h
)
```

```go
// internal/config/config.go:161
// 未知字段一律报错。备份清单里的拼写错误如果被静默忽略，
// 会让人以为某个目标已经在备份、而实际上它从未被执行过——
// 这种错误只会在真正需要恢复的那天暴露。
dec.KnownFields(true)
```

**注释用中文**，与项目其他文本一致。

涉及 ADR 的代码要在注释里点明约束理由，让后来者知道这里不能随便改
（例：`internal/config/config.go:49` 的 `TargetPostgres` 说明为什么不能打包 PGDATA）。

---

## Forbidden Patterns

| 禁止 | 原因 |
|---|---|
| `panic` | 全项目零 panic。备份进程崩溃比返回错误更难诊断。 |
| `os.Setenv` 注入凭证 | 会泄漏给后续所有子进程。见 `external-command-guidelines.md`。 |
| `fmt.Println` / 直接写 `os.Stdout` | 无法测试。用 `cmd.OutOrStdout()`。 |
| 静默忽略错误（裸 `_, _ =` 且无注释） | 备份工具里的静默失败是最危险的缺陷类型。 |
| YAML 解析不开 `KnownFields(true)` | 拼错的字段名会被忽略，目标从未被备份。ADR-007。 |
| 打包 PGDATA 卷代替 `pg_dump` | 得到不一致的快照，恢复时才发现。ADR-003。 |
| 按 tag 而非 digest 拉镜像 | tag 可变，半年后 schema 已不兼容。ADR-004。 |
| `--stdin-filename` 里带时间戳 | restic 去重失效，仓库体积线性膨胀。 |
| 在 `config.Validate` 里访问文件系统 | 破坏静态/环境校验的分层。ADR-008。 |
| 外部命令不设超时 | docker 守护进程异常时会让整个进程挂死。 |
| 提前抽象接口来「方便 mock」 | 目前没有需要 mock 的边界，徒增间接层。 |

---

## Testing Requirements

### 覆盖标准：按风险而不是按行数

项目不设覆盖率门槛。判断标准是：**这段逻辑出错时，
错误会不会安静地留到恢复那天**。会，就必须有测试。

`internal/config` 是当前唯一有测试的包（229 行测试对 416 行实现），
因为清单校验正是「错了不会立刻发现」的典型。

`internal/config/config_test.go:117` 那条测试的注释写明了这种取舍：

```go
// TestLoad_RejectsUnknownField 是这个包最重要的一条测试：
// 清单里的拼写错误必须立刻失败，而不是被静默忽略后
// 在真正需要恢复的那天才发现某个目标从没被备份过。
```

### 表驱动测试

多分支场景用表驱动加 `t.Run` 子测试，参照
`internal/config/config_test.go:128` 的 `TestValidate_Errors`：

```go
tests := []struct {
    name    string
    mutate  func(string) string
    wantSub string
}{
    {
        name:    "host 含非法字符",
        mutate:  func(s string) string { return strings.Replace(s, "host: web-01", "host: Web_01", 1) },
        wantSub: "host",
    },
    // ...
}
for _, tc := range tests {
    t.Run(tc.name, func(t *testing.T) { /* ... */ })
}
```

子测试名用中文描述场景。断言错误信息时匹配**子串**而不是全文，
否则每次微调措辞都要改测试。

### 固定基准 + 最小改动

用一份完整合法的样本（`validManifest`），各用例在它基础上做
最小改动来构造异常。这样测试意图一目了然：改了什么，就是在测什么。
不要每个用例都从零手写一份 YAML。

### 测试辅助函数

- 辅助函数第一行调 `t.Helper()`，失败时行号才指向调用处
  （`internal/config/config_test.go:50`）。
- 临时文件用 `t.TempDir()`，自动清理，不要用 `/tmp` 硬编码路径。
- 写文件用 `0o600`，与生产代码对密钥文件的要求一致。

### 命名

`TestXxx` 或 `TestXxx_Scenario`，如 `TestLoad_AppliesDefaults`、
`TestValidate_RetentionAllZero`。

### 依赖外部二进制的测试要能跳过

需要真实 restic / docker 的集成测试用 `testing.Short()` 保护：

```go
if testing.Short() {
    t.Skip("需要 restic，短模式跳过")
}
```

CI 环境不一定装了这些工具，不加保护会让 `go test ./...` 直接红掉
（roadmap P2-2 的验收标准已明确这一点）。

---

## Code Review Checklist

- [ ] `make check` 通过（fmt / vet / test -race 全绿）。
- [ ] 新增包有 package doc，说明职责边界而非复述包名。
- [ ] 复杂逻辑的注释解释了「为什么」，涉及 ADR 的地方点明了约束。
- [ ] 没有引入 Forbidden Patterns 表里的任何一项。
- [ ] 新的失败路径**会被用户看到**——没有静默 continue、没有吞掉的 error。
- [ ] 密钥只出现在路径层面，没有任何内容进入输出或错误信息。
- [ ] 外部命令有 context 超时，凭证只注入子进程环境。
- [ ] 信任库等安全文件的更新使用同目录临时文件、限制权限并原子替换；失败路径保留原文件，
      且符号链接或非普通文件不会被覆盖。
- [ ] 「错了不会立刻发现」的逻辑有对应测试。
- [ ] 依赖外部二进制的测试有 `testing.Short()` 保护。
- [ ] 用户可见文本、注释、错误信息都是中文。
