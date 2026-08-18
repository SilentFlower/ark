package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// sampleAssets 模拟一份真实的 Vite 产物：入口 HTML 加两个带内容 hash 的资源。
func sampleAssets() fstest.MapFS {
	return fstest.MapFS{
		"index.html":              &fstest.MapFile{Data: []byte("<!doctype html><div id=app></div>")},
		"assets/index-abc123.js":  &fstest.MapFile{Data: []byte("console.log(1)")},
		"assets/index-def456.css": &fstest.MapFile{Data: []byte(".a{color:red}")},
		"favicon.svg":             &fstest.MapFile{Data: []byte("<svg/>")},
		"nested/deep/thing.txt":   &fstest.MapFile{Data: []byte("hello")},
	}
}

func newTestHandler(t *testing.T) *Handler {
	t.Helper()
	handler, err := newHandler(sampleAssets())
	if err != nil {
		t.Fatalf("构造 handler 失败: %v", err)
	}
	return handler
}

func do(t *testing.T, handler *Handler, target string) *http.Response {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
	return recorder.Result()
}

// TestNewHandler_FallsBackToPlaceholder 覆盖「只跑了 go build、没跑 make hub」这条路径：
// 前端缺失不应该让 Hub 起不来，但页面必须说清楚缺的是什么、怎么补。
func TestNewHandler_FallsBackToPlaceholder(t *testing.T) {
	handler, err := newHandler(fstest.MapFS{"PLACEHOLDER": &fstest.MapFile{Data: []byte("x")}})
	if err != nil {
		t.Fatalf("缺少真实产物时仍应构造成功: %v", err)
	}
	if !strings.Contains(string(handler.index), "make hub") {
		t.Fatalf("占位页必须提示 make hub，实际 = %q", handler.index)
	}
	response := do(t, handler, "/")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("占位页 status = %d", response.StatusCode)
	}
}

// TestNewHandler_EmbeddedAssets 校验仓库里真实嵌入的内容可用。
// 无论是否跑过前端构建，这一条都必须通过。
func TestNewHandler_EmbeddedAssets(t *testing.T) {
	handler, err := NewHandler()
	if err != nil {
		t.Fatalf("嵌入产物不可用: %v", err)
	}
	if len(handler.index) == 0 {
		t.Fatal("嵌入的入口 HTML 为空")
	}
	if _, err := IndexHTML(); err != nil {
		t.Fatalf("IndexHTML 失败: %v", err)
	}
	if _, err := Assets(); err != nil {
		t.Fatalf("Assets 失败: %v", err)
	}
	// Built 只是报告状态，两种取值都合法；这里确认它与入口内容一致。
	if Built() == strings.Contains(string(handler.index), "make hub") {
		t.Fatal("Built 与入口 HTML 的实际来源不一致")
	}
}

func TestHandler_ServesAssets(t *testing.T) {
	handler := newTestHandler(t)

	tests := []struct {
		name            string
		target          string
		wantBody        string
		wantContentType string
	}{
		{
			name:            "js 资源带正确类型",
			target:          "/assets/index-abc123.js",
			wantBody:        "console.log(1)",
			wantContentType: "javascript",
		},
		{
			name:            "css 资源带正确类型",
			target:          "/assets/index-def456.css",
			wantBody:        ".a{color:red}",
			wantContentType: "text/css",
		},
		{
			name:            "根目录之外的静态文件同样可取",
			target:          "/favicon.svg",
			wantBody:        "<svg/>",
			wantContentType: "image/svg+xml",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			response := do(t, handler, tc.target)
			if response.StatusCode != http.StatusOK {
				t.Fatalf("状态码 = %d，期望 200", response.StatusCode)
			}
			if got := response.Header.Get("Content-Type"); !strings.Contains(got, tc.wantContentType) {
				t.Errorf("Content-Type = %q，期望包含 %q", got, tc.wantContentType)
			}
			if got := response.Header.Get("Cache-Control"); got != immutableCacheControl {
				t.Errorf("Cache-Control = %q，期望 %q", got, immutableCacheControl)
			}
			body := make([]byte, len(tc.wantBody))
			if _, err := response.Body.Read(body); err != nil && err.Error() != "EOF" {
				t.Fatalf("读取响应体失败: %v", err)
			}
			if string(body) != tc.wantBody {
				t.Errorf("响应体 = %q，期望 %q", body, tc.wantBody)
			}
		})
	}
}

// TestHandler_IndexIsNotCached 是升级安全的关键：入口 HTML 一旦被缓存，
// 升级 ark-hub 之后浏览器会继续加载旧版本引用的 JS 文件名。
func TestHandler_IndexIsNotCached(t *testing.T) {
	response := do(t, newTestHandler(t), "/")
	if got := response.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("入口 HTML 的 Cache-Control = %q，期望 no-store", got)
	}
	if got := response.Header.Get("Content-Type"); !strings.Contains(got, "text/html") {
		t.Errorf("入口 HTML 的 Content-Type = %q", got)
	}
}

// TestHandler_SPAFallback 覆盖 history 模式路由：前端路径在产物里没有对应文件，
// 必须回落到入口 HTML 交给前端路由接管。
func TestHandler_SPAFallback(t *testing.T) {
	handler := newTestHandler(t)
	for _, target := range []string{"/hosts/web-01", "/alerts", "/operations", "/completely/unknown"} {
		t.Run(target, func(t *testing.T) {
			response := do(t, handler, target)
			if response.StatusCode != http.StatusOK {
				t.Fatalf("状态码 = %d，期望 200", response.StatusCode)
			}
			if got := response.Header.Get("Content-Type"); !strings.Contains(got, "text/html") {
				t.Errorf("fallback 应返回 HTML，实际 Content-Type = %q", got)
			}
			if got := response.Header.Get("Cache-Control"); got != "no-store" {
				t.Errorf("fallback 的 Cache-Control = %q，期望 no-store", got)
			}
		})
	}
}

// TestHandler_RejectsTraversal 保证请求路径不能逃出嵌入产物。
// 这里的期望不是 404 而是 fallback：穿越尝试与普通未命中路径得到完全相同的响应，
// 攻击者无法据此判断某个路径是否存在。
func TestHandler_RejectsTraversal(t *testing.T) {
	handler := newTestHandler(t)
	targets := []string{
		"/../webui.go",
		"/assets/../../webui.go",
		"/nested/../../etc/passwd",
		"/./../../go.mod",
	}
	for _, target := range targets {
		t.Run(target, func(t *testing.T) {
			response := do(t, handler, target)
			if got := response.Header.Get("Content-Type"); !strings.Contains(got, "text/html") {
				t.Fatalf("穿越路径必须回落到入口 HTML，实际 Content-Type = %q", got)
			}
			if got := response.Header.Get("Cache-Control"); got == immutableCacheControl {
				t.Fatal("穿越路径不得命中静态资源分支")
			}
		})
	}
}
