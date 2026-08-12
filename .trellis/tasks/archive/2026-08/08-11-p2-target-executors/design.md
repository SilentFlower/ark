# 技术设计：P2-3 target 执行器

## 1. 分发边界

`executor.go` 根据 `config.Target.Type` 选择非导出的类型实现，并统一构造 compose 前缀、
stable filename 和结果结构。调用方提供已经选择好的 `sshexec.Runner`，执行器不判断
host 是 local 还是 SSH。

## 2. 流生命周期

执行器返回 `sshexec.Runner.Stream` 的 reader 与 Wait，不提前消费或隐藏 Wait。
P2-4 负责把 reader 交给 restic 后按正确顺序校验 Wait。启动失败时不返回半初始化结果；
reader Close 和 Wait 仍由统一结果封装保证至多一次。

## 3. 各类型策略

- postgres：单条 compose exec 命令，纯文本 SQL。
- redis：Run(LASTSAVE) → Run(BGSAVE) → 有界轮询 Run(LASTSAVE) → Stream(cat RDB)。
- volume：一次 docker run tar 流，只读挂载。
- files：一次 tar 流，路径逐参数传递。
- image_digest：Run 查询 compose 容器，再 Run inspect 镜像；结构化解析后生成稳定 JSON reader。

## 4. 测试替身

使用实现 `sshexec.Runner` 的可控 fake，记录 Run/Stream/Feed 调用并按步骤返回结果。
不暴露新的 mock 接口。测试覆盖 shell 元字符、单引号、空格、取消和阶段错误。

## 5. 延后

真实 docker/SSH/数据库产流、pg_dump 被 kill 和对象存储快照检查统一留给
`p2-live-validation`，不能把单测写成真实验收已通过。
