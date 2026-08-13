# P3-3 跨主机重建实测记录

## 执行摘要

- 日期：2026-08-13（Asia/Shanghai）
- 结论：**通过·已接受风险**；受限隔离验收 PASS，严格干净 VPS 验收仍未满足。
- 拓扑：hub 上运行当前提交构建的 Ark、restic、隔离源业务与 S3 兼容仓库；destination 为
  用户授权的备用 VPS，通过 SSH 执行 agentless 隔离恢复。
- 边界：真实地址、账号、密码、SSH 私钥、对象存储凭证、`.env` 内容均未写入任务文件或 Git。
- 未触碰范围：hub 与 destination 上原有业务容器、DNS、TLS、dnsmgr、防火墙和代理配置均未修改。

## 前置条件与基线

### Hub

- Debian 10，Docker 26.1.4，Docker Compose v5.1.0。
- 使用当前提交构建的静态 Linux/amd64 Ark 二进制；restic 0.19.1 来自官方发布包并完成 SHA-256
  校验。
- 本轮资源统一使用 `ark-p3-live-*` 项目、volume、目录和端口；既有 nginx、代理、DNS、数据库等
  业务资源保持不变。
- 为降低多次 SSH 建连延迟，仅在 hub 测试目录使用 OpenSSH ControlMaster wrapper；该 wrapper
  不属于产品代码，也未部署到 destination。

### Destination

- Debian 12，Docker 29.7.2，Docker Compose v5.4.0。
- 未安装 Ark、restic，也不存在 hub 的仓库密码、对象存储凭证或 SSH 私钥。
- 机器不是干净 VPS：验收前已有两个业务容器和其它宿主机服务。因此本轮只使用稳定 isolation ID、
  独立 Compose project、独立路径、自动端口和 isolation label 做受限验证。
- 恢复前没有本轮 isolation 对应的容器、volume、network 或文件树；原有两个业务容器在中断、
  清理、重建和复验后始终保持运行。

## 备份与仓库检查

- Run ID：`20260813T012226.844890627Z-87285f62ab9d9597`
- Manifest snapshot：`945fdf00cee50595c857a079094912e46623530cecc9deeeabcd58de4835237e`
- 5 个备份 target 均为 `ok`；整体状态为 `warn`，唯一原因是测试 S3 仓库未启用 object lock，
  与数据完整性无关。
- Snapshot：
  - files：`9818be5421552867d0614fd9834264d42c2fced0094993c3d265398f9c02a9ea`
  - image digest：`2fc35086a83d5bf1635c90cd20d560cbe72f7cd9950e3268ef23f8e1ccf337a1`
  - application volume：`3fd7449fb8717eb87e2db8991233926059edd4442e2a5653d712f11e0e35cefc`
  - PostgreSQL：`a9837cde9d65aee4f295f0f6c852cb19280eae5affd952a234bf5820edde1cef`
  - Redis：`18fd1291659d8ae88bdbbec63b5b43a07f6d033a103f0f7a98cc9989a8881d5a`
- `restic check --read-data-subset=1/1` 完整读取 6 个 snapshot、6 个 data pack，无错误。

## Dry-run 与隔离计划

- source：`p3-source`；destination：`p3-destination`。
- Isolation ID：`b346da1b71a06e206c30875e54b2179454caa1ebabfde0f997a45e67d6febf70`。
- 隔离项目：`ark-p3-live-source-restore-b346da1b71a0`。
- 隔离根目录：`/var/lib/ark/restore/isolations/<isolation-id>`。
- 9 个阶段按 files、image digest、volume、PostgreSQL prepare、Redis prepare、PostgreSQL data、
  Redis data、application、health 排列，snapshot 与 manifest 一致。
- 原 Compose 的两个发布端口在 Plan 中均为 `auto`；dry-run 后确认 destination 没有任何新增文件、
  容器、volume 或 network。
- 真实执行未使用 `--force`，没有覆盖或停止既有 destination 资源。

## 中断与幂等续跑

1. 首次真实恢复在首个阶段 marker 附近向 Ark 进程组发送 TERM，退出码为 `143`，stderr 为空。
2. 中断后仅出现隔离文件树与状态目录；没有创建容器、volume 或 network。
3. 重跑同一 manifest、source、destination 与 `--isolate` 命令，9 个阶段全部 `ok`，三个容器健康。
4. 在完整成功后再次运行完全相同命令，9 个阶段全部为 `skipped`，每项 detail 均为
   `后置条件已验证`，没有重复执行破坏性恢复。
5. 隔离副本第一次分配端口 `32768/32769`。执行归属清理后占用条件发生变化，再次从零恢复自动分配
   `32770/32771`，未与既有端口冲突。

结论：中断恢复和完整恢复后的幂等重跑均符合预期。中断发生在首个阶段完成边界附近，但没有形成
可验证的完整阶段；不能据此声称该阶段应跳过。完整成功后的复跑则明确证明全部完成步骤会先验证
后置条件再跳过。

## Source Independence

为避免仅凭代码路径推断，执行了源业务停止后的完整重建：

1. 停止 hub 上本轮创建的 PostgreSQL、Redis、MinIO 三个源业务容器，确认运行数为 0；保留备份仓库
   和 hub 恢复工具。
2. 使用 Ark cleanup 的 isolation 归属校验清理前一副本；结果 `ok`，只移除 6 个隔离资源，随后
   destination 的本轮隔离容器、volume、network 均为 0，原有两个业务容器仍在运行。
3. 在源业务持续停止时，仅凭同一 manifest、restic 仓库与 destination SSH，从零执行恢复。
4. 9 个阶段全部真实执行为 `ok`，恢复后业务、数据、文件和镜像验证全部通过。

