<div align="center">

# ark

**Docker Compose 整机备份与重建**

把一台机器的数据库、数据卷、配置和镜像身份打包成快照，
在任意一台干净的新机器上重建出来。

</div>

---

## 这是什么

如果你的服务用 docker compose 部署，而你担心某天服务器突然没了——
ark 要保证那天你手上有的不只是一个 `.sql.gz`，而是**一整套能直接拉起来的东西**：

- 数据库（`pg_dump` 逻辑备份，不是不可靠的 PGDATA 热拷贝）
- 数据卷
- `docker-compose.yml`、`.env`、反代配置
- 当时运行的镜像 digest（不是会漂移的 `:latest`）

一台 hub 通过 SSH 管理所有机器。**被备份的机器上不装任何东西**——
只要有 docker 和 sshd 就行。加一台新机器的成本是在清单里加一段配置。

## 这不是什么

- **不是主备复制的替代品**。复制解决「机器挂了切过去」；备份解决「数据被删/被写坏」。
  逻辑错误会同步到备机，只有备份能救。两者互补，不互相替代。
- **不做秒级 RPO**。默认 24 小时，可配到小时级。
- **不做自动故障切换**。ark 只备份和恢复，不碰流量调度。

## 架构

```
机器 A ─┐
机器 B ─┼─ ssh ─►  hub  ─ restic(加密) ─►  对象存储 (R2/S3)
机器 C ─┘         编排 / 界面              开启对象锁
                  全部凭证
```

hub 是刻意设计的单点：它持有 restic 密码、对象存储凭证和各机 SSH 私钥，
能读所有备份、能登所有机器、能发起恢复。换来的是运维上的简洁——
一处配置、一处升级、一个界面。

代价用两条兜底摁住：**对象存储桶开启对象锁**（攻陷 hub 也删不掉备份），
**restic 密码另存离线副本**（hub 磁盘挂了也解得开）。详见 [设计文档](docs/design.md)。

## 当前状态

**早期开发中。** 目前可用的是清单模型和两个校验命令，备份和恢复尚未实现。
进度见 [路线图](docs/roadmap.md)。

| 命令 | 状态 | 说明 |
|---|---|---|
| `ark validate` | ✅ | 校验清单语法语义，不碰 docker 和网络 |
| `ark doctor` | ✅ | 检查是否具备执行条件 |
| `ark backup` | 🚧 P2 | 执行备份 |
| `ark restore` | 🚧 P3 | 恢复 / 跨机重建 |
| `ark verify` | 🚧 P3 | 自动恢复演练 |
| `ark-hub` | 🚧 P4 | Web 界面与 API |

> ⚠️ 架构在 P0 之后从「每台机器装 agent」改成了「hub 集中编排 + 目标机零安装」。
> 已有的 `validate` / `doctor` 仍然是按单机清单实现的，
> **P1 会把清单模型迁移到多机（`version: 2`）并改造 doctor**。
> 现在照着 `examples/ark.yaml` 写的清单在 P1 之后需要迁移。

## 快速开始

```bash
# 构建
make build

# 准备清单
sudo mkdir -p /etc/ark
sudo cp examples/ark.yaml /etc/ark/ark.yaml
sudo vim /etc/ark/ark.yaml

# 校验清单本身（不碰 docker 和网络，任何机器都能跑）
./bin/ark validate -c /etc/ark/ark.yaml

# 校验是否真的能执行
./bin/ark doctor -c /etc/ark/ark.yaml
```

`doctor` 的退出码：`0` 全部通过，`1` 工具本身出错，`2` 检查未通过。
方便直接挂到监控上。

## ⚠️ 关于密钥

**restic 仓库密码必须另外保存一份在 hub 之外**（密码管理器 / 离线介质）。

只存在于 hub 上的仓库密码，在 hub 损毁时会让所有备份一起变成无法解密的废数据——
这是备份系统最常见、也最致命的失败方式。

SSH 私钥同理，且**密钥本身不进备份**：把开锁的钥匙和锁着的箱子放在同一个地方没有意义。

## 开发

```bash
make check     # 格式化 + vet + 测试
make build     # 编译到 bin/
make test      # 只跑测试
```

要求 Go 1.22+。

运行时依赖分两侧：

- **hub**：`restic`、`ssh`、`systemd`、`docker`（hub 自己也被备份时需要）
- **目标机**：`docker`、`docker compose v2`、`sshd`——**不需要装 ark 或 restic**

## 文档

- [设计文档](docs/design.md) — 架构、ADR、取舍理由
- [路线图](docs/roadmap.md) — 分阶段计划与验收标准
- [清单示例](examples/ark.yaml) — 带注释的完整配置

## License

MIT
