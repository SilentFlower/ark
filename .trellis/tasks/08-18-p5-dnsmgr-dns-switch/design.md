# P5-2 技术设计

## 1. 边界与仓库

本任务跨两个仓库：

- `/root/project/dnsmgr`：提供无副作用认证检查和 Value-only DNS 更新接口。
- `/root/project/ark`：扩展清单、恢复计划、dnsmgr client、doctor、恢复结果和文档。

ark 仍只负责“恢复完成后请求切换”，不承担 provider 差异、DNS 传播或流量调度。provider 当前记录详情与更新能力继续由 dnsmgr 统一封装。

## 2. 清单模型

建议 schema v2 使用兼容的可选字段，不提升版本号：

```yaml
dnsmgr:
  base_url: https://dns.example.com
  env_file: /etc/ark/dnsmgr.env

hosts:
  - host: app-dr
    dnsmgr:
      value: 203.0.113.10
      records:
        - domain_id: 12
          record_id: "abc123"
```

对应类型边界：

- `config.Config.DNSMgr *DNSMgr`
- `config.Host.DNSMgr *HostDNSMgr`
- `HostDNSMgr.Value string`
- `HostDNSMgr.Records []DNSMgrRecord`
- `DNSMgrRecord.DomainID int64`
- `DNSMgrRecord.RecordID string`

顶层配置描述如何访问 dnsmgr，host 配置描述恢复到该主机后要切哪些记录。一个 host 只声明一个 IP，保持 MVP 简单；同时更新 A 与 AAAA 的双栈需求延后。

静态校验不访问网络或文件系统。URL 解析复用 monitoring 的 HTTPS/loopback 原则；host 错误沿用 `hosts[i](name).field` 前缀。

## 3. dnsmgr API

### 3.1 认证检查

`POST /api/auth/check`

- 路由位于现有 `Route::group('api', ...)` 中，继续受 `AuthApi` 保护。
- controller 只返回 `{"code":0,"msg":"认证成功"}`，不查询 DNS provider、不修改数据库。
- ark doctor 用它验证 URL、时钟、UID、API 开关和签名。

### 3.2 Value-only 更新

`POST /api/record/value/:id`

请求表单除 AuthApi 字段外包含：

| 字段 | 必填 | 含义 |
| --- | --- | --- |
| `recordid` | 是 | provider 的稳定记录 ID |
| `value` | 是 | 新 IPv4 或 IPv6 |
| `expected_value` | 否 | 实际写入前当前值必须匹配，用于安全补偿；目标值已存在时幂等优先 |

成功数据：

```json
{
  "code": 0,
  "msg": "修改解析记录成功",
  "data": {
    "recordid": "abc123",
    "previous_value": "198.51.100.20",
    "value": "203.0.113.10",
    "changed": true
  }
}
```

处理顺序：

1. 查询域名行并执行现有 `checkPermission`。
2. 通过 `DnsHelper::getModel` 创建 provider。
3. 优先调用 `getDomainRecordInfo(recordid)`；若返回 `false`，分页扫描 `getDomainRecords` 返回的标准化 `list`，按 RecordId 精确定位。对于 qingcloud 返回的带 `Count` 主机分组行，再调用 `getSubDomainRecords` 展开实际记录；该回退覆盖当前未实现详情方法的 qingcloud、dnsmgr、powerdns 驱动。
4. 详情与列表均无法定位记录时返回脱敏业务错误；分页必须按 total/返回条数收敛并设置上限，避免 provider 异常数据导致死循环。
5. 只允许 Type 为 A/AAAA，并用 PHP IP validator 校验地址族匹配。
6. 当前值已经等于新值时返回 `changed=false`，不调用 provider；补偿重试即使携带过期的 `expected_value` 也视为目标状态已经达成。
7. 需要实际更新且提供 `expected_value` 时，再比较当前 Value；不匹配时 fail closed。
8. 调用 `updateDomainRecord`，完整传回当前 Name、Type、Line、TTL、MX、Weight、Remark，仅替换 Value。
9. 写入现有操作日志并返回旧值、新值和 changed。

接口不承诺跨 provider 的数据库事务。`expected_value` 是补偿时的乐观并发保护，避免无条件覆盖 dnsmgr 或管理员在恢复期间做出的新切换。

## 4. ark dnsmgr 包

新增 `internal/dnsmgr`，职责限定为：

- 从受限 env 文件加载 `ARK_DNSMGR_UID`、`ARK_DNSMGR_API_KEY`。
- 构造 AuthApi 签名和表单请求。
- 提供 `CheckAuth` 与 `SetRecordValue`。
- 统一处理超时、禁止重定向、HTTP/JSON/业务码校验和错误脱敏。

