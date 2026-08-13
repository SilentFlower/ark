# 执行计划：P3-2A 隔离恢复命名与端口映射

## 步骤

1. 扩展 restore Plan/Result 的可选 isolation 纯数据模型，增加稳定 ID、项目名与路径派生函数；保持普通
   Plan JSON 和 dry-run 契约兼容。
2. 扩展 restore CLI：增加 `--isolate`、与 `--force` 的参数互斥、隔离 dry-run 输出及 Result 的
   isolation/port/cleanup 信息。
3. 实现 isolation root、files/raw-file 路径映射和 root-only schema 状态；让 files 阶段只写专用根。
4. 在 files 后实现 `isolation_prepare`：调用 Compose canonical JSON，解析并验证支持矩阵，结构化改写
   project/container/volume/network/path/port/label，生成隔离 Compose 配置和 effective Plan。
5. 接入具体 host IP 校验、Docker 原子端口分配、启动后 inspect 与实际映射持久化；补中断后重建映射。
6. 让现有 image digest、volume、database、application、health 和 marker 使用 effective isolated Plan，
   并证明隔离模式不会进入 force/safety-backup/stop 原项目路径。
7. 实现 label/path 双重校验的 cleanup API 和
   `ark restore cleanup --host <destination> --isolation <id> [--json]`，支持部分失败幂等续跑。
8. 补纯函数、fake Runner、CLI 和可选 Docker 集成测试，覆盖端口语法、TCP/UDP、固定名称、路径映射、
   unsupported matrix、脱敏、续跑和 cleanup。
9. 更新 restore Plan、真实 restore、外部命令规范及 P3 verify 复用边界，运行全量质量门禁。

## 重点文件

- `internal/restore/plan.go`、`plan_test.go`
- `internal/restore/execute.go`、`execute_test.go`
- 新增的 `internal/restore/isolation*.go` 与对应测试
- `internal/cli/restore.go`、`restore_test.go`
- `.trellis/spec/backend/restore-plan-guidelines.md`
- `.trellis/spec/backend/external-command-guidelines.md`

## 验证命令

```bash
go test ./internal/restore ./internal/cli -race -count=1
go test ./internal/restore -run Isolation -count=1
make check
make build
CGO_ENABLED=0 go build ./cmd/ark
git diff --check
```

可选真实 Docker 集成测试只使用唯一隔离 project 和临时目录，测试结束通过被测 cleanup API 回收；
测试失败时输出 isolation ID，不运行宽泛删除命令。

## 风险与回滚点

- Compose 转换和 Executor 接入先保持可选字段门控；任何回归可通过不传 `--isolate` 回到原位路径。
- 不使用 YAML 文本替换；转换模型或 unsupported matrix 不完整时先 fail closed，不扩大 MVP。
- cleanup 完成前不删除 state；生成配置和 canonical 内容不得进入测试失败输出、JSON 或错误链。
- 修改阶段顺序时，普通 Plan 不增加 isolation phase，避免破坏既有 marker 与续跑身份。

## 启动前检查

- 三件套无 Open Questions，最新 Brief 已展示并由用户确认。
- `trellis-before-dev` 已加载 restore、database 和 external-command 相关规范。
- 实现路由已由 `trellis-route(target=implement)` 决定。
