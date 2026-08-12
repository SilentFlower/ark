# Brief — P2-2 restic 封装

## Goal

- 建立稳定、流式、可测试且不会泄漏凭证的 `internal/restic` CLI 边界。

## Scope

- 实现 New、EnsureInit、BackupStdin、Snapshots、Forget、ForgetSnapshot、Dump 和 Check。
- 使用 JSON 输出、子进程级凭证环境、完整错误链和本地 repo 集成测试。

## Non-Goals

- 不实现 target、manifest、状态库或 backup CLI，不支持其它仓库后端。

## Key Decisions

- 禁止 `os.Setenv`/`RESTIC_PASSWORD`；Dump 的 ReadCloser 必须保留最终退出状态。
- EnsureInit 只对明确未初始化分支执行 init，不能吞掉鉴权或损坏错误。

## Key Context

- 输入模型是 `config.Repo/Retention`；命令规范来自 external-command guidelines。

## Risks / Deferred

- 当前机器没有 restic，集成测试会明确 skip，真实证据留给人工验收。

## Acceptance

- API、argv、JSON、env、流生命周期与敏感信息测试通过；make check 和无 CGO 构建全绿。

## Next Step

- 启动任务后先核对 restic JSON 与初始化错误语义，再实现命令边界。
