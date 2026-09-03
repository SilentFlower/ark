# Redis readiness 多行输出修复技术设计

## 1. 问题边界

本任务修复 Redis readiness 对 `sshexec.Runner.Run` 合并输出的解析，不修改命令本身、Redis 配置、Runner 接口或恢复状态机。

现有两条路径使用了相同但重复的整体精确匹配：

- `waitDatabaseReady` 在数据库 prepare/data 阶段循环执行 `redis-cli PING`。
- `waitDatabaseReadyOnce` 在读取恢复 marker 后复核数据库后置条件，决定同 Plan 是否可以跳过已完成步骤。

Runner 合并 stdout 和 stderr。只要 `redis-cli` 在成功响应前后输出警告，整个字符串就不再等于 `PONG`，即使命令退出码为 0 且 Redis 已返回成功响应。

## 2. 成功判定

新增一个包内非导出纯函数，例如：

```go
func redisPingReady(output string) bool
```

判定规则：

1. 按换行拆分合并输出。
2. 对每一行执行 `strings.TrimSpace`，兼容 LF、CRLF 和空白行。
3. 任一独立行严格等于 `PONG` 即返回 true。
4. 其它情况返回 false；不接受字段级、子串级、大小写不敏感或 RESP 形式匹配。

选择“任一独立行”而不是“最后一行”的原因是 Runner 合并 stdout/stderr 后不承诺两个流的最终排列顺序。独立整行匹配又能避免警告文本或其它内容中偶然出现 `PONG` 子串导致误判。

退出码仍由调用方先判断：

- `waitDatabaseReady` 仅在 `err == nil && redisPingReady(out)` 时返回成功。
- `waitDatabaseReadyOnce` 先原样返回 command error，再用同一 helper 校验输出。

因此输出内容不能覆盖命令失败，helper 也不接触或输出敏感文本。

## 3. 数据流与兼容性

```text
docker compose exec -T <redis> redis-cli PING
        |
        v
sshexec.Runner.Run -> 合并 stdout/stderr + error
        |
        +-- error != nil -> 现有失败/轮询路径
        |
        +-- error == nil -> redisPingReady(output)
                              |
                              +-- 独立 PONG 行 -> ready
                              +-- 其它输出     -> not ready
```

- 不修改 `restore.Result`、`StepResult` 或 verify DTO。
- 不新增配置、schema、迁移或公开 API。
- PostgreSQL `pg_isready` 仍以退出码为准，不经过 Redis helper。
- verify 不增加第二套解析；它继续通过 `restore.Execute` 继承修复。
- 现有 context 取消和轮询行为保持不变。本任务不选择新的全局 readiness timeout，避免把一次输出解析修复扩大为恢复时限策略变更。

## 4. 测试设计

### 4.1 纯函数

使用表驱动测试覆盖：

- `PONG`、首尾空白、空行、CRLF。
- AUTH 警告在 `PONG` 前或后。
- 空输出、只有警告、`pong`、`+PONG`、`PONG extra`、`warning PONG`。

### 4.2 两个调用点

- Redis `waitDatabaseReady` 使用生产形态多行输出，断言只调用一次 Runner 并立即成功。
- Redis `waitDatabaseReadyOnce` 使用同一输出，断言 marker 后置条件复核成功。
- command error 与 `PONG` 同时存在时，断言一次性复核返回原错误；循环路径继续遵守 context 取消。
- 保留现有 PostgreSQL、阶段失败和 marker 测试，证明共享 helper 未改变其它 target。

## 5. 规范与部署

更新 restore 规范，明确 Redis readiness 在成功退出后按独立响应行解析合并输出，并说明完整外部输出不得进入 Result/CLI。

本地质量门通过后使用静态构建。hub 部署必须先创建即时备份并记录 SHA-256；随后运行 validate、doctor、dnsmgr AuthApi 和 `biz` latest verify。验收同时证明生产基线一致、演练容器未连接 `api_shared`、隔离资源清理完成且 timer 正常。

若部署验证失败，立即恢复本次即时备份，只通过 isolation ID 对应的结构化 cleanup 清理可证明归属的资源，不修改生产 Redis 或共享 network 绕过失败。
