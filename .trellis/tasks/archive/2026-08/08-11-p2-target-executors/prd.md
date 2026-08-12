# P2-3 target 执行器

## Goal

新增 `internal/backup/` 的五类 target 执行器，通过现有 `sshexec.Runner` 在本地或 SSH
目标机生成纯数据流和稳定元数据，供 restic 消费。

## Requirements

### R1 包边界与结果

- 新增 `executor.go`、`postgres.go`、`redis.go`、`volume.go`、`files.go`、`image.go`
  及对应测试。
- 执行器只依赖 config 与 `sshexec.Runner`，不直接创建 ssh/exec 命令，不依赖 store 或 CLI。
- 结果必须保留 reader、只调用一次的 Wait、稳定 stdin filename 和 target 元数据；
  `image_digest` 还需提供确定的 service→digest 映射。
- stdin filename 固定为 `<host>/<target.ID()>` 加与内容匹配的稳定后缀，禁止日期或 run ID。

### R2 Compose 命令

- compose argv 复用现有 `-f`、可选 `-p`、可选 `--env-file` 顺序和语义。
- PostgreSQL 固定执行 `exec -T <service> pg_dump -U <user> -d <database>
  --no-owner --no-acl --clean --if-exists`；未配置 user 时不传 `-U`。
- 不能使用 `-Fc`、gzip、临时文件或 PGDATA 热拷贝。

### R3 Redis、volume 与 files

- Redis 先读取 `LASTSAVE` 基线，触发 `BGSAVE`，再按 context 轮询到时间戳变化，
  最后执行 `exec -T <service> cat /data/dump.rdb` 产生流。
- Volume 固定使用只读挂载和保留权限的 tar：`docker run --rm -v <name>:/src:ro
  alpine tar -cpf - -C /src .`。
- Files 使用 `tar -cpf - -- <paths...>`，保留权限/属主，不预压缩；路径保持独立 argv。

### R4 镜像 digest

- 必须从正在运行容器的镜像 ID 反查 `RepoDigests`，不能直接相信 compose tag。
- `RepoDigests` 为空、无法对应目标仓库或出现多个歧义候选时失败，禁止猜测。
- 输出使用确定性 JSON，service 排序稳定，便于 snapshot 去重和 manifest 复用。

### R5 安全与错误

- 所有用户配置值继续由 `sshexec` 做远程参数转义，本包不增加 `bash -c` 或 shell 拼接。
- stdout 只包含备份数据；stderr 和命令失败只进入脱敏错误。
- context 取消、Run/Stream/Wait/Close 错误必须保留错误链。

## Non-Goals

- 不调用 restic、不写状态库、不执行坏快照撤销或体积跌幅判断。
- 不在本任务执行真实机器验收；真实产流由 `p2-live-validation` 覆盖。
- 不实现恢复方向的 Feed 命令。

## Acceptance Criteria

- [ ] 五类执行器均有表驱动单元测试，断言精确 argv、稳定文件名和结果元数据
- [ ] PostgreSQL 必含 `-T` 且无压缩；Redis 确实等待 LASTSAVE 变化
- [ ] volume/files tar 保留权限，volume 只读挂载，参数不经额外 shell
- [ ] image digest 只从运行容器反查，空值和多候选明确失败
- [ ] Wait、context、Close 和各阶段失败均可见且不会污染数据流
- [ ] `go test ./internal/backup -race -count=1`、`make check` 与无 CGO 构建通过
