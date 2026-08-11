# 技术设计：清单模型 v2

## 1. 目标结构

```go
const SchemaVersion = 2

type Config struct {
    Version  int      `yaml:"version"`
    Repo     Repo     `yaml:"repo"`
    Defaults Defaults `yaml:"defaults"`
    Hosts    []Host   `yaml:"hosts"`

    path string // 仅用于错误信息
}

type Defaults struct {
    Schedule  *Schedule  `yaml:"schedule"`
    Retention *Retention `yaml:"retention"`
}

type Host struct {
    Host      string     `yaml:"host"`
    Local     bool       `yaml:"local"`
    SSH       *SSH       `yaml:"ssh"`
    Project   Project    `yaml:"project"`
    Targets   []Target   `yaml:"targets"`
    Schedule  *Schedule  `yaml:"schedule"`
    Retention *Retention `yaml:"retention"`
}

type SSH struct {
    Address        string `yaml:"address"`          // host:port
    User           string `yaml:"user"`
    IdentityFile   string `yaml:"identity_file"`    // 绝对路径
    KnownHostsFile string `yaml:"known_hosts_file"` // 绝对路径，必填
}
```

`Repo` / `Project` / `Target` / `Schedule` / `Retention` 与 `Target.ID()`、
`hostPattern`、`allowedFields`、`filledFields` 全部原样保留——它们描述
「备份什么」，与「谁来执行」无关，这轮不动。

`Host.SSH` 用指针而不是值：`local` 与 `ssh` 互斥的判定需要区分
「写了一个空的 ssh 段」和「压根没写 ssh」，值类型做不到。

## 2. 加载流程：两遍解析

v1 清单的顶层有 `host:` / `project:` / `targets:` 等字段，在 v2 结构上
用 `KnownFields(true)` 解析会直接炸出「field host not found in type config.Config」——
这正是 R6 要避免的那种含糊错误。

因此版本判定必须发生在严格解析**之前**：

```go
func Load(path string) (*Config, error) {
    data, err := os.ReadFile(path)          // 清单是人手写的小文件，整读无压力
    // 空文件判定沿用现有提示

    // 第一遍：宽松解析，只取 version
    var probe struct{ Version int `yaml:"version"` }
    yaml.Unmarshal(data, &probe)            // 解析失败留给第二遍报语法错，这里忽略

    switch probe.Version {
    case SchemaVersion:                     // 2，继续
    case 0:
        return nil, errors.New("version: 未声明。v2 清单必须显式写 version: 2")
    case 1:
        return nil, errors.New("version: 检测到 v1 单机清单。" +
            "架构已改为 hub 集中编排，清单需要迁移到 v2，参考 examples/ark.yaml")
    default:
        return nil, fmt.Errorf("version: 不支持的版本 %d，当前支持 %d", probe.Version, SchemaVersion)
    }

    // 第二遍：严格解析
    dec := yaml.NewDecoder(bytes.NewReader(data))
    dec.KnownFields(true)
    ...
}
```

**为什么 `version` 缺失也报错**：v2 之前默认补 `SchemaVersion` 是安全的，
因为只有一个版本。现在不兼容变更已经真实发生过一次，静默把没写版本号的清单
当成最新版，会让一份从旧文档抄来的清单以一串字段错误的形式失败。显式要求写版本号，
换来的是任何版本错配都有一条准确的提示。

## 3. 默认值继承

`applyDefaults` 只做两件事：

- `Repo.Type` 为空 → `restic`
- `Defaults.Schedule` / `Defaults.Retention` 为 nil → 填入常量默认值

**不把 defaults 拷进各 host**。归一化会丢掉「这个值是用户写的还是继承来的」，
导致 `on_calendar` 非法时错误指向 `hosts[2].schedule`，而用户其实写在 `defaults` 里。

生效值由访问器给出：

```go
// ScheduleFor 返回 host 实际生效的备份时机：host 自身覆盖优先，其次 defaults，
// 最后是内置常量（兜底分支让手工构造的 Config 也能拿到合理值）。
func (c *Config) ScheduleFor(h *Host) Schedule
func (c *Config) RetentionFor(h *Host) Retention
```

现有 `applyDefaults` 里「retention 三项同时为 0 才套默认值」的启发式删除：
指针已经精确表达了「没写」，不需要再猜。

## 4. 校验组织

