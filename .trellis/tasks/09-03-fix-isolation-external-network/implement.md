# external network 隔离恢复实施计划

## 1. 锁定回归测试

- [x] 在 `internal/restore/isolation_test.go` 增加 external network 成功转换用例，覆盖派生名称、bridge driver、isolation label、删除 external/原名称、service alias 与逻辑引用保持。
- [x] 覆盖布尔/object external 表示、无显式名称回退和 Compose v5 空 canonical 默认字段；external network 带非空额外运行时参数时必须在 Docker 创建前拒绝。
- [x] 保留并扩充 external volume/config/secret、非 bridge network 和 driver_opts 的拒绝用例。
- [x] 在 restore、verify 与 CLI 测试中固定安全摘要传播和敏感错误不进入 Result/人类输出/JSON 的契约。

定向验证：

```bash
go test ./internal/restore ./internal/verify ./internal/cli -race -count=1
```

## 2. 实现 external network 私有化转换

- [x] 调整 `internal/restore/isolation.go` 的 named resource 转换，只为 network 增加 external 专用分支，volume 行为保持不变。
- [x] 严格校验 external network 输入形态，提取稳定来源名称，并重新构造最小派生 bridge network。
- [x] 复用现有 `isolationResourceName`、isolation label、资源排序和 state network 清单，不新增 cleanup 特例或 schema。
- [x] 保持 service `networks` attachment 原样，证明 alias 和逻辑拓扑不因顶层物理 network 改写而变化。

定向验证：

```bash
go test ./internal/restore -race -count=1
```

## 2.1 兼容 Compose 默认 local volume driver

- [x] 在 `internal/restore/isolation_test.go` 增加普通 named volume 缺省 driver 与 `driver: local` 成功转换用例。
- [x] 调整 volume driver 校验，只允许空值或 `local`；非 `local` driver、任何非空 `driver_opts` 与 external volume 继续返回受控安全摘要。
- [x] 在真实 Docker external-network 用例中保留 Compose v5 canonical 的 `driver: local`，证明派生 volume、cleanup 与生产基线均正常。
- [x] 同步 restore/verify 规范和 Brief，明确这是 canonical 默认兼容，不是共享 volume 放行。

定向验证：

```bash
go test ./internal/restore ./internal/verify ./internal/cli -race -count=1
ARK_DOCKER_INTEGRATION=1 go test ./internal/restore -run 'TestIsolationDockerIntegration' -count=1
```

## 3. 实现脱敏阶段摘要

- [x] 在 `internal/restore` 增加仅包内使用的安全摘要错误契约；只允许代码生成的固定摘要进入结果，禁止自动复制任意底层错误文本。
- [x] 让 Compose 隔离策略拒绝点提供安全摘要，`failResult` 在命中该契约时写入 `restore.Result.Error`，其它错误继续使用通用文案。
- [x] 调整 `internal/verify/verify.go`，把更具体的 restore 安全摘要带入 verify 阶段文案，并保持 cleanup、baseline、store 的现有错误优先级与合并语义。
- [x] 验证 `isolationComposeCommandError`、SSH/Docker/restic stderr 和注入的敏感字符串不会进入 CLI 或 JSON。

定向验证：

```bash
go test ./internal/restore ./internal/verify ./internal/cli -race -count=1
go test ./internal/restore ./internal/verify ./internal/cli -race -count=10
```

## 4. Docker 集成与规范同步

- [x] 扩展 `internal/restore/isolation_docker_test.go`，用真实 external bridge 验证隔离容器只连接派生 network，生产 external network 与生产容器基线不变。
- [x] 验证普通 isolate 的自动端口、verify 的禁用端口、cleanup 无残留和失败保留路径没有回归。
- [x] 更新 `.trellis/spec/backend/restore-plan-guidelines.md` 的 external network、错误矩阵与 Good/Bad case。
- [x] 更新 `.trellis/spec/backend/verify-guidelines.md` 的共享网络隔离与安全摘要契约。

可用 Docker 和本地镜像时运行：

```bash
ARK_DOCKER_INTEGRATION=1 go test ./internal/restore -run 'TestIsolationDockerIntegration' -count=1
```

## 5. 全量质量门

- [x] 对变更 Go 文件执行 `gofmt`，并检查无无关格式或元数据改动。
- [x] 运行 verify 相关包单次 race 与十次重复回归。
- [x] 运行全项目 `make check`、模块校验和 diff 检查。
- [x] 使用静态构建生成部署二进制，并确认目标机 glibc 兼容风险已消除。

```bash
gofmt -w internal/restore/isolation.go internal/restore/isolation_test.go internal/restore/isolation_docker_test.go internal/restore/execute.go internal/restore/execute_test.go internal/verify/verify.go internal/verify/verify_test.go internal/cli/restore_test.go internal/cli/verify_test.go
go test ./internal/verify ./internal/restore ./internal/cli ./internal/store ./internal/systemd -race -count=1
go test ./internal/verify ./internal/restore ./internal/cli ./internal/systemd -race -count=10
make check
go mod verify
git diff --check
CGO_ENABLED=0 make build
file bin/ark
ldd bin/ark
```

`ldd` 对静态二进制应报告不是动态可执行文件，而不是缺少共享库。

## 6. hub 部署与 biz 真机验收

- [x] 首次部署已验证 external network 转换与安全摘要生效；发现 Compose v5 普通 volume 默认 `driver: local` 后已回滚，生产基线一致、cleanup 无残留、timer active。
- [ ] 部署前记录本地 commit、二进制 SHA-256 和 `biz` 生产基线；在 hub 为当前 `/usr/local/bin/ark` 创建新的带时间戳备份。
- [ ] 原子替换静态二进制后执行 `/usr/local/bin/ark --config /etc/ark/ark.yaml validate`、doctor 与 dnsmgr AuthApi 检查。
- [ ] 执行 `/usr/local/bin/ark --config /etc/ark/ark.yaml verify --host biz --snapshot latest --json`，保存结构化验收证据。
- [ ] 证明演练容器未连接 `api_shared`，原 external network、生产容器、volume、files 基线前后一致。
- [ ] 证明结束后无带本次 isolation label 的容器、network、volume 或 `/var/lib/ark/restore/isolations/<id>` 残留。
- [ ] 核对 `ark-verify.service` 最近一次结果和 `ark-verify.timer` 启用/活跃状态。

## 回滚

1. 停止新的手工 verify，不删除任何无法证明归属的 Docker 资源。
2. 恢复本次部署前即时备份的 `/usr/local/bin/ark`，校验 SHA-256 后原子替换。
3. 重新执行 validate、doctor，并确认 verify timer 仍指向有效二进制。
4. 若新版本留下隔离资源，只使用其结构化 cleanup command 或匹配 isolation ID 的 `ark restore cleanup`；归属校验失败时保留 state/root，禁止手工按宽泛名称批量删除。
5. `/usr/local/bin/ark.backup-42a8936-20260903` 仅作为即时备份不可用时的更早兜底。
