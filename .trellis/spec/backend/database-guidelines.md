# Database Guidelines

> ark 如何对待它所备份的数据库。

---

## Overview

**ark 自己没有数据库。** 它不持久化任何状态，没有 ORM，没有迁移，
没有连接池，也不写一行 SQL。运行结果写成对象存储上的 JSON 状态文件
（见 `docs/design.md` 第 9 节），仅此而已。

因此本文档不是「我们怎么用数据库」，而是「我们怎么备份和恢复**别人的**数据库」。
这是 ark 最核心也最不容出错的部分：备份工具在数据库上犯的错，
只会在真正需要恢复的那天暴露。

**不要**因为需要读写数据而引入 `database/sql`、`pgx`、GORM 或任何 ORM。
如果你正准备加这类依赖，先确认需求是不是能用一个 JSON 文件解决。

---

## Access Pattern

ark 与数据库交互的唯一方式是**通过 `docker compose exec -T` 调用容器内自带的 CLI 工具**：

| 数据库 | 备份 | 恢复 |
|---|---|---|
| PostgreSQL | `docker compose exec -T <service> pg_dump` | `docker compose exec -T <service> psql` |
| Redis | `docker compose exec -T <service> redis-cli BGSAVE` 后取 RDB | 放回 RDB 文件后再起容器 |

不用 Go 数据库驱动直连，理由有三条：

1. **版本天然匹配**。容器内的 `pg_dump` 与该容器的 PostgreSQL 服务端同版本。
   宿主机上装的 `pg_dump` 可能比服务端旧，会直接拒绝导出。
2. **不需要暴露端口**。数据库通常只监听 compose 内部网络，
   走 `exec` 不必为了备份把端口开到宿主机。
3. **不需要在 ark 里管理数据库凭证**。容器内通常已有 `.pgpass`、
   `POSTGRES_USER` 或 trust 认证，ark 不必再存一份密码。

`-T` 参数必须带上。不带 `-T` 时 compose 会分配 TTY，
输出里会混入控制字符并做 CRLF 转换，**二进制转储会被静默损坏**——
备份能跑完、能上传，只有恢复时才发现导入失败。

---

## Backup Rules

### 逻辑备份，绝不打包 PGDATA（ADR-003）

`postgres` 类型固定走 `pg_dump`。**任何情况下都不要改成直接打包 PGDATA 卷。**

运行中的 PostgreSQL 数据目录做热拷贝，得到的是撕裂的、不一致的快照，
恢复时可能根本起不来，而且在真正恢复之前你无从知道它是坏的。
Redis 同理：必须先 `BGSAVE` 再取 RDB，不能直接拷贝正在写入的 dump.rdb。

只有确定与数据库无关的卷（用户上传文件、静态资源）才允许用 `volume` 类型直接打包。
`internal/config/config.go:49` 起的 `TargetPostgres` 注释记录了这条约束的理由，
改动该类型的实现前先读它。

### 转储走 stdin 流式进 restic，不落临时文件

`pg_dump` 的 stdout 直接接到 `restic backup --stdin`，中间不写宿主机磁盘。
备份一个几十 GB 的库时，宿主机往往没有那么多余量放临时文件；
更重要的是，临时文件是明文的数据库全量转储，多存在一秒就多一秒风险。

### --stdin-filename 必须跨次运行保持稳定

这是最容易被无意破坏的一条。restic 按文件名做内容寻址去重，
文件名一变就当成全新文件，**去重效果直接归零**，仓库体积会随备份次数线性膨胀。

```
✅ postgres/sub2api.sql           固定值
❌ postgres/sub2api-20260811.sql  含日期，每天都是新文件
```

快照的时间信息由 restic 自己的 snapshot 元数据承载，不需要编进文件名。
`Target.ID()`（`internal/config/config.go:250`）返回的就是这种稳定标识，
新增 target 类型时沿用它，不要另造带时间戳的命名。

### 不要预压缩

restic 自带压缩且基于内容分块去重。在送进 restic 之前先 `gzip`，
会让每次转储的字节流完全不同，去重和压缩双双失效。
`pg_dump` 也不要加 `-Fc`（自定义格式自带压缩），用默认的纯文本 SQL。

---

## Restore Rules

恢复顺序是有严格依赖的，写恢复逻辑时按此执行（详见 `docs/design.md` 第 7 节）：

1. 先按 digest 拉取镜像（ADR-004），不要用 tag。
2. 只启动数据库容器，**等它真正 ready 之后再导入**——
   容器状态 running 不等于 PostgreSQL 已经接受连接，
   需要轮询 `pg_isready` 而不是 `sleep` 一个固定秒数。
3. 导入数据库转储。
4. 恢复数据卷与配置文件。
5. 最后才启动应用容器。

顺序颠倒的典型后果：应用先起来，发现表不存在，自动跑了一次初始化建表，
随后的转储导入撞上已存在的对象而失败——**而且部分表已经被应用写脏了**。

---

## Common Mistakes

- **忘记 `-T`**，二进制流被 TTY 转换损坏，直到恢复时才发现。
- **在 `--stdin-filename` 里放日期或快照 ID**，去重失效，仓库体积失控。
- **把 `pg_dump` 输出先落成临时文件再上传**，磁盘可能不够，且明文转储落盘。
- **改用打包 PGDATA 卷「省事」**，得到一份看起来正常、实际无法恢复的备份。
- **用 `sleep 10` 代替 `pg_isready` 轮询**，在慢机器上间歇性失败。
- **恢复时按 tag 拉镜像**，半年后的 `:latest` 与备份的 schema 已不兼容。
- **在 ark 里存数据库密码**，没必要——容器内已有认证配置，
  多存一份就是多一个泄漏点。
