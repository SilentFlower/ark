# 执行计划

## 步骤

1. **重构 doctor 公共入口与共享判定**
   - 保留 `Status`、`Check`、`Report` 的外部结构
   - 将旧 `Run` 改为 `RunLocal`
   - 提取路径元数据与存在性、普通文件、权限位的共用判定
   - 更新 package doc，删除远程检查尚未实现的过渡说明

2. **完成 RunLocal 与仓库解锁检查**
   - hub 二进制、repo 文件、SSH 文件、schedule 检查按依赖重排
   - 实现严格的 repo env 文件读取和子进程环境合并
   - 执行带 15 秒超时的 `restic cat config`
   - 覆盖敏感值不泄露、依赖失败降级和环境覆盖测试

3. **新增 `doctor_remote.go` 与 RunHost**
   - 根据 `Host.Local` 选择 `NewLocal` / `NewSSH`
   - 增加连接与 60 秒时钟偏移检查
   - 通过 Runner 检查 docker、compose、项目文件、services、volumes 和 paths
   - 按依赖图只降级无法判断的检查，继续执行独立项
   - 增加假 Runner 与可选真实 host 集成测试

4. **改造 doctor CLI 范围选择**
   - 新增 `--host` / `--all` 并声明互斥
   - 默认只运行 `RunLocal`，`--all` 串行合并全部报告
   - 未知 host 返回工具错误，保持 JSON、文本摘要和退出码语义
   - 新增 CLI orchestration 测试

5. **同步用户文档与 roadmap**
   - 更新 README 当前状态、快速开始和 doctor 命令示例
   - 更新 `examples/ark.yaml` 顶部 doctor 示例注释
   - 标记 roadmap CLI 项完成并删除远程检查过渡说明

6. **验证**
   - `go test ./internal/doctor -race -count=1`
   - `go test ./internal/cli -race -count=1`
   - `go test ./internal/sshexec -race -count=1`
   - `make check`
   - 配置真实测试清单时运行
     `go test ./internal/doctor -run TestRunHostIntegration -count=1 -v`
   - 用构建产物验证 `doctor` 默认、`--host`、`--all` 和 `--json`

## 顺序理由

先固定报告和文件判定，再分别实现 hub 与 host 检查，能让依赖降级和测试复用建立在
稳定边界上。CLI 最后接入两个已验证入口，避免命令层同时承担业务调试。

## 高风险文件与回滚点

- `internal/doctor/doctor.go`：默认检查范围和 restic 凭证环境发生变化，必须重点审计
  env 泄露、重复 fail 与 15 秒超时。
- `internal/doctor/doctor_remote.go`：错误依赖图会把“未检查”误报成 fail/ok，
  或在 docker 失败后跳过本可独立检查的 files target。
- `internal/cli/root.go`：默认 doctor 行为变化且影响退出码；必须覆盖三种范围和 JSON。
- repo env parser：不得执行 shell 语法，不得把解析失败的值写入日志；P2-2 接入时需要
  重新确认只保留一个实现。

## 提交前检查

- 所有导出函数和类型有中文文档注释及参数、返回值说明。
- `RunLocal` 不检查 host 项目环境，`RunHost` 不检查 restic 或 systemd。
- remote command 全部通过 `sshexec.Runner`，没有 doctor 自行拼 SSH 或 shell。
- 登录、docker、compose、文件和 target 的 fail/warn 依赖符合 PRD 矩阵。
- 60 秒时钟阈值与中点算法有边界测试。
- restic 凭证只存在于目标子进程 Env，测试和错误输出不含值。
- `--all` 在单台失败后继续，并保持清单顺序。
- README、示例注释和 roadmap 与默认 local 行为一致。
- `make check` 全绿。
