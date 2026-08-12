# 执行计划

## 步骤

1. 在 `internal/backup/manifest.go` 增加可返回精确 snapshot 元数据的 manifest 选择入口，
   让现有 latest API 复用它并补齐单测与规范。
2. 新建 `internal/restore/plan.go`，定义 Plan/Step/Phase 值类型、映射校验和稳定排序。
3. 为 source/destination Project 与 Target 精确兼容实现纯比较和聚合错误，不读取环境。
4. 新建 `internal/cli/restore.go`，接入 `--host`、`--to`、`--snapshot`、`--dry-run`、`--json`。
5. 将 restore 命令注册到 `internal/cli/root.go`，保持根命令退出与输出语义。
6. 增加 manifest 选择、Plan 构建、人类/JSON 输出和 dry-run 零副作用测试。
7. 同步 `backup-manifest-guidelines.md` 与必要的 restore 规范条目。
8. 运行目标包、race、全仓、构建、无 CGO 与 diff 门禁。

## 验证命令

```bash
go test ./internal/restore ./internal/backup ./internal/cli -race -count=1
make check
make build
CGO_ENABLED=0 go build ./cmd/ark
git diff --check
```

## 风险与回滚点

- manifest 读取入口必须保留现有公开签名，避免破坏 P2 调用方。
- 不得让 dry-run 为了“检查目标状态”创建 Runner；目标环境检查属于 P3-2。
- 如果 Plan 字段无法由 manifest/config 明确得到，应扩充 Plan 构建错误而不是执行时猜测。

## 完成前检查

- `implement.jsonl` 与 `check.jsonl` 只有真实规范/设计来源，无 seed 行。
- `prd.md` 没有 Open Questions。
- Plan 输出与真实执行将共用同一结构化字段，不输出 shell 命令或密钥内容。
