package hub

import (
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestWebConsole_未认证不泄露控制台入口 是本轮最重要的一条负向测试。
//
// SPA fallback 把「所有未知路径都返回 index.html」引进了 Hub，一旦分流写反，
// 未登录的人就能直接拿到控制台入口和它引用的全部资源路径。鉴权必须在 fallback 之前。
func TestWebConsole_未认证不泄露控制台入口(t *testing.T) {
	application, _ := newHTTPTestApplication(t, false, time.Now)
	handler := application.handler()

	pageTargets := []string{"/", "/hosts/web-01", "/alerts", "/operations", "/assets/index-abc.js"}
	for _, target := range pageTargets {
		t.Run("页面 "+target, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
			if response.Code != http.StatusSeeOther ||
				response.Header().Get("Location") != "/login" {
				t.Fatalf("status=%d location=%q", response.Code, response.Header().Get("Location"))
			}
			if strings.Contains(response.Body.String(), "<!doctype html>") {
				t.Fatalf("未认证响应泄露了页面内容: %q", response.Body.String())
			}
		})
	}

	t.Run("API 未知路径返回 401 JSON", func(t *testing.T) {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/does-not-exist", nil))
		if response.Code != http.StatusUnauthorized ||
			!strings.Contains(response.Body.String(), `"authenticated":false`) {
			t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
		}
	})
}

// TestWebConsole_登录后按路径分流 覆盖 history 模式路由与 API 404 的边界：
// 前端路径要拿到入口 HTML，API 的未知路径必须仍然是 JSON，
// 否则客户端会把一份 HTML 当成数据解析。
func TestWebConsole_登录后按路径分流(t *testing.T) {
	application, _ := newHTTPTestApplication(t, false, time.Now)
	handler := application.handler()
	sessionCookie := loginHTTPTestUser(t, handler)

	t.Run("前端路径回落到入口 HTML", func(t *testing.T) {
		for _, target := range []string{"/hosts/web-01", "/alerts", "/operations", "/unknown/deep/path"} {
			request := httptest.NewRequest(http.MethodGet, target, nil)
			request.AddCookie(sessionCookie)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK ||
				!strings.Contains(response.Header().Get("Content-Type"), "text/html") {
				t.Fatalf("%s status=%d content-type=%q", target, response.Code, response.Header().Get("Content-Type"))
			}
			if !strings.Contains(response.Body.String(), "<!doctype html>") {
				t.Fatalf("%s 未返回入口 HTML", target)
			}
		}
	})

	t.Run("API 未知路径返回 404 JSON", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "/api/does-not-exist", nil)
		request.AddCookie(sessionCookie)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("status=%d", response.Code)
		}
		if !strings.Contains(response.Header().Get("Content-Type"), "application/json") ||
			!strings.Contains(response.Body.String(), `"not_found"`) {
			t.Fatalf("content-type=%q body=%q", response.Header().Get("Content-Type"), response.Body.String())
		}
		if strings.Contains(response.Body.String(), "<!doctype html>") {
			t.Fatal("API 404 不得返回 HTML")
		}
	})

	t.Run("入口 HTML 禁止缓存", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.AddCookie(sessionCookie)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if got := response.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("入口 HTML Cache-Control = %q，期望 no-store", got)
		}
	})
}

// TestWebConsole_内容安全策略 精确断言 CSP。
//
// 这条测试的价值在于让任何一次 CSP 变更都必须显式改测试：收紧会让控制台白屏，
// 放宽会悄悄扩大脚本来源，两种方向都不能靠 review 漏掉。
func TestWebConsole_内容安全策略(t *testing.T) {
	application, _ := newHTTPTestApplication(t, false, time.Now)
	handler := application.handler()

	const expected = "default-src 'none'; script-src 'self'; " +
		"style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; " +
		"connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

	for _, target := range []string{"/healthz", "/login", "/"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
		if got := response.Header().Get("Content-Security-Policy"); got != expected {
			t.Fatalf("%s 的 CSP = %q，期望 %q", target, got, expected)
		}
	}
}

// TestWebConsole_登录页样式内联可用 确认登录页在新 CSP 下仍能正常渲染。
// 登录页刻意保持服务端渲染，样式内联依赖 style-src 的 'unsafe-inline'。
func TestWebConsole_登录页样式内联可用(t *testing.T) {
	application, _ := newHTTPTestApplication(t, false, time.Now)
	response := httptest.NewRecorder()
	application.handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/login", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("登录页 status=%d", response.Code)
	}
	body := response.Body.String()
	if !strings.Contains(body, "<style>") || !strings.Contains(body, `action="/login"`) {
		t.Fatalf("登录页内容异常: %q", body)
	}
	if strings.Contains(body, "<script") {
		t.Fatal("登录页不应引入脚本")
	}
}

