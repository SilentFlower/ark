# 技术设计：P2-2 restic 封装

## 1. 包边界

`internal/restic` 持有 `config.Repo` 的值副本、受限凭证环境和非导出的命令构造入口。
生产代码使用 `exec.CommandContext`；测试通过非导出的 command factory 捕获 argv、env、
stdin、stdout 和退出状态，不新增只为 mock 服务的公开接口。

## 2. 命令模型

- 所有命令由 argv 切片构造，不调用 shell。
- 公共环境先复制 `os.Environ()`，再追加受限 env 文件值，最后由清单强制覆盖
  `RESTIC_REPOSITORY` 和 `RESTIC_PASSWORD_FILE`。
- 错误包装使用脱敏后的 argv；stderr 先按潜在敏感信息处理，不能无条件回显。

## 3. JSON 与流

- `BackupStdin` 解析 restic JSON message 流，提取最终 snapshot ID 和可验证统计。
- `Snapshots` 解析 JSON 数组，并在 Go 层保留稳定排序语义。
- `Dump` 返回一个拥有子进程生命周期的 ReadCloser：读到 EOF 后等待进程，提前关闭时
  关闭管道并回收进程，Wait 错误不能静默丢失。

## 4. 初始化与回滚

- `EnsureInit` 先用只读命令确认仓库配置；仅对明确的“仓库不存在”分支执行 init。
- init 失败不删除仓库路径，也不重试为成功。
- `ForgetSnapshot` 失败必须上抛，调用方不能把坏快照撤销失败伪装成 target 成功。

## 5. 测试

单元测试使用可控假命令覆盖所有失败路径；真实 restic 集成测试使用 `t.TempDir()`，
短模式或二进制缺失时明确 skip，并在非短模式验证完整生命周期。
