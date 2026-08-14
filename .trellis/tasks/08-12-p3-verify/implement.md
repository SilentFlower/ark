# 执行计划

## 步骤

1. 定义 `internal/verify` 的 Result、Detail、Baseline 和单 host 生命周期，复用现有 store status 与
   verification 字段，不新增数据库 schema。
2. 扩展 `restore.IsolationOptions` 的 published-port 策略：普通 restore 保持 `runtime_auto`，verify
   使用 `disabled`；补隔离 Plan、Compose 转换和回归测试。
3. 实现生产 Compose/container/image/network/volume/files 基线采集与脱敏 diff，确保全部命令使用
   固定 argv 和结构化输出。
4. 实现 verify 状态机：BuildPlan、verify isolation、Execute、默认 cleanup、keep-on-failure 归属校验、
   结束基线、bounded cancellation cleanup 和 `Store.RecordVerification`。
5. 新建 `internal/cli/verify.go`，接入 manifest/host 选择、全局锁、repo/store/runner、串行 all-host、
   `--snapshot`、`--keep-on-failure`、人类/JSON 输出和整体退出语义，并在 root 注册命令。
6. 扩展 `internal/systemd` 与 `ark install`，原子安装单个 weekly all-host verify service/timer，更新
   陈旧受管 timer 扫描和安装输出测试。
7. 增加不可读 snapshot、Feed/database/digest/health/baseline/cleanup/store 失败、多 host 继续、锁冲突、
   取消收尾和敏感信息不泄露测试。
8. 增加 Docker 集成场景：生产 project 保持运行，verify 不发布端口，成功/失败清理后生产基线不变；
   测试由 `testing.Short()` 和 Docker/Compose 检测保护。
9. 更新 directory、restore、backup orchestration、database、logging 和 external command 等可执行规范，
   同步 roadmap 的 P3-5 实际契约。

## 验证命令

```bash
go test ./internal/verify ./internal/restore ./internal/cli ./internal/store ./internal/systemd -race -count=1
go test ./internal/verify ./internal/restore ./internal/cli ./internal/systemd -race -count=10
make check
make build
CGO_ENABLED=0 go build -o bin/ark-nocgo ./cmd/ark
git diff --check
```

## 风险与回滚点

- Compose 端口策略必须扩展现有结构化转换，不能另写 YAML 文本替换；普通 isolation 端口行为是重点
  回归面。
- cleanup、生产基线复核和结果持久化都是最终状态的一部分，任何一项失败都不能输出成功。
- systemd unit 必须与现有 backup units 一起通过同一原子安装/回滚路径，不能分两次产生半更新状态。
- `DetailJSON` 和错误只保留阶段、资源标识与差异类型，不记录 canonical Compose、env 或业务数据。

## 完成前检查

- P3-3 的受限实机链路证据已归档且风险已由用户接受；不把严格干净 VPS 项写成通过。
- P3-4 已取消，不作为本任务依赖或完成条件。
- 默认执行位置为原 host、频率为每周一次；第一版无 `--to` 和自定义 schedule。
- 第二波 auto-loop 只包含本任务，不把真实服务器人工任务塞入 runner 队列。
