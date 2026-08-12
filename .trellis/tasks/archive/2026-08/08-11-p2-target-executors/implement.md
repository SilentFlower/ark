# 执行计划

## 步骤

1. 定义最小 executor result 与分发入口，复用现有 compose argv 规则。
2. 实现 postgres、volume、files 及其 argv/稳定 filename 测试。
3. 实现 Redis LASTSAVE 状态机、context 轮询和失败测试。
4. 实现运行容器到 RepoDigest 的确定解析和稳定 JSON 输出。
5. 补齐流生命周期、参数注入和所有 target 类型错误矩阵。

## 验证

```bash
go test ./internal/backup -race -count=1
make check
make build
CGO_ENABLED=0 go build ./cmd/ark
git diff --check
```

## 高风险点

- 任何 target 都不能丢失 Wait，P2-4 依赖该退出状态判断截断。
- PostgreSQL 与 Redis 必须通过容器内 CLI，且 `exec -T` 不能遗漏。
- image digest 的歧义必须失败，不能退回 compose tag。
