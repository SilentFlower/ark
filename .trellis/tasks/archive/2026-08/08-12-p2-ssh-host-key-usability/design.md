# 技术设计：P2 SSH 主机密钥易用性

## 1. 跨层数据流

```text
ark.yaml ssh.host_key_policy
  -> config.Load / Validate / effective policy
  -> doctor known_hosts 前置检查
  -> sshexec Runner 的 StrictHostKeyChecking 参数

ark host-key refresh --host <name>
  -> config 选择远程 host
  -> hostkey 扫描与指纹比较
  -> 默认预览 / --apply 原子更新 known_hosts
```

配置类型和默认值由 `internal/config` 唯一持有；Runner、doctor 和 CLI 只能消费生效策略，不能分别解释空字符串。

## 2. 配置模型

- 新增 `SSHHostKeyPolicy` 字符串类型及 `accept-new`、`strict` 常量。
- 空值通过统一访问器解析为默认 `accept-new`，不在 YAML 加载后改写用户结构。
- `validateSSH` 接受空值和两个合法值，拒绝其它字符串；`known_hosts_file` 继续要求绝对路径。
- schema 版本保持 2，因为这是向后兼容的可选字段；显式 `strict` 可恢复旧行为。

## 3. Runner 参数

`sshRunner` 在构造时保存已经归一化的 OpenSSH 策略值：

| 清单策略 | OpenSSH 参数 |
| --- | --- |
| 默认 / `accept-new` | `StrictHostKeyChecking=accept-new` |
| `strict` | `StrictHostKeyChecking=yes` |

参数继续由唯一的 `build` 方法生成，确保 `Run`、`Stream`、`Feed` 不漂移。任何分支都不得生成 `StrictHostKeyChecking=no`。

## 4. 主机密钥刷新边界

新增 `internal/hostkey` 包，负责扫描、指纹投影和原子更新；CLI 只负责参数、host 选择和输出。

刷新流程：

1. 从清单解析 host、port 和 `known_hosts_file`。
2. 用 `ssh-keygen -F <host-token> -f <file>` 读取该 host 已记录的键；文件不存在时旧集合为空。
3. 用 `ssh-keyscan -T <seconds> -p <port> <host>` 获取远端当前公开键。
4. 用 `ssh-keygen -lf -` 生成 SHA256 指纹，排序去重后输出旧、新集合。
5. 默认停止，不写文件。
6. `--apply` 时在目标目录创建 `0600` 临时副本，对临时副本执行 `ssh-keygen -R`，追加扫描结果，sync 后原子 rename。

目标文件存在时必须通过 `Lstat` 确认为普通文件且不是符号链接；成功更新后的权限固定为 `0600`。临时文件、`ssh-keygen` 生成的 `.old` 和失败产物始终清理。原文件直到最终 rename 前保持不变；rename 后目录 sync 失败时，恢复原内容和权限，首次创建则删除新文件，并再次同步目录。若回滚本身也失败，合并返回原始错误与回滚错误。

`ssh-keyscan` 只观察当前网络端点，不能认证服务器；预览输出和文档必须提示操作者通过云控制台或服务器本地指纹核对。`--apply` 是显式信任动作，不由 backup、doctor 或 systemd 自动调用。

## 5. Doctor

doctor 按生效策略检查 known_hosts：

- `strict`：沿用现有普通文件存在性检查。
- `accept-new` 且文件存在：检查为普通文件。
- `accept-new` 且文件不存在：检查父目录存在、是目录且当前进程可写；满足时 warn，否则 fail。

doctor 不创建目录或文件，避免环境检查产生副作用。

## 6. CLI 与错误

- 根命令新增 `host-key` 组和 `refresh` 子命令。
- `--host` 必填，`--apply` 默认 false；不引入交互式 stdin prompt，便于审计和自动化。
- 未知 host、local host、扫描为空、工具缺失、超时和写入失败均返回中文错误并保留 `%w`。
- 输出只包含逻辑 host、地址、文件路径、算法和 SHA256 指纹，不输出 known_hosts 原始行、私钥或环境变量。

## 7. 测试策略

- config：默认值、strict、非法值、错误路径和 schema v2 往返。
- sshexec：两种策略的完整 argv；三种 Runner 路径共享 build；全仓搜索无 `StrictHostKeyChecking=no`。
- hostkey：旧键为空/存在/哈希记录、扫描为空、工具失败、超时、符号链接、权限、原子替换、回滚和临时清理。
- CLI：host 选择、local 拒绝、预览零写入、`--apply` 调用和输出脱敏。
- doctor：strict 缺文件、accept-new 首次连接 warn、父目录失败和已有文件通过。
