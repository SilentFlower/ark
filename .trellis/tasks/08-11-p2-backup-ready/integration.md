# P2 最终集成复核

## 结论

- 复核日期：2026-08-12（Asia/Shanghai）
- 结果：PASS
- 范围：P2-2 至 P2-8、实机流生命周期修复、真实环境最终验收
- 业务代码：本父任务未修改业务代码；未发现需要重开子任务的新缺陷

## 子任务完成链

| 子任务 | 交付提交 | 归档提交 | 质量证据 |
|---|---|---|---|
| `p2-restic` | `8421ea6` | `a9537cc` | auto-loop Full Check-All PASS |
| `p2-target-executors` | `ed9a8ac` | `8b54103` | auto-loop Full Check-All PASS |
| `p2-stream-integrity` | `62d9e4e` | `8e8cd10` | auto-loop Full Check-All PASS |
| `p2-manifest` | `717e9e8` | `5d751ec` | auto-loop Full Check-All PASS |
| `p2-backup-cli` | `c33dbbe` | `185cf60` | auto-loop Full Check-All PASS |
| `p2-hub-protection` | `e7d1700` | `215588b` | auto-loop Full Check-All PASS |
| `p2-ssh-host-key-usability` | `2ff5a01` | `548bb54` | Full Check-All PASS |
| `p2-stream-close-lifecycle` | `b76f930` | `4f9c40e` | Full Check-All 与实机复验 PASS |
| `p2-live-validation` | `f19fcdc` | `ad1e382` | 隔离实机场景表全部 PASS |

上述提交均可从当前 `main` 到达；九个子任务的 `task.json` 均为 `completed`，并已归档。

## 数据主链复核

1. `config.Target` 通过本地或 SSH `sshexec.Runner` 创建纯 stdout 数据流，参数按 argv
   传递，不经过额外 shell。
2. backup executor 生成跨运行稳定的 `<host>/<target-id><suffix>` filename；restic 快照
   使用稳定 `host:`、`target:`、`run:` 标签。
3. `restic.BackupStdin` 保留已解析的 snapshot ID。成功路径按 Read EOF -> Wait -> Close
   回收；restic 提前失败路径按 Close -> Wait 回收，避免上游阻塞。
4. 上游、restic 或 manifest 保存失败且返回 snapshot ID 时，调用方精确撤销该快照；
   撤销错误与原错误并列保留，不会静默丢失。
5. `store.RunTarget` 与 manifest 均记录 run、host、target、status、bytes、duration、
   snapshot ID 和脱敏错误，可按 run/tag 双向追溯。
6. restic 仓库位置和密码文件由清单覆盖父进程环境；密码、SSH 私钥和外部命令详情
   不进入备份清单、systemd unit 或持久化错误摘要。

## 验证结果

- `make check`：PASS
- `make build`：PASS
- `CGO_ENABLED=0 go build -o /tmp/ark-p2-parent-static ./cmd/ark`：PASS，静态 ELF
- `go test ./internal/backup ./internal/restic ./internal/sshexec ./internal/systemd ./internal/cli -race -count=10`：PASS
- 官方 restic `0.19.1` 发布包 SHA256 校验：PASS
- `TestRepoIntegration_LocalLifecycle`：PASS，覆盖 init -> backup -> snapshots -> forget -> check
- `git diff --check`：PASS
- 真实 Debian 10 / systemd 241 / restic 0.19.1 隔离环境场景：PASS，详见已归档
  `p2-live-validation/validation.md`

## 上线后续

- 生产部署、systemd unit 刷新、管理密码轮换和隔离验证 volume 清理由 `release.md` 记录。
- Object Lock 保留截止时间是 `2026-08-13T04:31:01Z`；到期前不得绕过保留策略，
  到期后才可删除 `ark-live-validation-minio-data`。
