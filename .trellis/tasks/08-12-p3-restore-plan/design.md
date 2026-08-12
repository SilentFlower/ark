# 技术设计：恢复 Plan

## 1. 边界

`internal/restore` 只接收已校验的 manifest/config 值并构造纯数据 Plan，不加载文件、不创建
restic repo、不连接目标机。`internal/cli/restore.go` 负责选择 manifest、source、destination，
调用 Plan 构建并输出。

manifest 的选择与解码继续由 `internal/backup` 持有，避免 restore 复制 wire schema。新增读取入口
返回 `backup.Manifest` 与对应 `restic.Snapshot`；现有 `LoadLatestManifest` 保持兼容并委托新入口。

## 2. 映射流程

```text
config.LoadAndValidate
  + backup.LoadManifestSelection(latest|id)
  -> select manifest source host
  -> select config source/destination host
  -> validate exact Project/Target compatibility
  -> map manifest TargetResult by ID + type
  -> build stable ordered restore.Plan
```

destination 必须是 source 部署定义的连接替换，而不是隐式迁移工具。Project 与 Target 精确一致
能让执行阶段使用 destination Runner，同时保持 compose 路径、service、volume 与 files paths 不变。

## 3. Plan 形态

Plan 是 JSON 友好的值类型，至少包含：

- `manifest_snapshot_id`、`run_id`、`source_host`、`destination_host`；
- Project 定位副本；
- 按阶段排序的步骤；
- 每一步的 target ID/type、snapshot ID、必要配置和 image digest；
- 固定人工确认项。

步骤只描述“做什么”，不包含命令字符串。P3-2 由结构化字段构造 argv，避免 dry-run 展示与真实
执行各自拼一套语义。

## 4. 错误与兼容

- 校验类映射错误使用 `errors.Join` 一次性报告。
- 同 schema 未知 manifest 可选字段继续兼容；未知 schema 拒绝。
- 显式 snapshot 只接受唯一 manifest snapshot，不能通过 target snapshot 或模糊回退继续。
- Plan 输出可增加向后兼容字段，但字段删除/改义需与 P4 API 一并设计，本任务不承诺公开 API。

## 5. 测试

- 表驱动覆盖五类 target、顺序稳定、多 image digest 与 source=destination/cross-host。
- 覆盖 manifest fail/missing、清单 target/project 漂移、未知 host、snapshot 选择歧义。
- CLI 依赖计数器断言 dry-run 只调用 load config、new repo 与 manifest read；所有目标副作用为零。
- JSON 反序列化后核对字段，不以整段字符串快照替代结构断言。
