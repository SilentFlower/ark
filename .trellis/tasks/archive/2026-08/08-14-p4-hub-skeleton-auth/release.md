# Release Operations

## Conclusion

Release operations exist. 本任务新增 `ark-hub` 二进制、管理员凭证文件和独立的 systemd service，部署时需要显式初始化与启用。

## Evidence Checked

- `task.json`、`prd.md`、`design.md`、`implement.md`、`implement.jsonl`、`check.jsonl`
- Git commits `178069b` and `640f28e`
- `docs/operations.md`、`internal/hub/install.go`、`internal/systemd/unit.go`

## Drift Check

Missing `release.md`; this file records the deployment and verification steps found in the task and Git evidence.

## SQL Changes

None.

## Configuration Changes

- 在目标机本地 TTY 执行 `ark-hub admin init --auth-file /var/lib/ark-hub/auth.json`，创建 root-only 管理员凭证。密码不得通过命令行参数或环境变量传递。
- 默认监听 `127.0.0.1:8080`，状态库为 `/var/lib/ark/ark.db`，凭证文件为 `/var/lib/ark-hub/auth.json`。需要修改时，通过 `ark-hub install` 的显式参数写入 service。
- 仅在 HTTPS 已由反向代理终止、浏览器实际通过 HTTPS 访问时，为 `ark-hub install` 增加 `--secure-cookie`。

## Batch / Deployment Scripts / Data Repair

- 将新的 `ark-hub` 二进制部署到稳定的绝对路径。
- 运行 `ark-hub install` 生成并原子安装 `/etc/systemd/system/ark-hub.service`。该命令不会执行 daemon-reload、enable 或 start，也不会创建或修改 ark 的 backup/verify timer。

## External Systems / Dependent Platforms

- 默认 loopback 部署无需外部系统变更。
- 若需要跨主机访问，应由受控反向代理提供 TLS，并显式配置监听地址与 `--secure-cookie`；不要依赖 forwarding header 自动推断安全属性。

## Release Order

1. 部署新的 `ark-hub` 二进制，并确认 `/var/lib/ark/ark.db` 可由服务账户读取。
2. 在目标机本地 TTY 初始化 `/var/lib/ark-hub/auth.json`。
3. 运行带有目标监听、状态库、凭证文件和 Cookie 参数的 `ark-hub install`。
4. 检查 `/etc/systemd/system/ark-hub.service`，再执行 `systemctl daemon-reload` 和 `systemctl enable --now ark-hub.service`。
5. 完成健康检查、未鉴权拦截和管理员登录验证。

## Rollback Notes

- 停止并禁用 `ark-hub.service`，回退 `ark-hub` 二进制和对应 service 内容后，再执行 `systemctl daemon-reload`。
- 回滚不得删除或修改现有 ark backup/verify timer。
- 默认保留 `/var/lib/ark-hub/auth.json`；只有明确需要重置管理员身份时，才通过 `ark-hub admin reset` 或重新初始化流程处理。

## Post-release Verification

- `systemctl status ark-hub.service` 显示服务正常运行，`GET /healthz` 返回成功。
- 未携带会话访问 `GET /api/session` 返回 `401`，浏览器可通过 `/login` 完成管理员登录。
- 确认 `ark-hub.service` 使用预期监听地址、状态库、凭证文件及 `--secure-cookie` 参数。
- 确认现有 backup/verify service 与 timer 未被创建、修改、停止或禁用。
