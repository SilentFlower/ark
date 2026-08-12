# 技术设计：P2-7 hub 自备份保护

## 1. 状态库导出边界

在线导出能力由 `internal/store` 持有，因为只有 store 能正确协调 WAL、连接和一致性。
公开 API 接受 context 与目标 writer/受控目标，不暴露 `*sql.DB`。实现前用 modernc v1.36.1
验证 SQLite backup API、受控 checkpoint/锁或安全临时副本方案，选择最小可验证实现。

选择必须满足：导出副本可独立打开、`integrity_check=ok`、不依赖同时复制 WAL/SHM、
context 可取消、临时文件权限正确且完整清理。不得把普通 files/volume target 改为落盘模式。

## 2. Backup 接入

hub local 的 `ark.db` 路径不能走普通 files tar。backup 编排在识别该受保护路径时调用
store 在线导出，再以稳定 filename 交给 restic；其它 files path 仍走 P2-3 tar 流。
检测必须精确匹配清理后的绝对路径，不能把任意同名文件当成状态库。

## 3. 对象锁检查

doctor 增加固定人工检查项。没有 provider-neutral 证据时状态为 warn，detail 只说明需要
在对象存储控制台核对，不打印仓库凭证。未来若增加 provider API，再单独扩展 config/spec。

## 4. 测试

使用真实临时 SQLite WAL 文件并发写入，导出后以独立连接运行 integrity/foreign_key 检查。
示例与文档测试断言敏感路径没有进入 targets，doctor 测试断言 warn 语义和 JSON 输出。
