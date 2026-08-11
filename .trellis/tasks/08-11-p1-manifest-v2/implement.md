# 执行计划

## 步骤

1. **`internal/config/config.go` 重写结构与加载**
   - `SchemaVersion` 1 → 2
   - 新增 `Defaults` / `Host` / `SSH`，`Config` 顶层改为 `repo` + `defaults` + `hosts`
   - `Load` 改为两遍解析（版本探测 → 严格解析）
   - `applyDefaults` 收缩为 repo.type + defaults 两项
   - 新增 `ScheduleFor` / `RetentionFor`

2. **`internal/config/config.go` 重写校验**
   - `Validate` 拆出 `validateDefaults` / `validateHost` / `validateSSH`
   - `validateProject` / `validateTargets` / `validateTargetRequired` 改为接收
     带下标的前缀，逻辑不变
   - `validateSchedule` / `validateRetention` 改为对可空值校验

3. **`internal/doctor/doctor.go` 最小适配**
   - `Run` 遍历 hosts，local 跑全量本地检查，远程只跑 hub 侧可判断项
   - `checkTargets` / `composeServices` 参数从 `*config.Config` 改为 `*config.Host`

4. **`internal/cli/root.go` 适配 validate 输出**

5. **`examples/ark.yaml` 重写为多机清单**（含一台 `local: true`）

6. **`internal/config/config_test.go` 重写**（按 design.md §7）

7. **README `validate` 用法段同步**

8. **`make check` + `./bin/ark validate -c examples/ark.yaml` 验证**

## 顺序理由

1→2 在同一个文件里、彼此耦合，但先落结构再落校验能让编译器把
所有需要跟着改的地方指出来。3→4 是被动适配，必须等 1 完成才知道改什么。
5 依赖最终的字段名，6 依赖 5（示例清单是测试用例的现实参照）。

## 风险

- **doctor 的适配面比预期大**：`checkTargets` 依赖 compose 上下文，
  从 Config 挪到 Host 时容易漏掉 `ProjectName` / `EnvFile` 的传递。
  完成后手动跑一次 `doctor` 确认输出结构正常。
- **测试常量牵一发动全身**：`validManifest` 改成多机后，
  现有用例里的 `strings.Replace` 锚点全部失效，需要逐条重写而不是微调。
