# Release Operations

## Conclusion

Release operations exist. P2 代码与验收已完成，以下事项属于部署和环境收尾，不阻塞任务质量结论。

## Deployment

1. 部署包含 `b76f930` 及其后续提交的 ark 二进制。
2. 使用生产清单运行 `ark validate` 和 `ark doctor --all`。
3. 运行 `ark install`，随后执行 `systemctl daemon-reload`。
4. 手动启动一次 `ark-backup.service`，核对 service 结果、状态库、manifest 和 restic 快照。
5. 确认生成的 service 包含 `CacheDirectory=ark`、`CacheDirectoryMode=0700` 和
   `XDG_CACHE_HOME=/var/cache/ark`。

## External Systems

- 在生产对象存储上确认 Object Lock 或等价仅追加保留已启用，保留窗口覆盖最长 restic
  保留策略；无法自动核对时，doctor 保持 warn 是预期行为。
- 轮换真实环境验收期间使用过的 root 管理密码。
- `ark-live-validation-minio-data` 受保护至 `2026-08-13T04:31:01Z`。到期后确认它仍是
  隔离验收 volume，再执行删除；不得使用 Object Lock bypass 或直接删除后端文件。

## Rollback

- 回滚 ark 二进制后，使用旧版本重新运行 `ark install` 并执行 `systemctl daemon-reload`。
- `/var/cache/ark` 可保留；其中是 restic 缓存，不是源数据或凭证。
- 对象锁保留期内不得通过缩短保留期、bypass 或后端文件删除加速清理。

## Post-release Verification

- `systemd-analyze verify` 对 ark service/timer 无告警。
- `/var/cache/ark` 为 `root:root` 且权限 `0700`。
- 手动 backup 的全部 target、manifest 与状态库 snapshot ID 一致。
- 失败 target 不留下可用截断快照，doctor 持久化摘要不含外部命令 detail。
- 管理密码已轮换；到达保留截止时间后，隔离验证 volume 已删除。
