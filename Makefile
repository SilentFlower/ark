BINARY      := ark
HUB_BINARY  := ark-hub
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE        ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

PKG         := github.com/silentflower/ark/internal/cli
LDFLAGS     := -s -w -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT) -X $(PKG).Date=$(DATE)

WEB_DIR     := web
WEB_DIST    := internal/hub/webui/dist

.PHONY: all build hub test vet fmt check clean install \
	web-install web-build web-check web-lint web-typecheck web-test

all: check build

## build: 编译 ark oneshot 二进制到 bin/
build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/$(BINARY)

## hub: 构建前端并编译 ark-hub 单二进制到 bin/
## 发布 ark-hub 必须走这个目标：直接 go build 得到的二进制里只有占位页，
## 因为前端产物要先由 Vite 写进 $(WEB_DIST)。
hub: web-build
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(HUB_BINARY) ./cmd/$(HUB_BINARY)

## web-install: 按 lockfile 安装前端依赖
web-install:
	pnpm --dir $(WEB_DIR) install --frozen-lockfile

## web-build: 构建前端并同步到 internal/hub/webui/dist
## 分两步是有原因的：Vite 的 emptyOutDir 会清空输出目录，直接输出到嵌入目录会连
## PLACEHOLDER 一起删掉，而那个文件是 `go:embed dist` 在没跑过前端构建的环境里
## 能编译的唯一保证。这里先清掉旧产物（保留 PLACEHOLDER）再拷贝新产物。
web-build:
	pnpm --dir $(WEB_DIR) run build
	find $(WEB_DIST) -mindepth 1 ! -name PLACEHOLDER -exec rm -rf {} +
	cp -R $(WEB_DIR)/dist/. $(WEB_DIST)/

## web-lint: 前端 ESLint
web-lint:
	pnpm --dir $(WEB_DIR) run lint

## web-typecheck: 前端类型检查
web-typecheck:
	pnpm --dir $(WEB_DIR) run typecheck

## web-test: 前端单元测试
web-test:
	pnpm --dir $(WEB_DIR) run test

## web-check: 前端提交门槛（lint + 类型检查 + 单测）
web-check: web-lint web-typecheck web-test

## test: 跑单元测试
test:
	go test ./... -race -count=1

## vet: 静态检查
vet:
	go vet ./...

## fmt: 格式化并检查是否有未格式化的文件
fmt:
	gofmt -w .
	@test -z "$$(gofmt -l .)" || (echo "以下文件未格式化:"; gofmt -l .; exit 1)

## check: 提交前的完整检查
## 刻意只跑 Go：check 必须能在没有 node 的机器上通过。
## 改动了 web/ 时另外跑 make web-check。
check: fmt vet test

## install: 安装到 /usr/local/bin
install: build
	install -m 0755 bin/$(BINARY) /usr/local/bin/$(BINARY)

## clean: 清理构建产物（保留 $(WEB_DIST)/PLACEHOLDER，否则 go build 会失败）
clean:
	rm -rf bin dist $(WEB_DIR)/dist
	find $(WEB_DIST) -mindepth 1 ! -name PLACEHOLDER -exec rm -rf {} +
