# 执行计划

## 步骤

1. 获取用户对测试 VPS、临时清单和隔离资源创建/清理的明确授权。
2. 记录目标机基线，确认只安装 Docker/Compose/sshd 且无同名资源。
3. 用临时 destination host 配置运行 validate、doctor 与 restore dry-run。
4. 核对 Plan 后执行真实 restore，记录阶段证据。
5. 在安全阶段制造一次可恢复中断，重跑验证幂等续跑。
6. 完成数据库、应用读写、加密字段、Redis、volume、files 和 digest 验证。
7. 核对目标机没有 ark/restic/仓库凭证，恢复期间没有从源主机读取材料。
8. 将缺陷回流到代码任务修复并重复失败场景，直至全部通过。
9. 写入 `validation.md`，清理测试写入与临时连接；目标 VPS 的保留/销毁等待用户指令。

## 完成前检查

- 不把未执行、skip 或只看容器状态的项目标记为通过。
- 不记录真实密码、私钥、AK/SK、`.env` 内容或业务数据样本。
- 所有破坏性清理都先以 Compose label 和基线确认资源归属。
- `p3-restore-plan` 与 `p3-restore-execute` 已完成并归档或处于可追溯完成态。
