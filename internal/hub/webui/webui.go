// Package webui 持有 ark-hub 控制台的前端构建产物，并把它作为只读静态资源提供出去。
//
// 这个包刻意只做一件事：嵌入 web/ 的 Vite 产物，按正确的 Content-Type 与缓存策略
// 返回文件，并在路径不命中任何文件时交回 SPA 入口 HTML。它不认识 store、config、
// 会话或任何业务概念——鉴权由 internal/hub 在调用本包之前完成。
//
// 产物目录 dist/ 由 `make hub`（先 pnpm build 再 go build）生成。仓库里提交了一份
// 占位 index.html，用于保证在没有 node 环境时 `go build ./...` 依然能编译；用
// `go build` 而不是 `make hub` 得到的二进制会显示那份占位页。
package webui

import (
	"embed"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

//go:embed dist
var embedded embed.FS

// placeholder 是没有真实前端产物时返回的入口页面。
//
// 它的存在是为了让 `go build ./...` 与 `go test ./...` 在没有 node 环境时照常工作：
// dist/ 里只有一个占位文件，index.html 要等 `make hub` 才会出现。
// 用明确的提示页而不是启动失败，是因为前端缺失不影响 API、登录和备份调度，
// 把整个 Hub 拦在门外反而是过度反应。
//
//go:embed placeholder.html
var placeholder []byte

const (
	// indexPath 是 SPA 入口文件在产物中的路径。
	indexPath = "index.html"
	// immutableCacheControl 用于带内容 hash 的资源：文件名变了才会重新请求，
	// 因此可以放心长期缓存。没有 hash 的入口 HTML 不能用这个值。
	immutableCacheControl = "public, max-age=31536000, immutable"
)

// Assets 返回嵌入产物的只读文件系统，根目录是 dist/。
// @return fs.FS 以 dist/ 为根的文件系统。
// @return error 嵌入内容结构异常时返回错误。
func Assets() (fs.FS, error) {
	assets, err := fs.Sub(embedded, "dist")
	if err != nil {
		return nil, fmt.Errorf("读取内嵌前端产物失败: %w", err)
	}
	return assets, nil
}

// IndexHTML 返回 SPA 入口 HTML 的内容。
//
// 产物里没有 index.html 时返回占位页，因此这个函数在任何构建方式下都能成功。
// @return []byte 入口 HTML。
// @return error 嵌入内容结构异常时返回错误。
func IndexHTML() ([]byte, error) {
	assets, err := Assets()
	if err != nil {
		return nil, err
	}
	return indexFrom(assets), nil
}

// Built 报告二进制里是否带有真实的前端产物。
// @return bool true 表示由 make hub 构建，false 表示只有占位页。
func Built() bool {
	assets, err := Assets()
	if err != nil {
		return false
	}
	_, err = fs.ReadFile(assets, indexPath)
	return err == nil
}

// indexFrom 读取入口 HTML，缺失时回落到占位页。
func indexFrom(assets fs.FS) []byte {
	body, err := fs.ReadFile(assets, indexPath)
	if err != nil {
		return placeholder
	}
	return body
}

// Handler 是前端产物的 SPA handler。
//
// 调用方必须在此之前完成鉴权：本 handler 不做任何身份判断，命中即返回内容。
type Handler struct {
	assets fs.FS
	index  []byte
}

// NewHandler 构造前端 handler，并在构造期就确认入口 HTML 可读。
//
// 把校验放在构造期而不是请求期，是为了让产物损坏在 ark-hub 启动时就暴露，
// 而不是等到管理员打开页面才发现。
// @return *Handler 可直接挂到 mux 上的 handler。
// @return error 产物不可读时返回错误。
// NewHandler 构造前端 handler。
//
// 入口 HTML 在构造期就解析出来：真实产物缺失时使用占位页，因此本函数只在
// 嵌入内容本身损坏时失败。
// @return *Handler 可直接挂到 mux 上的 handler。
// @return error 嵌入内容不可读时返回错误。
func NewHandler() (*Handler, error) {
	assets, err := Assets()
	if err != nil {
		return nil, err
	}
	return newHandler(assets)
}

// newHandler 从任意只读文件系统构造 handler，供测试注入固定产物。
func newHandler(assets fs.FS) (*Handler, error) {
	if assets == nil {
		return nil, fmt.Errorf("构造前端 handler 失败: 产物文件系统为空")
	}
	return &Handler{assets: assets, index: indexFrom(assets)}, nil
}

// ServeHTTP 按请求路径返回嵌入文件，未命中任何文件时返回 SPA 入口。
//
// 未命中就回落到 index.html 是 history 模式路由的要求：`/hosts/web-01` 这样的
// 前端路径在产物里没有对应文件，必须由前端路由接管。调用方负责保证 /api/ 前缀
// 的请求不会走到这里，否则 API 的 404 会变成一份 HTML。
func (handler *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	name := strings.TrimPrefix(path.Clean(request.URL.Path), "/")
	if name == "" || name == "." {
		handler.writeIndex(writer)
		return
	}
	// fs.ValidPath 拒绝 "..", 绝对路径和空路径元素，是这里唯一需要的穿越防护。
	if !fs.ValidPath(name) {
		handler.writeIndex(writer)
		return
	}
	body, err := fs.ReadFile(handler.assets, name)
	if err != nil {
		handler.writeIndex(writer)
		return
	}
	contentType := mime.TypeByExtension(path.Ext(name))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	writer.Header().Set("Content-Type", contentType)
	// 只有带内容 hash 的产物才允许长期缓存；入口 HTML 走 writeIndex 的 no-store。
	writer.Header().Set("Cache-Control", immutableCacheControl)
	writer.WriteHeader(http.StatusOK)
	// 客户端断开后的写错误已无法改变响应，且本阶段 hub 不引入日志框架。
	_, _ = writer.Write(body)
}

// writeIndex 返回 SPA 入口。入口 HTML 不带内容 hash，必须禁止缓存，
// 否则升级 ark-hub 之后浏览器会继续加载旧版本引用的 JS 文件名。
func (handler *Handler) writeIndex(writer http.ResponseWriter) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(handler.index)
}
