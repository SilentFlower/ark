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

多台机器共用一套工具和一个对象存储，各自推送到自己的 prefix，
任意一台的备份可以在任意一台上恢复。一台中心机提供 Web 页面看全局状态。

## 这不是什么

- **不是主备复制的替代品**。复制解决「机器挂了切过去」；备份解决「数据被删/被写坏」。
  逻辑错误会同步到备机，只有备份能救。两者互补，不互相替代。
- **不做秒级 RPO**。默认 24 小时，可配到小时级。
- **不做自动故障切换**。ark 只备份和恢复，不碰流量调度。

## 架构

```
机器 A ─┐
机器 B ─┼─► 对象存储 (R2/S3)  ─►  中心机 ark-hub
机器 C ─┘   restic 加密仓库        只读状态元数据 + Web 页面
            _status/*.json
```

中心机**不持有 restic 密码**，拿不到任何备份内容，只能看到状态元数据。
它宕机不影响备份继续执行。详见 [设计文档](docs/design.md)。

## 当前状态

**早期开发中（P0 已完成）。** 目前可用的是清单模型和两个校验命令，
备份和恢复尚未实现。进度见 [路线图](docs/roadmap.md)。

| 命令 | 状态 | 说明 |
|---|---|---|
| `ark validate` | ✅ | 校验清单语法语义，不碰 docker |
| `ark doctor` | ✅ | 检查本机是否具备执行条件 |
| `ark backup` | 🚧 P1 | 执行备份 |
| `ark restore` | 🚧 P2 | 恢复 / 跨机重建 |
| `ark verify` | 🚧 P2 | 自动恢复演练 |
| `ark-hub` | 🚧 P3 | 中心机 Web 页面 |

## 快速开始

```bash
# 构建
make build

# 准备清单
sudo mkdir -p /etc/ark
sudo cp examples/ark.yaml /etc/ark/ark.yaml
sudo vim /etc/ark/ark.yaml

# 校验清单本身（不需要 docker，任何机器都能跑）
./bin/ark validate -c /etc/ark/ark.yaml

# 校验这台机器是否真的能执行
./bin/ark doctor -c /etc/ark/ark.yaml
```

`doctor` 的退出码：`0` 全部通过，`1` 工具本身出错，`2` 检查未通过。
方便直接挂到监控上。

## ⚠️ 关于密钥

**restic 仓库密码必须另外保存一份在生产机之外**（密码管理器 / 离线介质）。

只存在于生产机上的仓库密码，在机器损毁时会让所有备份一起变成无法解密的废数据——
这是备份系统最常见、也最致命的失败方式。

## 开发

```bash
make check     # 格式化 + vet + 测试
make build     # 编译到 bin/
make test      # 只跑测试
```

要求 Go 1.22+。运行时依赖 `docker`、`docker compose v2`、`restic`、`systemd`。

## 文档

- [设计文档](docs/design.md) — 架构、ADR、取舍理由
- [路线图](docs/roadmap.md) — 分阶段计划与验收标准
- [清单示例](examples/ark.yaml) — 带注释的完整配置

## License

MIT
