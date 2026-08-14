# 技术设计：P3 恢复可用

## 1. 任务拓扑

```text
p3-restore-plan
    -> p3-restore-execute
          -> p3-isolated-restore
          -> p3-live-rebuild-validation（人工）
                -> p3-verify（第二波 auto-loop）
                       -> p3-restore-ready（最终集成复核）
```

父任务只维护范围、依赖和最终集成结论。auto-loop 的队列顺序不表达依赖，启动时必须把
依赖边显式传给 `--depends-on`。

## 2. 数据主链

```text
backup.Manifest + config.Config
  -> restore.Plan
  -> restic.Dump(target snapshot)
  -> sshexec.Runner.Feed / Run
  -> files + image digest + volume + database + application
  -> compose health checks + manual checklist
  -> live validation / store.Verification
```

manifest 只提供备份事实，当前清单提供 source/target 的连接和项目参数。Plan 构建阶段必须
完成二者映射，执行阶段不得重新猜 target、服务、路径或目标主机。

## 3. 批次策略

- Wave A：`p3-restore-plan -> p3-restore-execute`，由 auto-loop 完成代码、检查、规范更新和
  本地提交。
- Wave B：人工执行跨主机重建实测；任何代码缺陷回到对应代码任务或新建缺陷任务，
  验收记录不临时修改业务代码。
- Wave C：在实机恢复链稳定后，由第二波 auto-loop 实现 `p3-verify`；第一版在原 host 的
  完全隔离 Compose 项目中每周执行一次，有条件时再迁移到专用验证 host。
- Final：全部子任务归档后，父任务反向审计 manifest 到恢复结果的数据链并更新 roadmap。
  P3-3 以“通过·已接受风险”纳入集成结论，严格干净 VPS 缺口不得改写为通过。

## 4. 安全边界

- `--dry-run` 只读；真实执行的目标冲突、覆盖和销毁必须由显式参数授权。
- `--to` 只引用清单内 host，避免在命令行携带临时身份、私钥或主机密钥策略。
- 真实服务器与离线材料永不进入 auto-loop；凭证不写任务文件、Git、stdout 或 stderr。
- verify 的生产隔离以 Compose project/volume/port/DNS 四层约束共同保证，销毁前再次校验
  `com.docker.compose.project` 标签，不能只靠资源名称。
- 同 host 演练与生产共享 Docker daemon，因此任何无法结构化隔离的 Compose 端口、bind mount、
  network 或资源标签都必须 fail closed，不能回退到“尽量隔离”。

## 5. 风险

- 当前 manifest 不携带完整 target 配置，必须与当前清单稳定匹配；清单漂移需要在 Plan
  阶段显式失败，不能在执行途中猜测。
- Redis RDB、Compose 端口重映射和应用健康判断依赖真实 Compose 结构；对应技术细节必须
  通过 P3-2/P3-5 的测试和 P3-3 实机证据闭环。
- `store.Verification` 需要一个稳定 snapshot ID；P3-1 应保留被选中的 manifest snapshot
  元数据，不能只返回解码后的 manifest。
- P3-3 的 destination 不是只预装 Docker、Compose v2 与 sshd 的干净 VPS；受限隔离恢复、
  source independence、业务数据、镜像 digest 与清理已通过，但严格验收缺口仍以 `CHK-001`
  保留。`FBK-001` 记录的本机回收站临时凭证可恢复风险同样未修复。
- P3-4 hub 自身重建实测因缺少独立干净机器且现有宿主机无 VM 空间而取消；同宿主机目录或
  容器隔离无法证明旧 hub 磁盘独立性，因此不以重复的受限演练替代。
