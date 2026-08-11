# 执行计划

## 步骤

1. 定义 schema v1 的 Manifest/Host/Target 类型和 Validate。
2. 实现稳定 JSON 编解码和往返/非法输入测试。
3. 实现 manifest filename/tag 构造与 restic 写入。
4. 实现 snapshots 选择、Dump、run tag 核对和最新读取。
5. 与 P2-3/P2-4 结果模型对齐，避免重复字段转换逻辑。

## 验证

```bash
go test ./internal/backup ./internal/restic -race -count=1
make check
make build
CGO_ENABLED=0 go build ./cmd/ark
git diff --check
```

## 高风险点

- manifest 是 P3 恢复入口，schema 不能靠实现细节隐式变化。
- stable filename、tags、run ID 和 snapshot ID 必须相互一致。
- 不能把 map 的随机遍历顺序写进持久化 JSON。
