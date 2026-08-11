# Release Operations

## Conclusion

Release operations exist：清单格式发生破坏性升级（`version: 1` → `version: 2`），
已部署的 `ark.yaml` 必须人工迁移，无自动迁移工具。

## Evidence Checked

- `task.json`（status=completed，无 branch / commit 字段记录）
- `prd.md`（R1–R7、Non-Goals「不提供 v1 → v2 的自动迁移工具」）
- `design.md` / `implement.md`
- `implement.jsonl` / `check.jsonl`（均只含模板示例行，无实质条目）
- git commit `ca1078d feat: 清单模型升级到多机 v2，validate 与 doctor 适配`
  改动文件：`internal/config/config.go`、`internal/config/config_test.go`、
  `internal/cli/root.go`、`internal/doctor/doctor.go`、`examples/ark.yaml`、
  `README.md`、`docs/design.md`、`docs/roadmap.md`、`.trellis/spec/backend/*`
- git commit `07e042e chore(task): update p1-manifest-v2 progress`（仅任务文档）
- `git status --porcelain`：无未提交业务文件
- 仓库内不存在 `.sql` / migration / systemd unit / 部署脚本 / Dockerfile

## Drift Check

Missing release.md（本次首次生成）。已有实现证据与 `prd.md` 一致，未发现漂移。

## SQL Changes

None。仓库内无任何 SQL、migration 或 DDL/DML 脚本。

## Configuration Changes

1. **`/etc/ark/ark.yaml` 需人工从 v1 迁移到 v2**（人工责任）
   - 顶层由 `host` / `project` / `targets` 改为 `version: 2` + `repo` + `defaults` + `hosts`。
   - 每个 host 条目新增 `local` 或 `ssh` 二选一；非 `local` 条目必须提供
     `ssh.known_hosts_file`（必填，且无跳过主机密钥校验的开关）。
   - `ssh.identity_file` / `ssh.known_hosts_file` 必须写绝对路径。
   - 参照 `examples/ark.yaml`（已重写为含一台 `local: true` 的多机清单）。
   - 迁移后执行 `ark validate -c /etc/ark/ark.yaml` 自检；仍是 v1 的清单会得到
     「检测到 v1 单机清单，请参考 examples/ark.yaml 迁移到 v2」提示而非字段级报错。

2. **`repo.url` 语义变更：不再按机器分路径**（人工责任）
   - v1 约定 URL 末尾带 `/<host>` 做隔离，v2 改为全局唯一仓库、靠 restic tag 区分 host。
   - 迁移时需把 URL 从 `.../bucket/<host>` 改回 bucket 根路径。
   - 影响范围有限：`ark backup` 尚未实现（roadmap P2-6 未完成），
     ark 自身不可能已经写入过快照，因此不存在 ark 产生的仓库数据需要搬迁。
     若目标 bucket 下已有**其它途径**创建的 restic 仓库，改 URL 前需人工确认指向。

3. **retention 继承行为变更**（无需操作，但需知悉）
   - 旧逻辑「retention 三项同时为 0 才套默认值」的启发式已删除。
   - 现在显式写 `retention: {daily: 3}` 时，`weekly` / `monthly` 就是 0，不再被补默认值。
   - 迁移清单时若依赖过旧的补值行为，需显式补全三项。

## Batch / Deployment Scripts / Data Repair

None。本次改动不含一次性命令、数据修复或定时任务触发；systemd timer 属 roadmap P2-6，尚未实现。

## External Systems / Dependent Platforms

None。本次改动不涉及网关、消息平台或第三方管理后台；SSH 执行层属 P1-2，本轮未实现。

## Release Order

无特殊顺序。清单迁移与二进制更新需同时进行：v2 二进制读不了 v1 清单（给迁移提示后退出），
v1 二进制也读不了 v2 清单。

## Rollback Notes

回滚代码即可，但需同时回滚清单：回退到 ca1078d 之前的二进制时，`/etc/ark/ark.yaml`
必须一并还原为 v1 版本。由于本轮不写入任何持久化数据（状态库属 P2-1，备份执行属 P2-3/P2-6），
回滚不涉及数据清理。

## Post-release Verification

按 `prd.md` 验收标准验证：

- `ark validate -c /etc/ark/ark.yaml` 通过，输出为逐 host 摘要。
- `ark validate -c examples/ark.yaml` 通过。
- `ark doctor` 能对多机清单跑完现有本地检查（远程检查记 warn，属 P1-3 范围，非缺陷）。
- `make check` 全绿。
