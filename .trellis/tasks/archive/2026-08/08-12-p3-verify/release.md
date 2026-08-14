# Release Operations

## Conclusion

Release operations exist. P3-5 增加新的 `ark verify` 命令以及每周执行的
`ark-verify.service` / `ark-verify.timer`，部署新二进制后必须重新安装并启用受管 units。

## Evidence Checked

- `[08-12-p3-verify]` `task.json`、`prd.md`、`design.md`、`implement.md`
- `[08-12-p3-verify]` `implement.jsonl`、`check.jsonl`
- `[08-12-p3-verify]` 业务提交 `6898e89` 与任务记录提交 `680fe8b`
- `[08-12-p3-verify]` `internal/cli/install.go`、`internal/systemd/unit.go` 与相关测试

## Drift Check

Missing release.md. 本文件依据已完成任务材料与已推送提交补充。

## SQL Changes

None. 本任务复用现有 `verifications` 表，不修改数据库 schema。

## Configuration Changes

- `[08-12-p3-verify]` 不需要新增或修改 `ark.yaml` 字段。
- `[08-12-p3-verify]` verify 首版固定每周执行，暂不提供自定义 schedule 配置。

## Batch / Deployment Scripts / Data Repair

- `[08-12-p3-verify]` 部署新 ark 二进制后，使用生产清单运行
  `ark --config /etc/ark/ark.yaml validate`、`ark --config /etc/ark/ark.yaml doctor --all` 和
  `ark --config /etc/ark/ark.yaml install`。
- `[08-12-p3-verify]` `ark install` 只原子写入并校验 unit 文件，不执行 systemd reload 或 enable；
  随后必须运行 `systemctl daemon-reload` 和 `systemctl enable --now ark-verify.timer`。

## External Systems / Dependent Platforms

None. verify 不修改 DNS、TLS、dnsmgr、防火墙或生产应用配置。

## Release Order

1. 部署包含提交 `6898e89` 的 ark 二进制。
2. 使用生产清单运行 `ark validate` 与 `ark doctor --all`。
3. 运行 `ark install`，确认输出包含 `ark-verify.service` 与 `ark-verify.timer`。
4. 运行 `systemctl daemon-reload`。
5. 运行 `systemctl enable --now ark-verify.timer`。
6. 手工启动一次 `ark-verify.service`，确认完整演练成功后再依赖每周 timer。

## Rollback Notes

- 回滚前运行 `systemctl disable --now ark-verify.timer`。
- 确认 `/etc/systemd/system/ark-verify.service` 与 `ark-verify.timer` 都带
  `# Managed by ark; DO NOT EDIT.` 标记后删除这两个 unit。
- 恢复旧版 ark 二进制，使用旧版重新运行 `ark install`，然后执行 `systemctl daemon-reload`。
- 已写入的 verification 历史可保留；旧版不会使用这些记录，且无需回滚数据库 schema。

## Post-release Verification

- 对 `/etc/systemd/system/ark-verify.service` 和 `ark-verify.timer` 运行
  `systemd-analyze verify`，确认无错误。
- 确认 `systemctl is-enabled ark-verify.timer` 返回 `enabled`，timer 为 `active`，并显示下次触发时间。
- 手工运行 `systemctl start ark-verify.service`，确认 service 的 `Result=success`、`ExecMainStatus=0`。
- 确认每台 host 均记录 Verification，演练容器不发布宿主机端口，成功后无隔离资源残留，
  且生产容器、镜像、network、volume 与声明文件基线未变化。
