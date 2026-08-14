# Maven Lifecycle Policy

## 目录

1. 生命周期覆盖
2. 昂贵绑定
3. 模块范围
4. 编译策略与并行
5. 计划升级条件

## 生命周期覆盖

按以下偏序判断普通 Maven lifecycle 覆盖：

```text
validate < compile < test < package < verify < install < deploy
```

附属 goal 独立判断，不能从普通 lifecycle 自动推出：

- `sources`、`javadoc`、`assembly`、`shade`、`repackage`、`copy-dependencies` 分别记录。
- `-DskipTests` 不产生测试通过证据。
- `-Dmaven.test.skip=true` 同时跳过 test compilation，不能满足测试编译或测试运行要求。
- `-Dmaven.source.skip=true` 不影响普通 compile 证据，但不能满足 sources 制品验收。
- `-Dmaven.compiler.useIncrementalCompilation=false` 在已确认兼容的 compiler plugin 上按源文件/class stale 判断；它只用于 compile 局部反馈，不能自动满足 conservative final。

## 昂贵绑定

| 插件/goal | 常见阶段 | 默认处理 |
| --- | --- | --- |
| `maven-source-plugin:jar*` | compile/package | 非 sources 验证可在确认参数后跳过 |
| `maven-dependency-plugin:copy-dependencies` | prepare-package | compile/test 不进入；package 明示复制成本 |
| `spring-boot:repackage` | package | 只在可运行制品验收时进入 |
| `maven-shade-plugin:shade` | package | 只在 shaded artifact 验收时进入 |
| `maven-assembly-plugin:*` | package | 只在 assembly 验收时进入 |
| `maven-javadoc-plugin:*` | package/verify | 非文档制品验收优先停在更早阶段 |
| frontend install/build goal | generate-resources 等 | 不自动跳过；报告绑定、阶段和项目风险 |

插件绑定来自 effective POM；只扫描仓库原始 POM不能排除外部父 POM继承。

- execution 没有显式 `<phase>` 时，只能使用脚本内已确认的 plugin goal 默认阶段兼容表。
- 已识别为昂贵 goal、但默认阶段仍未知时，计划必须降低 `confidence` 并报告 `binding-phase-unknown`；不得当成“当前 lifecycle 不会执行”。
- 只有全部命中的 `maven-source-plugin` 版本都在兼容表覆盖范围内时，才可自动添加 `-Dmaven.source.skip=true`；版本缺失或过旧时报告 `sources-skip-unsupported`。
- 只有全部命中的主源码 `maven-compiler-plugin:compile` 版本都为 3.1 或更高时，quick auto 才可添加 `-Dmaven.compiler.useIncrementalCompilation=false`。无法确认时降级 conservative；显式 source-stale 失败关闭。

## 模块范围

- 构建侧由 Maven 根所在的原生文件系统决定：原生 Windows/Linux 使用本侧工具链；WSL Windows 挂载项目使用 Windows Maven/JDK/settings/本地仓库，WSL ext4 项目使用 Linux 工具链。不得因 Codex 运行在 WSL 就强制所有项目使用 Linux Maven。
- 自动 Maven 选择顺序是同侧项目 wrapper、同侧 PATH Maven；不自动安装或升级 Maven。Maven 3.9+ 的 `MAVEN_ARGS` 等能力只在当前已选 Maven 实际支持时生效。
- Windows 与 WSL 路径可在 evidence 中双表示，但 Maven、JDK、环境、settings 和本地仓库不能跨侧混搭。显式参数无法映射到项目构建侧时失败关闭。
- 把变更文件映射到最近的 reactor module POM。
- 根 POM以及 `.mvn/maven.config`、`jvm.config`、extensions、wrapper 配置变化按全 reactor 风险处理。
- `MAVEN_ARGS` 只在 Maven 3.9+ 计入有效参数；旧版本保留诊断信息，但不能据此判断测试、制品或本地仓库覆盖。
- Maven 从 Linux 侧访问位于 `9p`、`drvfs`、CIFS/NFS 等高延迟小文件文件系统的本地仓库时，计划必须报告 `local-repository-high-latency-filesystem`。Windows Maven 原生访问 Windows 盘时不得仅因 WSL 宿主视图是 `9p` 就误报。只有调用方已准备同侧完整仓库时，才通过 `--local-repository` 显式切换；不得自动复制仓库、修改 `settings.xml` 或把不完整仓库用于离线验证。
- `quick` 选择变更模块并默认加 `-am`，覆盖必要上游而不读取陈旧本地 SNAPSHOT。source-stale 的 `fallbackArgv` 保持相同 reactor 范围，但恢复 conservative 编译。
- `final` 选择变更模块和显式消费者，并使用 `-am` 覆盖必要上游。
- 消费者必须来自任务材料、项目 spec、可靠的反向依赖结果或显式输入。依赖坐标含未展开属性时不得按同名 artifactId 猜测关系；降低置信度并要求显式 module/consumer。不要默认使用 `-amd`。
- 公共 DTO/API、跨模块协议和父 POM变化通常需要提高消费者覆盖，由任务 owner 决定范围。

## 编译策略与并行

- `auto`：quick compile 且兼容时选择 source-stale；其它 quick lifecycle 和所有 final 默认 conservative。
- `conservative`：保留 Maven compiler plugin 默认语义，适合公共 API/ABI、常量内联、注解处理器、POM、资源契约或跨模块协议变化。
- `source-stale`：只允许 compile。quick 可自动选择；final 必须由任务材料明确确认模块内部低风险变化后显式选择。
- `--threads` 只接受正整数或正数 CPU 倍数。模型根据 reactor 范围与依赖拓扑、插件线程安全、测试共享端口/文件以及 CPU、内存、I/O 压力，自行决定是否启用以及使用哪个并行度。
- 常规 implement 只执行当前选定的一份 Maven 计划，不为选择并行度额外运行串行或其它线程配置；Check-All 不运行 Maven goal。

## 计划升级条件

只有满足对应验收时升级：

| 当前目标 | 最低 goal |
| --- | --- |
| 语法、注解处理、主源码编译 | `compile` |
| 单元测试或测试契约 | `test` |
| JAR/WAR、资源布局、repackage、依赖复制 | `package` |
| 集成检查或质量插件 | `verify` |
| 下游必须消费本地安装制品 | `install`，需明确理由 |
| 发布远端仓库 | 不由本 Skill 自动执行 |

计划进入 `package`、`install` 或 `deploy` 时，必须在报告中列出触发原因和命中的昂贵绑定。
