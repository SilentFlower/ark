# Release Operations

## Conclusion

Release operations exist.

## Evidence Checked

- `[08-18-p5-dnsmgr-dns-switch]` `task.json`、`prd.md`、`design.md`、`implement.md`
- `[08-18-p5-dnsmgr-dns-switch]` `implement.jsonl`、`check.jsonl`
- ark 提交 `d6799e3 feat: 恢复后自动切换 dnsmgr DNS`
- dnsmgr 提交 `9121e93 feat(api): 增加 Value-only DNS 更新接口`
- `README.md`、`docs/operations.md`、`examples/ark.yaml`

## Drift Check

Missing release.md. 本文件依据任务材料、提交文件和运维说明补齐。

## SQL Changes

None.

## Configuration Changes

- `[08-18-p5-dnsmgr-dns-switch]` 在运行 ark 的 hub 上创建 `/etc/ark/dnsmgr.env`，文件所有者必须与
  ark 运行用户一致，权限不得宽于 `0600`，且只允许：

  ```dotenv
  ARK_DNSMGR_UID=REDACTED
  ARK_DNSMGR_API_KEY=REDACTED
  ```

- `[08-18-p5-dnsmgr-dns-switch]` 在 `ark.yaml` 顶层增加环境对应的 `dnsmgr.base_url` 与
  `dnsmgr.env_file`；真实凭证不得进入清单、工单、日志或命令行。
- `[08-18-p5-dnsmgr-dns-switch]` 最后才在目标 host 增加 `dnsmgr.value` 与有序的
  `{domain_id, record_id}`。这些值必须从目标环境和现有 dnsmgr 记录中确认，禁止猜测。

## Batch / Deployment Scripts / Data Repair

- `[08-18-p5-dnsmgr-dns-switch]` 部署包含 `POST /api/auth/check` 与
  `POST /api/record/value/:id` 的 dnsmgr fork。
- `[08-18-p5-dnsmgr-dns-switch]` 部署包含 P5-2 的新版 ark 二进制。
- 无 SQL、数据修复或一次性批处理脚本。

## External Systems / Dependent Platforms

- `[08-18-p5-dnsmgr-dns-switch]` dnsmgr AuthApi 必须启用，UID、API key、服务时间和反向代理路径
  必须能通过 `ark doctor` 的 `dnsmgr.auth` 检查。
- `[08-18-p5-dnsmgr-dns-switch]` P5-3 完成前，真实恢复开始前必须人工暂停相关 dmonitor 检测，
  结束后无论成功失败都恢复检测。
- `[08-18-p5-dnsmgr-dns-switch]` 可回滚 DNS 测试记录由环境管理员指定；不得直接使用未授权的
  生产流量记录做首次补偿验证。

## Release Order

1. `[08-18-p5-dnsmgr-dns-switch]` 先部署或核对 dnsmgr fork 的两个 AuthApi 接口。
2. `[08-18-p5-dnsmgr-dns-switch]` 再部署新版 ark，但暂不增加 host 的 `dnsmgr` 关联。
3. `[08-18-p5-dnsmgr-dns-switch]` 执行 `ark validate` 与 `ark doctor`，确认 `dnsmgr.auth` 通过。
4. `[08-18-p5-dnsmgr-dns-switch]` 最后增加目标 host 关联，并运行普通 dry-run 与 inspect dry-run。
5. `[08-18-p5-dnsmgr-dns-switch]` 完成可回滚记录验证后，才允许真实跨机恢复使用自动 DNS。

## Rollback Notes

- `[08-18-p5-dnsmgr-dns-switch]` 先从所有 host 删除 `dnsmgr` 关联并运行 `ark validate`，阻止新的
  restore 调用 dnsmgr。
- `[08-18-p5-dnsmgr-dns-switch]` 再回滚 ark 二进制。dnsmgr 的两个加法接口可以保留；确认没有新版
  ark 调用后也可以回滚 dnsmgr 镜像。
- `[08-18-p5-dnsmgr-dns-switch]` 若结果为 `rollback_failed`，按结果中的 `domain_id/record_id`
  在 dnsmgr 人工核对当前 Value，不得无条件覆盖第三个值。

## Post-release Verification

- `[08-18-p5-dnsmgr-dns-switch]` `ark validate` 通过；`ark doctor` 的 `dnsmgr.auth` 为成功。
- `[08-18-p5-dnsmgr-dns-switch]` dry-run 与 inspect 展示 DNS 计划并进入 preview digest，同时证明
  不读取 `/etc/ark/dnsmgr.env`、不发 HTTP。
- `[08-18-p5-dnsmgr-dns-switch]` 使用可回滚 A/AAAA 记录验证 forward、幂等和带
  `expected_value` 的 compensation；完成后记录必须恢复原值。
- `[08-18-p5-dnsmgr-dns-switch]` 执行一次真实跨机恢复，核对 completion marker 先于 DNS、
  失败重跑只重试 DNS、dnsmgr 操作日志可定位且所有输出不含秘密。
- `[08-18-p5-dnsmgr-dns-switch]` 验证结束后恢复所有人工暂停的 dmonitor 检测。
