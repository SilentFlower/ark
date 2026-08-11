# 执行计划

## 步骤

1. **同步 roadmap 与依赖基线**
   - 保留已补齐的 P1-2 完成标记
   - 引入 `modernc.org/sqlite v1.36.1`，检查 go.mod/go.sum 未升级 Go 版本

2. **建立 `internal/store` 连接边界**
   - 新增 package doc、`DefaultPath`、`Store`、`Open`、`Close`
   - 创建目录/文件权限，使用结构化 file URI
   - 配置并验证 WAL、foreign key、busy timeout

3. **实现 schema v1 与顺序迁移**
   - 新增嵌入式 `schema.sql`
   - 创建四张表、CHECK、外键和索引
   - 用单连接 `BEGIN IMMEDIATE` + `PRAGMA user_version` 实现并发安全迁移
   - 拒绝高于当前程序版本的数据库

4. **实现最小状态读写 API**
   - 定义 Status、Run、RunResult、RunTarget、DoctorReport、Verification
   - 实现 run 创建/查询/完成、target 结果写入、最近成功 bytes 查询
   - 实现 doctor report 与 verification 追加
   - 补齐字段、状态、非负数和 JSON 校验

5. **补齐高风险测试**
   - 首次建库、重复打开、文件权限、schema/索引/PRAGMA
   - CRUD、无历史结果、外键级联、非法字段、较新 schema
   - 两个 Store 并发读写、多个 Open 并发迁移
   - 所有测试使用 `t.TempDir()`，不依赖系统 sqlite3 命令

6. **同步规范与验证**
   - 更新 `database-guidelines.md`：区分 ark 自身 SQLite 与被备份的业务数据库
   - P2-1 完成后更新 roadmap 状态，不改 P2-3 的 WAL 一致性备份边界
   - 运行完整检查与无 cgo 构建验证

## 验证命令

```bash
go test ./internal/store -race -count=1
CGO_ENABLED=0 go test ./internal/store -count=1
make check
make build
CGO_ENABLED=0 go build ./cmd/ark
git diff --check
```

## 高风险文件与回滚点

- `internal/store/schema.sql`：表字段和约束会成为后续 P2/P3/P4 的持久化契约；重点反查
  multi-host run 语义、target 唯一键和 P2-4 bytes 查询。
- `internal/store/store.go`：迁移加锁、连接级 PRAGMA 或错误链错误会造成偶发锁失败、
  半套 schema 或“写入成功”的假象。
- `go.mod` / `go.sum`：只能引入 Go 1.22 兼容的 v1.36.1，不能被工具自动抬高 go 指令。
- `.trellis/spec/backend/database-guidelines.md`：当前前提已过期，但业务数据库备份规则仍有效，
  更新时不能整篇替换掉既有 pg_dump/Redis 约束。

## 提交前检查

- 所有导出类型和方法都有中文 Javadoc/Docstring 风格注释，参数与返回语义明确。
- 迁移不按字符串分号拆 SQL，不在失败时删除或降级现有数据库。
- `Open` 的每个失败分支都关闭已创建的 `*sql.DB`。
- `RowsAffected`、`rows.Err()`、`Close()` 和 rollback 错误均未静默吞掉。
- 两个独立 Store 的并发测试确实使用同一磁盘文件，不是共享单连接造成的假通过。
- 数据库错误、doctor JSON 和 verification JSON 不包含凭证内容。
- WAL 在线备份问题只记录为 P2-3 风险，没有在本任务引入 files target 特判。
