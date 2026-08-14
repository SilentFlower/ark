package hub

import (
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func TestHTTP_登录访问退出与安全Cookie(t *testing.T) {
	application, _ := newHTTPTestApplication(t, true, time.Now)
	handler := application.handler()

	unauthenticatedAPI := httptest.NewRecorder()
	handler.ServeHTTP(unauthenticatedAPI, httptest.NewRequest(http.MethodGet, "/api/session", nil))
	if unauthenticatedAPI.Code != http.StatusUnauthorized ||
		!strings.Contains(unauthenticatedAPI.Body.String(), `"authenticated":false`) {
		t.Fatalf("未认证 API status=%d body=%q", unauthenticatedAPI.Code, unauthenticatedAPI.Body.String())
	}

	loginPage := httptest.NewRecorder()
	handler.ServeHTTP(loginPage, httptest.NewRequest(http.MethodGet, "/login", nil))
	if loginPage.Code != http.StatusOK {
		t.Fatalf("登录页 status=%d", loginPage.Code)
	}
	csrfCookie := responseCookie(t, loginPage, loginCSRFCookieName)
	if !csrfCookie.HttpOnly || !csrfCookie.Secure || csrfCookie.SameSite != http.SameSiteStrictMode || csrfCookie.Path != "/" {
		t.Fatalf("登录 CSRF Cookie = %#v", csrfCookie)
	}
	if !strings.Contains(loginPage.Body.String(), csrfCookie.Value) || strings.Contains(loginPage.Body.String(), "totp") {
		t.Fatalf("登录页 CSRF/TOTP 边界错误: %q", loginPage.Body.String())
	}
	for _, header := range []string{
		"Content-Security-Policy", "X-Content-Type-Options", "Referrer-Policy", "X-Frame-Options", "Cache-Control",
	} {
		if loginPage.Header().Get(header) == "" {
			t.Fatalf("登录页缺少安全 header %s", header)
		}
	}

	missingCSRF := postFormRequest("/login", url.Values{
		"username": {"admin"},
		"password": {string(testPassword)},
	})
	missingCSRF.AddCookie(csrfCookie)
	missingCSRFResponse := httptest.NewRecorder()
	handler.ServeHTTP(missingCSRFResponse, missingCSRF)
	if missingCSRFResponse.Code != http.StatusForbidden {
		t.Fatalf("缺失 CSRF status=%d", missingCSRFResponse.Code)
	}

	loginRequest := postFormRequest("/login", url.Values{
		"username":   {"admin"},
		"password":   {string(testPassword)},
		"csrf_token": {csrfCookie.Value},
	})
	loginRequest.RemoteAddr = "192.0.2.10:12345"
	loginRequest.AddCookie(csrfCookie)
	loginResponse := httptest.NewRecorder()
	handler.ServeHTTP(loginResponse, loginRequest)
	if loginResponse.Code != http.StatusSeeOther || loginResponse.Header().Get("Location") != "/" {
		t.Fatalf("登录响应 status=%d location=%q body=%q", loginResponse.Code, loginResponse.Header().Get("Location"), loginResponse.Body.String())
	}
	sessionCookie := responseCookie(t, loginResponse, sessionCookieName)
	if !sessionCookie.HttpOnly || !sessionCookie.Secure || sessionCookie.SameSite != http.SameSiteStrictMode ||
		sessionCookie.MaxAge != int(sessionTTL/time.Second) {
		t.Fatalf("会话 Cookie = %#v", sessionCookie)
	}

	sessionRequest := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	sessionRequest.AddCookie(sessionCookie)
	sessionResponse := httptest.NewRecorder()
	handler.ServeHTTP(sessionResponse, sessionRequest)
	if sessionResponse.Code != http.StatusOK ||
		!strings.Contains(sessionResponse.Body.String(), `"username":"admin"`) {
		t.Fatalf("会话 API status=%d body=%q", sessionResponse.Code, sessionResponse.Body.String())
	}

	shellRequest := httptest.NewRequest(http.MethodGet, "/", nil)
	shellRequest.AddCookie(sessionCookie)
	shellResponse := httptest.NewRecorder()
	handler.ServeHTTP(shellResponse, shellRequest)
	if shellResponse.Code != http.StatusOK || !strings.Contains(shellResponse.Body.String(), "当前管理员：admin") ||
		!strings.Contains(shellResponse.Body.String(), `action="/logout"`) {
		t.Fatalf("受保护壳 status=%d body=%q", shellResponse.Code, shellResponse.Body.String())
	}

	value, ok := application.sessions.get(sessionCookie.Value)
	if !ok {
		t.Fatal("登录后服务端会话不存在")
	}
	logoutWithoutCSRF := postFormRequest("/logout", url.Values{})
	logoutWithoutCSRF.AddCookie(sessionCookie)
	logoutWithoutCSRFResponse := httptest.NewRecorder()
	handler.ServeHTTP(logoutWithoutCSRFResponse, logoutWithoutCSRF)
	if logoutWithoutCSRFResponse.Code != http.StatusForbidden {
		t.Fatalf("退出缺失 CSRF status=%d", logoutWithoutCSRFResponse.Code)
	}

	logoutRequest := postFormRequest("/logout", url.Values{"csrf_token": {value.csrfToken}})
	logoutRequest.AddCookie(sessionCookie)
	logoutResponse := httptest.NewRecorder()
	handler.ServeHTTP(logoutResponse, logoutRequest)
	if logoutResponse.Code != http.StatusSeeOther || logoutResponse.Header().Get("Location") != "/login" {
		t.Fatalf("退出响应 status=%d location=%q", logoutResponse.Code, logoutResponse.Header().Get("Location"))
	}
	if _, ok := application.sessions.get(sessionCookie.Value); ok {
		t.Fatal("退出后服务端会话仍存在")
	}
}

func TestHTTP_健康检查固定且包含安全Header(t *testing.T) {
	application, _ := newHTTPTestApplication(t, false, time.Now)
	response := httptest.NewRecorder()
	application.handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if response.Code != http.StatusOK || response.Body.String() != "ok\n" ||
		response.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("健康检查 status=%d content-type=%q body=%q", response.Code, response.Header().Get("Content-Type"), response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatalf("健康检查安全 header=%v", response.Header())
	}
}

func TestHTTP_错误密码限流且不泄露账号存在性(t *testing.T) {
	application, _ := newHTTPTestApplication(t, false, time.Now)
	handler := application.handler()
	csrfCookie := getLoginCSRFCookie(t, handler)
	for attempt := 0; attempt < loginFailureLimit; attempt++ {
		username := "admin"
		if attempt == 0 {
			username = "missing"
		}
		request := postFormRequest("/login", url.Values{
			"username":   {username},
			"password":   {"incorrect password value"},
			"csrf_token": {csrfCookie.Value},
		})
		request.RemoteAddr = "198.51.100.20:4321"
		request.AddCookie(csrfCookie)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), "用户名或密码错误") {
			t.Fatalf("第 %d 次失败 status=%d body=%q", attempt+1, response.Code, response.Body.String())
		}
		csrfCookie = responseCookie(t, response, loginCSRFCookieName)
	}

	// 限流 key 包含规范化用户名；先前 missing 的失败不应计入 admin，所以补一次 admin 失败。
	request := postFormRequest("/login", url.Values{
		"username":   {"ADMIN"},
		"password":   {"incorrect password value"},
		"csrf_token": {csrfCookie.Value},
	})
	request.RemoteAddr = "198.51.100.20:4321"
	request.AddCookie(csrfCookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("第五次 admin 失败 status=%d", response.Code)
	}
	csrfCookie = responseCookie(t, response, loginCSRFCookieName)

	blocked := postFormRequest("/login", url.Values{
		"username":   {"admin"},
		"password":   {string(testPassword)},
		"csrf_token": {csrfCookie.Value},
	})
	blocked.RemoteAddr = "198.51.100.20:4321"
	blocked.AddCookie(csrfCookie)
	blockedResponse := httptest.NewRecorder()
	handler.ServeHTTP(blockedResponse, blocked)
	if blockedResponse.Code != http.StatusTooManyRequests || blockedResponse.Header().Get("Retry-After") == "" {
		t.Fatalf("限流响应 status=%d retry=%q", blockedResponse.Code, blockedResponse.Header().Get("Retry-After"))
	}
}

