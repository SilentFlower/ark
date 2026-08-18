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
- `6777071 feat: 实现 ark-hub Web 控制台并内嵌打包`（57 文件）
- `742286f chore(task): update p4-hub-web-frontend progress`
- `Makefile`、`.gitignore`、`README.md`

## Drift Check

Missing release.md. 本文件根据任务材料与业务提交证据首次生成。

## SQL Changes

None. 状态库 schema 保持 v2，未新增表、列或索引，`internal/store` 未改动。
本次不需要执行任何 SQL，也不需要迁移前后的库结构核对。

## Configuration Changes

- `[08-17-p4-hub-web-frontend]` `ark-hub serve` / `ark-hub install` 的参数**没有变化**，
  沿用现有 `--config`、`--ark-binary`、`--state-db`、`--auth-file`、`--listen`。
  不需要修改 `ark-hub.service`，也不需要新增环境变量或凭证。
- `[08-17-p4-hub-web-frontend]` **CSP 已放宽**：从 `default-src 'none'`（禁止一切脚本）
  改为 `default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline';
  img-src 'self' data:; font-src 'self'; connect-src 'self'; form-action 'self';
  base-uri 'none'; frame-ancestors 'none'`。
  如果 Hub 部署在反向代理之后且代理自行注入或覆盖 `Content-Security-Policy`，
  需要人工确认代理不会把脚本来源收得更紧（控制台会白屏）或放得更宽（失去防护）。
- `[08-17-p4-hub-web-frontend]` 带内容 hash 的静态资源返回
  `Cache-Control: public, max-age=31536000, immutable`，且这些资源**需要登录**才能获取。
  部署在共享缓存（反向代理 / CDN）之后时，人工确认缓存不会把鉴权后的资源提供给未认证请求。
  Hub 默认只监听 `127.0.0.1`，直连部署不涉及该问题。

## Batch / Deployment Scripts / Data Repair

- `[08-17-p4-hub-web-frontend]` **发布 `ark-hub` 必须使用 `make hub`**，不能用
  `go build ./cmd/ark-hub`。前端产物不进版本库，`make hub` 会先跑
  `pnpm --dir web run build` 再同步到 `internal/hub/webui/dist` 并编译。
  直接 `go build` 得到的二进制能正常启动、API 与调度不受影响，但**页面只有一页
  「请用 make hub 构建」的占位提示**。
- `[08-17-p4-hub-web-frontend]` 构建机需要 Node 20+ 与 pnpm（本轮验证使用
  Node v22.21.1 / pnpm 10.23.0），首次构建先跑 `make web-install`。
  **部署机不需要 node**——界面随二进制分发。
- `[08-17-p4-hub-web-frontend]` 不需要数据修复、一次性批处理或后台任务重跑。

## External Systems / Dependent Platforms

None. 本次不涉及 dnsmgr、对象存储、钉钉或任何外部平台的配合上线。
`/api/hosts` 摘要只新增 `last_backup_bytes` 与 `recent_backup_sizes` 两个字段，
向后兼容，既有 API 消费方无需改动。

## Release Order

1. 在具备 Node 与 pnpm 的构建机上执行 `make hub`，产出 `bin/ark-hub`。
2. 确认产物不是占位页：`strings bin/ark-hub | grep -c 'assets/index-.*\.js'` 应大于 0。
3. 分发并替换 hub 上的 `ark-hub` 二进制。
4. 重启 `ark-hub.service`（`ark` 二进制与 timer 均无需改动）。

`ark` oneshot 二进制本轮未改动业务行为，可不同批发布；如同批发布则沿用现有流程。

## Rollback Notes

- 回滚代码即可：换回旧 `ark-hub` 二进制并重启服务。
- 无 schema 变化、无迁移、无状态残留，回滚不需要恢复状态库。
- 旧版本会以旧 CSP 与旧路由提供服务；控制台随之消失，API、登录与备份调度不受影响。

## Post-release Verification

- 浏览器打开 Hub 监听地址：未登录跳转登录页，登录后进入总览，能看到全部主机卡片、
  健康度、最近备份大小与趋势、下次计划时间。
- 浏览器控制台**无 CSP 拦截报错**（这是本次唯一实质扩大的攻击面，必须实机确认）。
- 直接访问 `/hosts/<name>` 并刷新，确认 SPA fallback 生效；退出登录后同一地址回到登录页。
- `curl -sI <hub>/api/hosts` 未带会话时返回 `401`，不返回 HTML。
- 从页面发起一次备份，确认 running → 终态可见且结果可查。
- `systemctl stop ark-hub` 后 `systemctl list-timers` 中的备份与演练 timer 不受影响
  （ADR-005 未被破坏）。
- **[上线后验证]** 在真实环境复核一次 `isolate` 恢复的完整成功路径。本轮验收在临时环境
  完成，其示例清单指向不存在的 restic 仓库，因此只验证到预检、operation 生命周期与
  结果展示；恢复执行的后端逻辑本轮未改动，由 P3 阶段实测覆盖。
  责任边界：由执行上线的管理员在有真实快照的环境中完成，确认生产资源无变化。
