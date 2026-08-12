# P2-7 hub 自备份保护

## Goal

让 hub 的清单、SQLite 状态和必要服务数据进入备份，同时明确排除所有解密/登录密钥，
并把对象锁这一人工安全前提变成持续可见的 doctor 与运维契约。

## Requirements

### R1 Hub 清单示例

- 更新 `examples/ark.yaml`，保留 `local: true` hub 条目，覆盖 `/etc/ark/ark.yaml`、
  `/var/lib/ark/ark.db` 和用户明确配置的其它 hub 服务数据。
- 示例和文档必须明确排除 repo password、repo env、SSH 私钥、known_hosts 之外的密钥介质；
  不提供“备份所有 /etc/ark”这类会误收密钥的宽泛路径。
- hub target 使用稳定、可恢复到原路径的标识。

### R2 SQLite 在线一致性

- 禁止运行中只 tar/copy `ark.db` 主文件；`-wal` 与 `-shm` 不能靠碰运气组合复制。
- 为状态库增加受控的在线一致性导出边界，并由 hub 自备份调用；具体 modernc/SQLite API
  在实现阶段用真实测试确定，但结果必须是可独立恢复的完整数据库。
- 临时产物如确有必要必须位于 `0700` 临时目录、文件 `0600`、成功/失败都清理，
  且不得让大业务 target 退回“先落盘再上传”的模式。
- 导出期间仍需保留 ark 写、未来 ark-hub 读的并发语义，不能长期停库。

### R3 对象锁提醒

- doctor hub 本地检查新增对象锁/immutability 人工确认项。
- 当前清单没有 provider-neutral 的对象锁探测配置；无法自动核实时返回 warn，
  明确提示在控制台确认保留期与 restic 长期保留策略对齐。
- warn 不阻断备份，也不能伪装成已验证的 ok。

### R4 运维文档

- 记录离线保存 restic 密码与 SSH 私钥、对象锁启用、恢复材料清单和定期人工验证步骤。
- 记录对象锁可能让 `forget --prune` 在保留期内无法回收空间的预期行为。
- 记录状态库恢复只恢复一致性导出文件，不拼接旧 WAL/SHM。

## Non-Goals

- 不在 auto-loop 中登录对象存储控制台、修改真实 bucket 或删除对象测试锁。
- 不备份或上传任何真实凭证内容。
- 不实现 provider-specific S3/R2 对象锁 API。

## Acceptance Criteria

- [x] 示例 hub 条目覆盖清单和一致性状态库，同时明确排除密码与私钥
- [x] 并发写入 WAL 数据库时导出的副本通过 integrity_check 且包含已提交数据
- [x] 导出失败、取消和清理错误均可见，不残留宽权限临时文件
- [x] doctor 在无法自动核对对象锁时稳定输出 warn 而非 ok/fail
- [x] 运维文档包含离线密钥、对象锁、空间回收和 hub 重建材料
- [x] make check、构建、无 CGO 构建和 git diff 检查通过