func TestHTTP_密码重置过期与伪造会话失效(t *testing.T) {
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	application, authFile := newHTTPTestApplication(t, false, func() time.Time { return now })
	handler := application.handler()
	sessionCookie := loginHTTPTestUser(t, handler)

	forged := &http.Cookie{Name: sessionCookieName, Value: "forged", Path: "/"}
	forgedRequest := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	forgedRequest.AddCookie(forged)
	forgedResponse := httptest.NewRecorder()
	handler.ServeHTTP(forgedResponse, forgedRequest)
	if forgedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("伪造会话 status=%d", forgedResponse.Code)
	}

	if err := resetCredentialPassword(authFile, []byte("new secure password value")); err != nil {
		t.Fatalf("重置密码失败: %v", err)
	}
	resetRequest := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	resetRequest.AddCookie(sessionCookie)
	resetResponse := httptest.NewRecorder()
	handler.ServeHTTP(resetResponse, resetRequest)
	if resetResponse.Code != http.StatusUnauthorized {
		t.Fatalf("密码重置后旧会话 status=%d", resetResponse.Code)
	}

	secondApplication, _ := newHTTPTestApplication(t, false, func() time.Time { return now })
	secondHandler := secondApplication.handler()
	secondCookie := loginHTTPTestUser(t, secondHandler)
	now = now.Add(sessionTTL)
	expiredRequest := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	expiredRequest.AddCookie(secondCookie)
	expiredResponse := httptest.NewRecorder()
	secondHandler.ServeHTTP(expiredResponse, expiredRequest)
	if expiredResponse.Code != http.StatusUnauthorized {
		t.Fatalf("到期会话 status=%d", expiredResponse.Code)
	}
}

