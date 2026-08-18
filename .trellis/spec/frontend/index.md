# Frontend Development Guidelines

> `web/` 的编码规约。所有内容来自本仓库的真实代码，不是通用建议。

---

## Overview

`web/` 是 `ark-hub` 控制台的前端，Vue 3 + Vite + TypeScript + Pinia + Tailwind，
构建产物由 `internal/hub/webui` 用 `go:embed` 打进 `ark-hub` 单二进制，
部署环境不需要 node。

前端要理解的前提和后端相同：**ark 的失败是延迟暴露的**。控制台的职责不是好看，
而是让「这台机器三个月没成功备份了」「这次恢复会覆盖生产数据」这类事实
无法被忽略、也无法被误读成正常。所有偏严的规定都源于此。

后端契约见 `.trellis/spec/backend/hub-guidelines.md`，架构见 `docs/design.md` §9。

---

## Guidelines Index

| Guide | Description | Status |
|-------|-------------|--------|
| [Web Guidelines](./web-guidelines.md) | 目录、状态管理、API 契约、恢复确认、CSP 约束与测试 | Filled |

---

## Pre-Development Checklist

动手改 `web/` 之前：

1. 读 [Web Guidelines](./web-guidelines.md)，尤其是「恢复确认 token」一节——
   那是全项目最容易写错、且错了不会立刻发现的一段前端逻辑。
2. 确认要用的后端字段真实存在：`src/api/types.ts` 必须与
   `internal/hub/query.go`、`internal/hub/health.go`、`internal/hub/operation.go` 逐字段对齐。
3. 确认新代码不会破坏 CSP：不引入 CDN、不写内联 `<script>`、不用 `eval`。
4. 提交前跑 `make web-check`（lint + 类型检查 + 单测）与 `make check`（纯 Go）。

---

**Language**: 文档标题保持英文，正文用中文。代码注释、错误信息、
界面文案一律中文。
