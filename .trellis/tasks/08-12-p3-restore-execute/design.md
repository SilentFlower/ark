# 技术设计：恢复执行

## 1. 包边界

`internal/restore/execute.go` 接收完整 Plan、Repo、destination Runner 和窄依赖，负责步骤状态机。
`internal/cli/restore.go` 负责加载、锁、doctor、破坏前备份、输出与退出语义。restore 不反向依赖
CLI；backup 的可复用编排边界应下沉到独立内部函数，而不是通过 Cobra 或子进程调用。

## 2. 状态机

```text
preflight
  -> detect conflicts
  -> optional forced safety backup
  -> files
  -> pull digests
  -> volumes
  -> database services ready
  -> database feeds
  -> full compose up
  -> health + digest verification
  -> manual checklist
```

每一步返回结构化结果。幂等不是“忽略 already exists”，而是重新检查步骤的完成后置条件：
文件元数据、镜像 digest、volume 归属、数据库 readiness、Compose project 标签和服务健康。

## 3. 强制恢复

默认 preflight 发现目标资源即停止。`--force` 先确认资源全部属于 Plan 的 destination Project，
然后复用 backup 指定 host 编排生成新的 manifest。备份整体状态不是 ok/warn 或 manifest 未成功写入
时不开始破坏性步骤。已有非目标标签资源永远不因 `--force` 获得删除授权。

## 4. 流生命周期

```text
reader := repo.Dump(snapshotID, stablePath)
feedErr := runner.Feed(ctx, reader, argv...)
closeErr := reader.Close()
return errors.Join(feedErr, closeErr)
```

`Repo.Dump` 的最终 Read/Close 负责暴露 restic Wait；`Runner.Feed` 负责远端退出状态。任何一侧失败
都表示恢复输入不完整。实现不得 `io.ReadAll` 或把 SQL/RDB/tar 落 hub 磁盘。

## 5. 数据库与 Compose

- Postgres 使用 Plan 中的 Project/Target 构造与 backup 相同的 compose 前缀；`pg_isready` 轮询
  受 context 控制，成功后 Feed 到 `psql`。
- Redis 在 service 停止状态恢复 RDB，通过独立临时 volume/容器完成流写入和权限准备，再把
  数据切入目标 volume；启动后 `redis-cli PING`。
- image digest 恢复与最终验证都使用实际容器 inspect 语义，不能只比较 compose 配置 tag。
- 最终 `docker compose up -d` 后逐服务判断 State/Health，并确认 Compose project label。

## 6. 测试

- fake Runner 记录 Run/Feed 顺序、argv、输入字节和错误。
- fake Repo 返回可注入最终 Close 错误的 reader，覆盖 restic 与 Feed 双错误。
- 表驱动覆盖默认冲突、force safety backup 失败、幂等跳过、各阶段中断和 context 取消。
- CLI 测试覆盖锁、doctor、JSON 单输出、errRestoreFailed 只转换退出码不重复打印。
