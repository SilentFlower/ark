# Maven Evidence Contract

## 目录

1. 状态
2. 新鲜度
3. 覆盖关系
4. Check-All 边界

## 状态

- `reusable`：证据成功、新鲜，且覆盖全部当前要求。
- `partial`：证据仍新鲜，但 lifecycle、模块、消费者、测试或附属制品覆盖不足。
- `stale`：源码、测试、POM、effective model、Git HEAD、JDK 主版本或 Maven 版本发生变化。
- `failed`：原 Maven 命令非零退出或被中断。
- `blocked`：证据损坏、schema 不支持，或当前 Git/POM/工具链无法读取。

## 新鲜度

证据至少绑定：

- Git HEAD、Maven 根下 staged/unstaged/untracked 内容指纹；
- reactor 全部 POM 与使用的 effective POM内容指纹；
- 完整 argv、工作目录、module/consumer/test/artifact 选择；
- `.mvn/jvm.config`、`.mvn/maven.config`、`MAVEN_ARGS`、`MAVEN_OPTS` 中影响覆盖的参数，包括测试跳过属性；
- Java 主版本、Maven 版本、项目 `buildSide`、执行 runner 与可确认的本地仓库构建侧/宿主路径；
- 退出码、耗时、日志路径与内容摘要、测试统计。

runtime evidence 位于 `.trellis/.runtime/maven-verification/`，不得提交。证据损坏时不要删除或覆盖；报告 `blocked` 并保留现场。

- 计划同时记录可跨等价工作区比较的语义指纹，以及绑定本机绝对路径和全部执行字段的完整性指纹；`run` 和 `check --require-plan` 必须同时重算并拒绝不一致计划。
- Maven 模型输入中的绝对路径仅用于诊断；跨工作区语义指纹只绑定稳定输入 ID 与内容摘要，不能因用户名或 checkout 路径不同而漂移。
- evidence 必须有完整内容指纹，`check` 在读取状态或覆盖前先校验，不能信任被修改的顶层 coverage。
- quick/final mode、compile strategy 和显式 threads 都属于计划语义；任一变化必须改变 plan fingerprint。Check-All 使用 `--require-plan` 时，source-stale quick 不得冒充 conservative final。
- `buildSide`、runner、Maven executable 和本地仓库侧是本机完整性与新鲜度条件。计划生成、effective POM、run 或 check 任一步发生跨侧切换时 evidence 必须 `stale` 或 `blocked`。
- 命令日志必须记录内容摘要；日志缺失、截断或被改写时 evidence 为 `stale`，不能只检查路径存在。
- `run` 分别捕获执行前和执行后输入；源码、POM或工具链在 Maven 执行窗口内变化时，保留日志但把 evidence 标为 `stale`。
- `check --latest` 以 evidence 文件命名顺序选择最新候选；最新文件损坏时直接 `blocked`，不得静默回退旧成功 evidence。

## 覆盖关系

- 高 lifecycle 可以覆盖同模块的低 lifecycle，但不能反向覆盖。
- `compile` 不能满足 `test` 或 `package`。
- 跳过测试的 `test/package` 命令不能满足测试通过要求。
- 附属制品逐项匹配。跳过 sources 的 package 不能满足 sources artifact 要求。
- evidence 的 module/consumer 集合必须包含全部要求；额外模块不抵消缺失模块。
- 测试模式按计划中的明确 pattern 匹配；未声明 pattern 的普通 test 只证明计划范围内的默认测试集。

## Check-All 边界

Check-All 调用：

```bash
python3 ./.trellis/scripts/maven_verify.py check --latest --require-plan <plan.json>
```

`check` 只读取 Git、POM、evidence 和工具链指纹。普通已安装 Maven 可做只读版本探测；项目 wrapper 只复用冻结版本并校验 wrapper 文件与配置指纹，不执行可能下载发行包的 wrapper。它不执行 Maven goal、不创建 target、不下载依赖、不写本地仓库。

当结果不是 `reusable` 时：

- 报告状态和每个 `reason.code`；
- 引用 `required` 与 `actual`；
- 给出原计划或重新生成计划的精确命令；
- audit-only subagent 停止在报告，不自行重跑；
- 主会话只有在当前请求允许写构建缓存时才进入 `plan` / `run`。