// TestWebConsole_登录CSRFCookie稳定且续期 复现真实浏览器里的两种登录失败。
//
// 其一：浏览器打开登录页时会顺带请求 /favicon.ico，该请求同样被重定向到登录页并触发
// 一次渲染。若每次渲染都轮换 Cookie 值，用户点提交时带的 Cookie 已被那次后台请求
// 覆盖，而表单里还是旧 token，登录稳定失败在 403。
//
// 其二：只复用值却不刷新过期时间，用户会拿到一个即将到期的 Cookie；页面停留一会儿
// 再提交，Cookie 已被浏览器丢弃，请求根本不带 Cookie，同样是 403。
//
// httptest 不会请求 favicon、也不会让 Cookie 真的过期，这两个缺陷都只有真浏览器
// 能暴露，必须由这条测试守住。
func TestWebConsole_登录CSRFCookie稳定且续期(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	current := now
	application, _ := newHTTPTestApplication(t, false, func() time.Time { return current })
	handler := application.handler()

	csrfCookie := getLoginCSRFCookie(t, handler)

	// 模拟浏览器随后对子资源的请求：它被重定向到 /login 并再次渲染。
	faviconRequest := httptest.NewRequest(http.MethodGet, "/favicon.ico", nil)
	faviconRequest.AddCookie(csrfCookie)
	handler.ServeHTTP(httptest.NewRecorder(), faviconRequest)

	// 页面在浏览器里停留了一会儿，用户刷新了一次登录页。
	current = now.Add(9 * time.Minute)
	secondRequest := httptest.NewRequest(http.MethodGet, "/login", nil)
	secondRequest.AddCookie(csrfCookie)
	secondResponse := httptest.NewRecorder()
	handler.ServeHTTP(secondResponse, secondRequest)

	renewed := responseCookie(t, secondResponse, loginCSRFCookieName)
	if renewed.Value != csrfCookie.Value {
		t.Fatal("已有合法 Cookie 时不得轮换登录 CSRF token")
	}
	if !renewed.Expires.After(current) {
		t.Fatalf("登录 CSRF Cookie 未续期: expires=%v now=%v", renewed.Expires, current)
	}
	if !strings.Contains(secondResponse.Body.String(), csrfCookie.Value) {
		t.Fatal("再次渲染的登录页未复用已有 CSRF token")
	}

	// 用最初那次渲染得到的 token 提交，必须能登录成功。
	loginRequest := postFormRequest("/login", url.Values{
		"username":   {"admin"},
		"password":   {string(testPassword)},
		"csrf_token": {csrfCookie.Value},
	})
	loginRequest.AddCookie(csrfCookie)
	loginResponse := httptest.NewRecorder()
	handler.ServeHTTP(loginResponse, loginRequest)
	if loginResponse.Code != http.StatusSeeOther {
		t.Fatalf("登录 status=%d body=%q", loginResponse.Code, loginResponse.Body.String())
	}
}

// TestWebConsole_非法登录CSRFCookie会被替换 保证复用只针对本服务签发的 token 形态，
// 伪造或损坏的 Cookie 不会被原样接受进表单。
func TestWebConsole_非法登录CSRFCookie会被替换(t *testing.T) {
	application, _ := newHTTPTestApplication(t, false, time.Now)
	handler := application.handler()

	for _, value := range []string{"", "short", strings.Repeat("x", 44), "invalid/token+chars=========================="} {
		request := httptest.NewRequest(http.MethodGet, "/login", nil)
		request.AddCookie(&http.Cookie{Name: loginCSRFCookieName, Value: value})
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		issued := responseCookie(t, response, loginCSRFCookieName)
		if issued.Value == value {
			t.Fatalf("非法 Cookie %q 未被替换", value)
		}
		if value != "" && strings.Contains(response.Body.String(), value) {
			t.Fatalf("非法 Cookie %q 被写进了登录表单", value)
		}
	}
}

// TestWebConsole_应用构造加载内嵌控制台 保证控制台 handler 在构造期就装配好，
// 并确认既有的凭证校验没有因为新增依赖而被绕过。
func TestWebConsole_应用构造加载内嵌控制台(t *testing.T) {
	application, _ := newHTTPTestApplication(t, false, time.Now)
	if application.webHandler == nil {
		t.Fatal("newApplication 未装配内嵌控制台 handler")
	}
	if _, err := newApplication("", false, rand.Reader, time.Now); err == nil {
		t.Fatal("凭证缺失时仍应构造失败")
	}
}