结论：恢复不依赖源业务容器、源数据库或源 volume 在线，也没有从源主机临时复制材料。源数据链路
由 hub 保存的 manifest、对象仓库 snapshot 与 SSH dump/restore 流程完整提供。

## 业务与数据验证

| 验证项 | 结果 | 脱敏证据 |
| --- | --- | --- |
| 容器状态 | PASS | 隔离项目 3 个容器均 healthy；destination 原有 2 个业务容器仍运行。 |
| PostgreSQL 表与关联 | PASS | 账号表 1 行、会话表 1 行，代表性外键关联存在；最新记录时间非空。 |
| 加密字段 | PASS | 使用恢复后的 `.env` 密钥通过 PostgreSQL 会话参数解密，得到预置明文标记。 |
| Redis | PASS | 代表性 string key 与 hash field 均与备份前一致。 |
| Application | PASS | 使用恢复出的测试账号读取对象，写入、回读并删除临时对象；删除后的测试写入未残留。 |
| Volume | PASS | 两个代表性对象目录存在，64 MiB 对象内容 SHA-256 与源端备份前记录一致。 |
| Files | PASS | 隔离 compose 与 `.env` 均恢复；权限分别为 `0644` 与 `0600`。 |
| Image digest | PASS | MinIO、PostgreSQL、Redis 运行容器 RepoDigest 与 Plan/manifest 全部一致。 |
| Agentless | PASS | destination 无 Ark、restic、仓库密码、对象存储凭证或 hub SSH 私钥。 |

运行镜像 digest：

- MinIO：`sha256:14cea493d9a34af32f524e538b8346cf79f3321eff8e708c1e2960462bd8936e`
- PostgreSQL：`sha256:57c72fd2a128e416c7fcc499958864df5301e940bca0a56f58fddf30ffc07777`
- Redis：`sha256:e7723ff73d963f5cc6d9c4643ea3d989527a402a319239054e9472a7fb9219a2`

## 差异与限制

1. **严格前置条件未满足**：destination 不是“只预装 Docker、Compose v2 与 sshd”的干净 VPS。
   虽然 isolation 归属、自动改名、自动端口和原有资源不变均已实机通过，仍不能把干净机验收项标为
   PASS。
2. 测试 S3 仓库没有启用 object lock，因此备份结果保留 object-lock warning；本轮 restic 全量读取
   检查和恢复均通过。
3. 普通 `doctor --host p3-destination` 会按原始项目路径检查尚未恢复的业务资源，不适用于隔离恢复
   前的 destination；真实 restore 使用恢复目标专用检查路径并成功完成。这不是本轮发现的代码缺陷。
4. 本轮未变更生产 DNS、TLS、dnsmgr、防火墙或代理，也不构成上线切流验收。

## 风险接受

- 2026-08-13，用户明确接受 `CHK-001`：本轮使用非干净 destination 和专用验证栈，不能替代
  干净 VPS 上的真实业务登录、真实业务数据与实际 Compose 重建验收。
- 2026-08-13，用户明确接受 `FBK-001`：本机回收站中的临时 SSH 私钥、仓库密码和其它测试材料
  仍可恢复，尚未永久清除。
- 风险接受不表示问题已修复；证据、影响或任务范围变化后需要重新检查和确认。

## 清理与当前状态

- 中间副本已通过 Ark isolation cleanup 完成一次归属清理，证明 cleanup 不影响 destination 原有资源。
- 用户继续执行推荐下一步后，最终 source-down 副本已通过同一 isolation cleanup 完成归属清理；
  Ark 返回 `ok` 并移除 6 个隔离资源。
- destination 清理后，本轮 isolation 的容器、volume、network 与根目录均为 0/不存在；临时 SSH
  公钥授权已精确撤销，原有两个业务容器仍在运行。
- hub 上两个 `ark-p3-live-*` 测试 Compose 项目、4 个测试 volume、2 个测试 network、测试目录、
  仓库数据和临时凭证均已清理；既有业务容器保持运行。
- 本机临时 Ark/restic 二进制、下载件、SSH 控制目录和 Git 忽略的运行凭证目录已移入系统回收站；
  其中私钥与仓库密码仍可从回收站恢复，尚未完成永久清除。
- 未创建临时 DNS 或端口转发；应用 roundtrip 测试对象已删除。

## 验收清单

- [ ] 干净 VPS 仅凭 hub 侧材料完成完整恢复，源主机零依赖
  - source independence 已实机 PASS；干净 VPS 前置条件未满足。
- [x] dry-run Plan 与真实执行目标一致，默认路径无隐式覆盖
- [x] 中断后重跑能够安全继续，已完成步骤不重复破坏
- [x] 业务表、最新记录、应用读写和加密字段验证通过
- [x] Redis、volume、files 与全部运行镜像 digest 验证通过
- [x] destination 没有 Ark、restic、仓库密码或对象存储凭证
- [x] 人工确认清单已逐项处理或明确记录为未上线事项
  - 用户继续执行推荐处置；全部测试资源已完成归属清理，严格干净机缺口明确保留。
- [x] `validation.md` 证据完整、脱敏；本轮未发现需要回流代码任务的产品缺陷

## 下一步

1. 若要完成严格 P3-3 验收，提供一台只预装 Docker、Compose v2 与 sshd 的授权干净 VPS，并复用
   本轮 manifest 再执行一次从零恢复。
2. `CHK-001` 与 `FBK-001` 已由用户明确接受，Check-All 结论为“通过·已接受风险”。
3. 下一步进入 `trellis-update-spec`，再由 `trellis-push` 生成精确提交计划。
