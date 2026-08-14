BINARY      := ark
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE        ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

PKG         := github.com/silentflower/ark/internal/cli
LDFLAGS     := -s -w -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT) -X $(PKG).Date=$(DATE)

.PHONY: all build test vet fmt check clean install

all: check build

## build: 编译 ark oneshot 二进制到 bin/
build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/$(BINARY)

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
check: fmt vet test

## install: 安装到 /usr/local/bin
install: build
	install -m 0755 bin/$(BINARY) /usr/local/bin/$(BINARY)

## clean: 清理构建产物
clean:
	rm -rf bin dist
