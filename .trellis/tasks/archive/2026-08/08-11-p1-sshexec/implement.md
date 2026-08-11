# 执行计划

## 步骤

1. **新增 `internal/sshexec/client.go` 的包与公共契约**
   - 写 package doc、`Runner`、`NewLocal`、`NewSSH`
   - 新增非导出 `localRunner` / `sshRunner`
   - 构造阶段解析并校验 `config.SSH`

2. **实现安全的命令构造**
   - 本地路径直接传递 argv
   - SSH 路径固定 `-T`、Compression、BatchMode、StrictHostKeyChecking、
     UserKnownHostsFile、IdentitiesOnly、identity、port、user
   - 实现逐参数 `shellQuote` 与单条 remote command 构造

3. **实现 Run / Stream / Feed 生命周期**
   - `Run` 用 CombinedOutput
   - `Stream` 分离 stdout/stderr，Start 后返回 reader + Wait
   - `Feed` 流式接 stdin，不整读、不落盘
   - 统一空 argv、退出状态和 context 错误包装

4. **新增 `internal/sshexec/client_test.go`**
   - helper process 驱动本地/SSH 单元测试
   - 覆盖转义、固定参数、数据流隔离、非零退出、SIGKILL、context 取消
   - 增加显式环境配置的 localhost 集成测试，并用 `testing.Short()` 保护

5. **更新 `internal/doctor/doctor.go`**
   - hub 全局二进制检查新增 `ssh -V`
   - 不改变当前本地/远程 host 的过渡检查逻辑

6. **验证**
   - `go test ./internal/sshexec -race -count=1`
   - `go test ./internal/doctor -race -count=1`
   - `make check`
   - 若本机具备专用 localhost SSH 测试账号，设置四个 `ARK_SSH_TEST_*` 环境变量后
     单独运行 `go test ./internal/sshexec -run TestSSHIntegration_Localhost -count=1`

## 顺序理由

先固定构造与生命周期契约，再写测试，可以让测试围绕稳定的行为边界展开；
doctor 只消费“系统 ssh 已成为运行时依赖”这一事实，放在执行层稳定后做最小接入。

## 高风险文件与回滚点

- `internal/sshexec/client.go`：远程转义或 stdout/stderr 接线错误会产生命令注入或
  静默损坏的备份流。每种模式独立完成测试后再进入下一种。
- `internal/sshexec/client_test.go`：helper process 必须避免依赖 shell 来验证本地 argv，
  否则测试本身会掩盖参数边界问题。
- `internal/doctor/doctor.go`：只增加一项全局检查；若报告行为产生意外变化，
  可单独回退该行，不影响 `sshexec` 包。

## 提交前检查

- 所有导出标识符有中文文档注释，复杂生命周期逻辑解释 why。
- 本地执行路径没有 `sh -c` / `bash -c`。
- SSH 远程命令的每个 argv 都经过单独转义，没有双层 shell。
- `Stream` 的测试明确先读完 stdout 再调用 Wait，并验证失败不会伪装成功。
- context 取消/超时可通过 `errors.Is` 判断。
- 错误与测试输出不含任何密钥文件内容或环境变量集合。
- `make check` 通过。
