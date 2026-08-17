# Release Operations

## Conclusion

Release operations exist.

## Evidence Checked

- `task.json`
- `prd.md`
- `design.md`
- `implement.md`
- `implement.jsonl`
- `check.jsonl`
- `f2f3924 feat: 实现 ark-hub HTTP API`
- `docs/operations.md`

## Drift Check

Missing release.md. 本文件根据任务材料、业务提交和运维文档补充上线操作。

## SQL Changes

- `[08-17-p4-hub-http-api]` 状态库会在新版 `ark` 或 `ark-hub` 首次打开时由 schema v1
  自动迁移到 schema v2，新增 `manual_operations` 表和三个索引，无需人工执行 SQL。
- 升级前必须通过现有 SQLite online backup 流程保留可独立恢复的 v1 `ark.db` 副本；禁止直接
  复制运行中的 WAL 主文件，也不能把旧 `ark.db-wal`、`ark.db-shm` 作为回滚材料。
- 迁移后核对 `PRAGMA user_version=2`、`PRAGMA integrity_check=ok`，并确认
  `manual_operations` 表及其索引存在。

## Configuration Changes

- `[08-17-p4-hub-http-api]` `ark-hub.service` 的 `ExecStart` 必须显式携带绝对路径参数
  `--config /etc/ark/ark.yaml` 和 `--ark-binary /usr/local/bin/ark`。
- 上线前确认清单严格校验通过，Ark 二进制是可执行普通文件；监听地址、状态库和管理员凭证路径
  沿用当前部署值。

## Batch / Deployment Scripts / Data Repair

- 同批安装相互兼容的新版 `ark` 与 `ark-hub`，避免 Hub 启动手工任务时调用旧 CLI 契约。
- 使用当前生产参数重新运行 `ark-hub install`，检查生成的 unit 后，由管理员显式执行
  `systemctl daemon-reload` 并重启或启动 `ark-hub.service`。
- 不修改现有 backup/verify timers，不需要数据修复或一次性业务批处理。

## External Systems / Dependent Platforms

None.

## Release Order

1. 通过 SQLite online backup 保留可独立恢复的 schema v1 状态库副本。
2. 安装匹配版本的 `ark` 与 `ark-hub` 二进制。
3. 使用显式 `--config`、`--ark-binary` 参数重新生成并检查 `ark-hub.service`。
4. 执行 `systemctl daemon-reload`，启动或重启 Hub，触发并验证 schema v2 迁移。
5. 完成 API、状态库和手工操作启动后的验证。

## Rollback Notes

- schema v2 是向前迁移。旧二进制会拒绝打开高版本数据库；回滚二进制前必须停止相关进程，
  恢复升级前导出的 v1 `ark.db`，并确保目标路径没有遗留新库的 WAL/SHM 文件。
- 恢复旧版 `ark-hub.service` 参数后执行 `systemctl daemon-reload` 并重启服务。

## Post-release Verification

- 验证 `ark-hub.service` unit 和进程参数包含正确的 `--config`、`--ark-binary` 绝对路径。
- 验证 `/healthz`、登录、`GET /api/hosts`、`GET /api/runs`、`GET /api/alerts` 和
  `GET /api/operations` 返回预期状态。
- 发起一项受控 backup 或 verify 手工操作，确认 HTTP 返回 operation ID，最终状态可查询且
  `manual_operations` 中没有遗留非预期 `running` 记录。
- 验证现有 backup/verify timers 仍保持原计划且未被 Hub 安装流程修改。