`Validate` 保持「一次性收集全部错误」的形式，错误路径带下标：

```
Validate
├─ version / repo（全局，沿用现有 validateRepo）
├─ defaults.schedule / defaults.retention（仅在非 nil 时校验）
└─ 逐 host：前缀 hosts[i]，host 名已知时前缀改用 hosts[i](name) 便于定位
   ├─ host 非空 + hostPattern + 全局唯一（重名报出与哪个下标冲突）
   ├─ local / ssh 互斥：两者并存报错，两者皆空报错
   ├─ ssh 非空时：address / user / identity_file / known_hosts_file 必填，
   │              后两者必须是绝对路径
   ├─ project（沿用 validateProject）
   ├─ targets（沿用 validateTargets，ID 唯一性是 per-host 的——
   │           不同机器上有同名 volume 完全正常，靠 host tag 区分）
   └─ schedule / retention（仅在 host 显式写了时校验）
```

`hosts` 为空数组时报「至少需要一个 host」。

`Repo.URL` 不再校验是否带 host 段：URL 形态千差万别，可靠地判断
「末尾这段是不是机器名」做不到，误报的代价高于收益。改为在 `examples/ark.yaml`
的注释里说明「仓库全局唯一，不要按机器分路径」。

## 5. doctor 的过渡处理

远程化是 P1-3 的范围，但 `doctor` 引用了 `cfg.Project` / `cfg.Targets` /
`cfg.Schedule`，不改就编译不过。本轮做**最小适配**，不提前实现远程检查：

```go
func Run(ctx context.Context, cfg *config.Config) *Report
```

- 全局检查各做一次：`docker` / `restic` / `systemd-analyze` 二进制、
  `repo.password_file` / `repo.env_file` 权限。
- 逐 host：`Check.Name` 加 `<host> / ` 前缀。
  - `local: true` 的 host → 跑现有的全部本地检查（compose 文件、env 权限、
    on_calendar、targets 存在性）。
  - 远程 host → 只跑能在 hub 上判断的部分：`identity_file` 权限 0600、
    `known_hosts_file` 存在、`on_calendar` 语法；其余记一条
    **warn**「远程检查待实现（P1-3）」。

warn 而不是 fail，沿用「无法判断不伪造成确定有问题」的既有原则。

`ssh` 二进制检查不在本轮加——SSH 执行层 P1-2 才引入这个依赖，
提前检查一个还没被用到的命令只会让 doctor 的输出说谎。

## 6. CLI 输出

```
清单校验通过: /etc/ark/ark.yaml
  3 台机器 / 12 个备份目标

  hub-01（本机）项目 ark-hub，3 个目标，*-*-* 04:17:00
  web-01（ssh 10.0.0.11:22）项目 sub2api，5 个目标，*-*-* 04:17:00
  db-01（ssh 10.0.0.12:22）项目 pgcluster，4 个目标，*-*-* 00,06,12,18:23:00
```

不做列对齐：host 名和项目名长短不一，而「本机」这类中文的终端显示宽度
是字符数的两倍，按 `%-10s` 补空格反而会错位。改用括号和逗号分隔。
末尾带上生效的 `on_calendar`，让 defaults 与 per-host 覆盖的结果直接可见——
这正是 v2 新引入、最容易配错的一段。

`doctor` 本轮不加 `--host` / `--all` 标志（P1-3 一并做），退出码语义不变。
但它的检查项名会加上 `<host> / ` 前缀，`printReport` 的列宽相应从 28 放宽到 38。

## 7. 测试计划

`config_test.go` 整体重写，沿用现有的表驱动 + `wantSub` 子串匹配风格。

- `validManifest` 常量改为 3 台机器（`hub-01` local + `web-01` + `db-01`），
  各用例在它基础上做最小改动。
- 新增用例：host 重名、local 与 ssh 并存、local 与 ssh 皆空、
  缺 known_hosts_file、identity_file 相对路径、v1 清单、version 缺失、
  未知版本号、hosts 为空。
- 默认值用例改为验证 `ScheduleFor` / `RetentionFor` 的三层回退，
  以及「host 显式写 daily: 3 时 weekly/monthly 保持 0」。
- 保留 `TestLoad_RejectsUnknownField`（这个包最重要的一条）、
  `TestTargetID`、`TestLoad_EmptyFile`。