为了避免 monitoring 与 dnsmgr 各自维护一份安全打开逻辑，提取一个最小 `internal/secretfile` helper：使用 `O_NOFOLLOW` 打开绝对路径，校验普通文件、当前 UID 和权限不宽于 `0600`。monitoring 改为复用该 helper，并由既有测试保护行为不变。

HTTP client 默认整体超时 10 秒；`CheckRedirect` 直接返回 `http.ErrUseLastResponse`，任何 3xx 都按失败处理。测试注入 clock 和 HTTP client，生产代码不暴露秘密字段。

## 5. 恢复计划与执行时序

`restore.Plan` 增加可选 `DNS *DNSPlan`，只包含：

- 目标 IP。
- 有序的 `{domain_id, record_id}`。

`BuildPlan` 在 source 与 destination 不同且 destination 配置了 dnsmgr 时填充 DNSPlan。`WithIsolation` 必须清空 DNSPlan，保证演练不绑定生产 IP。DNSPlan 参与 JSON 和 preview digest，因此检查后更改目标 IP 或记录列表会导致 `--expected-preview-sha256` 拒绝执行。

执行时序：

```text
BuildPlan -> doctor -> restore.Execute -> completion marker
                                    -> result ok/warn
                                    -> dnsmgr 顺序更新
                                       -> 成功：保留 ok/warn
                                       -> 失败：逆序补偿并返回 fail
```

DNS 编排放在 CLI/service orchestration 层，不嵌入单个 restore step：

- `restore.Execute` 保持数据恢复职责和现有 marker 语义。
- CLI 在 `Execute` 无 error 且 Result 为 ok/warn 后调用 DNS switcher。
- DNS 失败时不删除 completion marker。再次运行同一恢复时，已有 step marker 允许数据步骤快速验证/跳过，然后重新尝试 DNS。

## 6. 补偿算法

顺序处理计划记录：

1. 调 `SetRecordValue(domainID, recordID, targetValue, nil)`。
2. 只在响应 `changed=true` 时把 `{record, previousValue}` 压入补偿栈。
3. 任一调用失败后停止前向更新。
4. 逆序遍历补偿栈，调用 `SetRecordValue(record, previousValue, expected=targetValue)`。
5. 收集每条补偿结果，不因一条补偿失败而停止其他补偿。

结构化结果增加 DNS 摘要，记录前向状态、补偿状态和人工处理记录。任何前向失败都会把整体状态改为 `fail`；若全部补偿成功，错误摘要标明 DNS 未切换且已恢复旧值；若补偿不完整，ManualChecks 追加精确记录标识。

## 7. doctor 与命令隔离

不能把 dnsmgr 可达性直接加入 `doctor.RunLocal` 的阻断结果后让 backup/verify 复用，否则 DNS 系统故障会反向阻断备份。

设计为独立 `doctor.RunDNSMgr(ctx, cfg)`：

- `ark doctor` 默认本地检查时追加该报告。
- restore 仅当 Plan 含 DNS 动作时调用，并把失败视为阻断。
- backup 和 verify 不调用该检查。
- `--skip-doctor` 跳过 `RunDNSMgr`，但运行时 client 仍安全加载凭证并执行全部响应校验。

## 8. 人工检查与输出

- 配置了自动 DNS 的跨机原位计划移除“确认 DNS 指向目标主机”。
- P5-3 前仍提示管理员暂停并在结束后恢复相关 dmonitor 检测。
- DNS 失败或补偿不完整时重新加入针对具体记录的人工处理项。
- JSON 不输出 credential settings；人类错误不拼接请求体或 dnsmgr/provider 原始响应。

## 9. 兼容性与发布顺序

1. 先发布 dnsmgr 新接口；旧 UI、旧 API 和 provider 不受影响。
2. 再发布 ark；未配置可选字段时行为不变。
3. 最后在 `ark.yaml` 增加顶层和目标 host 配置，并通过 `ark doctor` 验证。

回滚时可先移除 host 的 dnsmgr 关联，ark 即恢复原行为；dnsmgr 新增路由为加法变更，可保留或回滚镜像。若补偿不完整，按结果中的记录标识在 dnsmgr 中人工核对 Value。

## 10. 风险与缓解

- **P5-3 未完成导致检测竞争**：保留暂停/恢复 dmonitor 的人工项；补偿使用 expected_value，避免覆盖新的切换。
- **provider 字段差异**：优先详情、列表回退都只消费标准化字段，并用详情可用/不可用及 null/缺省字段夹具覆盖。
- **恢复完成但 DNS 失败**：completion marker 保留，使重试不重复破坏性恢复步骤。
- **双栈需求**：MVP 一个 host 只配置一个 IP；后续可把 value 下沉到 record 级，不在本任务预埋复杂模型。
- **dnsmgr 缺少现成测试框架**：至少增加可独立测试的纯逻辑 helper，并执行 PHP 语法、路由/控制器测试或容器级 API 冒烟；ark 侧用 `httptest` 完整覆盖客户端和编排。
