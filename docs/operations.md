# ark 运维保护与 hub 重建

本文记录 hub 自备份、离线密钥和对象锁的最低操作要求。它们是 ark 恢复能力的一部分，
不是可选加固项。

## 1. hub 自备份清单

`examples/ark.yaml` 中的 `local: true` host 必须至少覆盖：

- `/etc/ark/ark.yaml`；
- `/var/lib/ark/ark.db`；
- 明确列出的 hub 服务配置和数据；
- SSH `known_hosts` 等不含私钥、但重建连接时需要的材料。

状态库必须是只包含 `/var/lib/ark/ark.db` 的独立 `files` target。ark 会用 SQLite
online backup 导出一致的单文件副本，再把该副本流式交给 restic。运行中的 `ark.db`、
`ark.db-wal` 和 `ark.db-shm` 不能直接复制或打包，也不能在恢复时拼回旧 WAL/SHM。

不要把 `/etc/ark` 整个目录放进 targets。以下内容必须排除：

- `repo.password_file` 指向的 restic 密码文件；
- `repo.env_file` 指向的对象存储凭证文件；
- `monitoring.env_file` 指向的钉钉与外部心跳凭证文件；
- 每台机器的 SSH 私钥；
- `/var/lib/ark-hub/auth.json`；
- 应用解密密钥和其它登录或解密介质。

`ark-hub` 凭证文件是独立的 root-only 状态，不属于需要跨机器恢复的数据。新 hub 上不要从
restic 复制旧文件；完成基础恢复后，在本机 TTY 执行：

```bash
ark-hub admin init --auth-file /var/lib/ark-hub/auth.json
```

密码只从终端无回显读取，不通过命令行参数或环境变量传递。初始化后再运行
`ark-hub install` 并由管理员显式执行 daemon-reload、enable 和 start。

安装和直接启动都应显式核对清单与 Ark 二进制路径：

```bash
ark-hub install \
  --listen 127.0.0.1:8080 \
  --state-db /var/lib/ark/ark.db \
  --auth-file /var/lib/ark-hub/auth.json \
  --config /etc/ark/ark.yaml \
  --ark-binary /usr/local/bin/ark
```

生成的 `ark-hub.service` 会携带相同参数。清单或 Ark 二进制不是绝对路径、清单严格校验失败、
二进制不是可执行普通文件时，Hub 必须在监听前失败；不要依赖 service 的工作目录或 PATH。

当前状态库 schema v3 包含 `manual_operations` 与跨重启生效的 `alert_states`。Hub 正常停止时会取消当前手工
Ark 子进程并写入 `interrupted`；若进程被强制终止，下次启动也会在监听前把遗留 running 记录
原子改为 interrupted。该状态表示操作未证明完成，恢复类操作必须重新预检，不能直接重放旧确认。

## 2. 主动告警与外部心跳

在清单中只保存秘密文件路径：

```yaml
monitoring:
  env_file: /etc/ark/monitoring.env
```

创建秘密文件后设置 `0600`，文件所有者必须与运行 `ark`、`ark-hub` 的用户一致：

```bash
install -m 0600 /dev/null /etc/ark/monitoring.env
```

允许的键只有 `ARK_DINGTALK_WEBHOOK_URL`、`ARK_DINGTALK_SECRET`、
`ARK_HEARTBEAT_SUCCESS_URL`、`ARK_HEARTBEAT_FAILURE_URL`。心跳成功/失败 URL 必须同时配置，
可以相同。默认只允许 HTTPS；HTTP 只供 loopback 本机代理与测试使用。不要在工单、日志或命令行
粘贴完整 URL，因为 query 中通常就是认证材料。

变更后依次执行 `ark validate` 和 `ark doctor --all`。Hub 启动时会 fail closed 校验秘密文件；
backup 读取失败时仍完成真实备份，但摘要显示 `heartbeat_status=failed`，由外部监控在宽限期后报警。
钉钉持续故障每 24 小时最多重发一次，恢复只发送一次。网络发送成功但状态提交前进程退出时，
可能重复一条通知，这是避免永久漏报所接受的 at-least-once 行为。

