# P5-2 实施计划

## 1. dnsmgr：建立稳定 API

- [x] 在 `/root/project/dnsmgr/route/app.php` 的 AuthApi group 增加 `/api/auth/check` 与 `/api/record/value/:id` 固定 POST 路由。
- [x] 在 `app/controller/Auth.php` 增加无副作用认证成功响应，并为新增 public method 添加中文 DocBlock。
- [x] 在 `app/controller/Domain.php` 增加 Value-only handler：权限、记录详情、A/AAAA 地址族、expected value、幂等、字段保留、provider 更新和脱敏响应。
- [x] 把记录值更新中的纯校验/参数组装拆成可测试的最小 helper；不改动现有 `record_update` 行为。
- [x] 增加 dnsmgr 侧测试或最小可执行验证，覆盖成功、幂等、expected 冲突、记录不存在、非 A/AAAA、IPv4/IPv6 不匹配和 provider 失败。

验证：

```bash
php -l route/app.php
php -l app/controller/Auth.php
php -l app/controller/Domain.php
composer validate --no-check-publish
```

## 2. ark：清单和安全 client

- [x] 按现有 DTO 定义扩展 `internal/config/config.go`，增加顶层/host dnsmgr 配置及静态组合校验。
- [x] 更新 `internal/config/config_test.go` 和清单示例，覆盖兼容性、IP、URL、绝对路径、记录 ID、重复项与错误前缀。
- [x] 提取 `internal/secretfile` 的安全打开 helper，让 monitoring 与 dnsmgr 共用；保持 monitoring 现有行为和测试不变。
- [x] 新增 `internal/dnsmgr` settings/client/result 类型，完整添加中文 GoDoc，并实现 env key 白名单、签名、超时、无重定向和响应脱敏。
- [x] 使用 `httptest` 覆盖认证检查、成功/业务失败/HTTP 失败/非法 JSON/超时/3xx、签名和秘密泄漏。

验证：

```bash
go test ./internal/config ./internal/envfile ./internal/monitoring ./internal/dnsmgr
```

## 3. ark：恢复计划和 DNS 编排

- [x] 扩展 `internal/restore.Plan` 与相关校验/JSON 输出，构建有序且非秘密的 DNSPlan。
- [x] 更新 `BuildPlan` 与 `WithIsolation`：只为跨机原位恢复保留 DNSPlan，并调整 DNS 人工检查项。
- [x] 增加 DNS switcher 的顺序执行和逆序补偿逻辑；changed=false 不进入补偿栈，补偿继续收集全部失败。
- [x] 在 `internal/cli/restore.go` 的 completion marker 成功返回之后调用 switcher；合并 DNS 结果、整体状态、error 和 ManualChecks。
- [x] 保持 marker 不回滚，并增加“重跑只重试 DNS”的测试。
- [x] 更新 dry-run、inspect、expected preview digest、人类输出与 JSON 测试，证明零凭证读取和零 HTTP。

验证：

```bash
go test ./internal/restore ./internal/cli
```

## 4. ark：doctor 与命令隔离

- [x] 新增独立 `doctor.RunDNSMgr`，检查安全凭证加载和 `/api/auth/check`。
- [x] `ark doctor` 本地范围追加 dnsmgr 报告；restore 仅在 DNSPlan 存在且未指定 `--skip-doctor` 时把该检查作为前置门槛。
- [x] 证明 backup 与 verify 不调用 dnsmgr doctor，dnsmgr 不可达不会阻断备份或演练。
- [x] 覆盖 doctor 成功、凭证错误、认证失败、网络错误和输出脱敏。

验证：

```bash
go test ./internal/doctor ./internal/cli
```

## 5. 文档、全量检查与部署验证

- [x] 更新 `docs/design.md` 和 `docs/roadmap.md`，把 P5-2 契约改为 Value-only API、补偿语义和已实现状态。
- [x] 更新示例配置和运维说明，列出 `/etc/ark/dnsmgr.env` 键名、权限要求、发布顺序和回滚步骤，不放真实秘密。
- [x] 运行 ark 全量格式化、静态检查和测试。
- [ ] 构建 dnsmgr fork 镜像，在测试/现有部署上先验证 auth check，再用可回滚测试记录验证 forward + expected compensation。
- [ ] 部署 ark 后执行 dry-run、inspect 和一次跨机恢复场景，核对 DNS 结果、completion marker、dnsmgr 操作日志和无秘密输出。

验证：

```bash
gofmt -w <本任务修改的 Go 文件>
go vet ./...
go test ./...
git diff --check
```

## 6. 风险文件与回滚点

- `internal/config/config.go`：加法 schema 必须保持旧清单兼容，不能提升版本或让空配置触发失败。
- `internal/restore/plan.go`：DNSPlan 会进入 preview digest，字段顺序和稳定序列化必须有测试。
- `internal/cli/restore.go`：DNS 只能位于成功 completion marker 之后，不能进入 dry-run、inspect 或失败路径。
- `internal/monitoring/monitoring.go`：安全文件 helper 重构必须保持权限、owner、NOFOLLOW 和错误脱敏行为。
- `app/controller/Domain.php`：更新必须保留 provider 当前记录元数据；任何无法确定的字段都应 fail closed。
- 发布回滚顺序：先移除 ark host 关联阻止新调用，再回滚 ark；dnsmgr 加法接口可独立保留或回滚镜像。

## 7. 启动前复核

- [x] 确认最新 PRD、design、implement 与 Brief 一致。
- [x] 确认 ark 工作区已有 `docs/design.md`、`docs/roadmap.md` 修改属于本阶段前置文档同步，不被覆盖。
- [x] 确认 dnsmgr 工作区分支、状态和 P5-1 基线 commit，再开始跨仓修改。
- [x] 通过 `trellis-route(target=implement)` 选择执行模式后再写产品代码。