func TestHTTP_未知路由默认要求认证(t *testing.T) {
	application, _ := newHTTPTestApplication(t, false, time.Now)
	handler := application.handler()
	pageResponse := httptest.NewRecorder()
	handler.ServeHTTP(pageResponse, httptest.NewRequest(http.MethodGet, "/missing", nil))
	if pageResponse.Code != http.StatusSeeOther || pageResponse.Header().Get("Location") != "/login" {
		t.Fatalf("未认证未知页面 status=%d location=%q", pageResponse.Code, pageResponse.Header().Get("Location"))
	}
	apiResponse := httptest.NewRecorder()
	handler.ServeHTTP(apiResponse, httptest.NewRequest(http.MethodGet, "/api/missing", nil))
	if apiResponse.Code != http.StatusUnauthorized {
		t.Fatalf("未认证未知 API status=%d", apiResponse.Code)
	}
}

func TestHTTP_运行时凭证存储失败返回503(t *testing.T) {
	application, authFile := newHTTPTestApplication(t, false, time.Now)
	handler := application.handler()
	sessionCookie := loginHTTPTestUser(t, handler)
	if err := os.Chmod(authFile, 0o640); err != nil {
		t.Fatalf("修改凭证权限失败: %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	request.AddCookie(sessionCookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Content-Type") != "application/json; charset=utf-8" ||
		response.Body.String() != "{\"error\":\"服务暂不可用\"}\n" {
		t.Fatalf("凭证故障响应 status=%d content-type=%q body=%q", response.Code, response.Header().Get("Content-Type"), response.Body.String())
	}
}

func TestHTTP_登录拒绝错误ContentType和超大Body(t *testing.T) {
	application, _ := newHTTPTestApplication(t, false, time.Now)
	handler := application.handler()

	wrongType := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("username=admin"))
	wrongType.Header.Set("Content-Type", "application/json")
	wrongTypeResponse := httptest.NewRecorder()
	handler.ServeHTTP(wrongTypeResponse, wrongType)
	if wrongTypeResponse.Code != http.StatusBadRequest || !strings.Contains(wrongTypeResponse.Body.String(), "Content-Type") {
		t.Fatalf("错误 Content-Type status=%d body=%q", wrongTypeResponse.Code, wrongTypeResponse.Body.String())
	}

	oversized := httptest.NewRequest(http.MethodPost, "/login", io.LimitReader(strings.NewReader(strings.Repeat("x", maximumFormBytes+2)), maximumFormBytes+2))
	oversized.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	oversizedResponse := httptest.NewRecorder()
	handler.ServeHTTP(oversizedResponse, oversized)
	if oversizedResponse.Code != http.StatusBadRequest || !strings.Contains(oversizedResponse.Body.String(), "解析表单失败") {
		t.Fatalf("超大 body status=%d body=%q", oversizedResponse.Code, oversizedResponse.Body.String())
	}
}

func newHTTPTestApplication(t *testing.T, secureCookie bool, now func() time.Time) (*application, string) {
	t.Helper()
	authFile := testCredentialPath(t)
	if err := initializeCredential(authFile, "admin", append([]byte(nil), testPassword...)); err != nil {
		t.Fatalf("初始化 HTTP 测试凭证失败: %v", err)
	}
	application, err := newApplication(authFile, secureCookie, rand.Reader, now)
	if err != nil {
		t.Fatalf("newApplication 失败: %v", err)
	}
	return application, authFile
}

func getLoginCSRFCookie(t *testing.T, handler http.Handler) *http.Cookie {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/login", nil))
	return responseCookie(t, response, loginCSRFCookieName)
}

func loginHTTPTestUser(t *testing.T, handler http.Handler) *http.Cookie {
	t.Helper()
	csrfCookie := getLoginCSRFCookie(t, handler)
	request := postFormRequest("/login", url.Values{
		"username":   {"admin"},
		"password":   {string(testPassword)},
		"csrf_token": {csrfCookie.Value},
	})
	request.AddCookie(csrfCookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("测试登录 status=%d body=%q", response.Code, response.Body.String())
	}
	return responseCookie(t, response, sessionCookieName)
}

func postFormRequest(path string, values url.Values) *http.Request {
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return request
}

func responseCookie(t *testing.T, response *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == name {
			return cookie
		}
	}
	t.Fatalf("响应缺少 Cookie %q", name)
	return nil
}
