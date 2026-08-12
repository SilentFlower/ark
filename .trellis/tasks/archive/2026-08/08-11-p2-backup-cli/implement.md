# 执行计划

## 步骤

1. 建立 backup 内部编排函数、依赖注入和 dry-run 计划输出。
2. 实现非阻塞 flock、local/host doctor、host/target 串行与结果聚合。
3. 接入 Repo、stream integrity、manifest、Store 和统一 forget/prune。
4. 实现全量 service、per-host service/timer 模板和 host schedule 映射。
5. 实现 ark 管理标记、陈旧 timer 安全回收、原子写入、verify 和 install CLI。
6. 补齐成功、partial、失败、取消、锁冲突、timer 碰撞、dry-run 和安装回滚测试。

## 验证

```bash
go test ./internal/cli ./internal/systemd ./internal/backup -race -count=1
systemd-analyze verify <测试生成的 service> <测试生成的 timer>
make check
make build
CGO_ENABLED=0 go build ./cmd/ark
git diff --check
```

## 高风险点

- 每 host timer 必须兑现 `ScheduleFor`，且不能因 timer 重叠绕过全局 flock。
- partial run 必须同时对用户、状态库和 manifest 可见。
- dry-run 与 unit 文件不能读取或输出任何凭证内容。
