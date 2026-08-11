# 技术设计：P2-6 backup CLI 与 systemd

## 1. CLI 边界

`newRootCmd` 注册 backup/install 子命令。复杂编排放在可注入依赖的内部函数，cobra 层只做
参数、输出和退出语义；测试不替换全局 stdout/stderr。

## 2. Run 状态机

```text
validate -> optional dry-run
  -> acquire flock
  -> local doctor
  -> open store / create run / ensure repo
  -> each host: doctor -> each target -> aggregate
  -> write manifest
  -> forget/prune
  -> finish run
```

每个阶段都在已有事实基础上记录结果。清理、FinishRun 或 Close 失败通过错误组合返回，
不能覆盖原始业务失败。

## 3. 依赖注入

沿用 doctor CLI 的内部函数注入模式，为测试注入 load config、doctor、runner factory、Repo、
Store、clock/run ID、lock 和 unit writer。接口保持包内，避免形成公开框架。

## 4. Dry-run

dry-run 只基于已静态校验清单生成计划，显示 host、target、稳定 filename、标签和保留策略；
不读取凭证文件、不执行 doctor/SSH/restic、不打开状态库、不获取 flock、不写 unit。

## 5. systemd

unit 模板与文件写入放在 `internal/systemd`。service 只调用 ark 二进制，不嵌入配置内容。
手动全量运行使用 `ark-backup.service`；自动运行使用 `ark-backup@.service` 模板和
每 host 一个 timer。host 名已受 config 正则限制，可稳定映射为 instance 名；timer 使用
`ScheduleFor(host)`，从而兑现 host 覆盖。

所有 service 共用全局 flock。`RandomizedDelaySec=600` 只降低碰撞概率，不替代锁；
碰撞必须由 systemd 非零结果和后续死人开关暴露。install 扫描同前缀 timer 时只处理
带固定 ark 管理标记的文件，并以当前 host 集合删除陈旧项。

## 6. 回滚

install 使用临时文件 + fsync/close + rename，任一模板或 verify 失败保持旧 unit 不变。
业务 backup 不回滚已成功 target；partial 事实写入状态和 manifest，退出码非零。