上线验收必须在真机完成：制造连续两次备份失败，确认页面与钉钉在一分钟内出现同一 kind；
静默期内确认不重复发送。再停止 `ark-hub`，确认 backup timer 和成功心跳仍执行；恢复 Hub 后停止
backup timer，确认外部监控按配置的宽限期报告失联。任何测试结束后都要恢复 timer 和故障注入。

## 3. 离线恢复材料

至少在 hub 之外保存两份恢复材料，并定期确认仍能读取：

1. restic 仓库密码；
2. 对象存储访问凭证或重新签发凭证的管理员入口；
3. 每台目标机的 SSH 私钥；
4. 应用自身的解密密钥；
5. ark-hub 管理员密码或可重新设定该密码的团队流程；
6. ark 版本、对象存储地址和恢复负责人信息。

推荐一份放在团队密码管理器，另一份放在受控离线介质。离线介质不能和 hub 放在同一
故障域，也不能只保存“获取密钥的方法”而没有验证该方法实际可用。

## 4. 对象锁与保留期

在对象存储控制台为 restic bucket 开启 Object Lock、Immutability 或等价的仅追加保留。
ark 当前没有 provider-neutral 的自动探测能力，因此 `ark doctor` 会固定输出
`repo.object_lock` 的 `warn`，提醒人工核对；这条告警不阻断备份，也不代表已验证成功。

对象锁保留期应覆盖最长的 restic 保留窗口，通常至少与月备保留时长对齐。对象仍在锁定
期内时，`restic forget --prune` 可能无法删除或回收空间，这是预期行为。容量规划必须按
对象锁保留期计算，不能把 prune 暂时失败误判为仓库损坏后关闭保护。

## 5. SSH 主机密钥维护

每台远程主机都必须配置持久化的 `known_hosts_file`。默认 `host_key_policy: accept-new`
允许 OpenSSH 在第一次连接时记录主机密钥，以降低新环境接入成本；一旦该主机已有记录，
后续密钥变化仍会被拒绝。需要预先建立信任的环境可显式改为 `strict`。

主机重装或密钥轮换时按以下顺序处理：

1. 运行 `ark host-key refresh --host <name>`，只预览已记录指纹和当前扫描指纹；
2. 通过云控制台、服务器本地终端或其它独立通道核对 SHA256 指纹；
3. 确认一致后运行 `ark host-key refresh --host <name> --apply`；
4. 再运行 `ark doctor --host <name>` 验证连接和目标环境。

`ssh-keyscan` 只收集公开主机密钥，不会使用账号密码或 SSH 私钥，也不能证明远端身份。
因此刷新命令默认不写文件，`--apply` 必须由管理员在带外核对后显式给出。任何场景都不要
改成 `StrictHostKeyChecking=no`；这会让首次连接和后续密钥变化都失去保护。

## 6. 定期人工验证

建议每月至少执行一次：

1. 在控制台确认对象锁仍启用，默认保留期与 ark 的长期保留策略一致；
2. 选择一个仍在保留期内的测试对象，用 hub 当前写入凭证尝试删除并确认被拒绝；
3. 从离线介质取得 restic 密码，在隔离环境执行 `restic snapshots`；
4. 恢复最新 `ark-state` 快照，只使用导出的 `ark.db`，运行 `PRAGMA integrity_check`；
5. 核对清单、known_hosts、SSH 私钥和应用解密密钥均能从各自规定位置取得。

真实对象删除测试和控制台配置属于人工上线验收，不在自动测试或 auto-loop 中执行。

## 7. hub 重建顺序

1. 在新机器安装与清单匹配的 ark 和 restic；
2. 从离线介质恢复 restic 密码和对象存储凭证；
3. 从 restic 恢复 `/etc/ark/ark.yaml`、known_hosts、hub 服务数据和导出的 `ark.db`；
4. 确认目标路径没有遗留旧 `ark.db-wal`、`ark.db-shm`，再放置导出的 `ark.db`；
5. 从离线介质恢复 SSH 私钥和应用解密密钥，并设置 `0600` 权限；
6. 在本机 TTY 执行 `ark-hub admin init`，重新创建 `/var/lib/ark-hub/auth.json`；
7. 运行 `ark validate`、`ark doctor --all`，人工确认对象锁告警后再恢复定时任务；
8. 运行 `ark-hub install`，检查 unit 后再显式 daemon-reload、enable 和 start。
