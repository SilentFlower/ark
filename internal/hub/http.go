package hub

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/silentflower/ark/internal/config"
	"github.com/silentflower/ark/internal/hub/webui"
	"github.com/silentflower/ark/internal/schedule"
)

const (
	sessionCookieName   = "ark_hub_session"
	loginCSRFCookieName = "ark_hub_login_csrf"
	maximumFormBytes    = 4 * 1024
	loginCSRFTTL        = 10 * time.Minute
	// loginCSRFTokenLength 是 32 字节随机值经 base64url 无填充编码后的长度。
	loginCSRFTokenLength = 43
)

// loginPageTemplate 是唯一的服务端渲染页面。
//
// 登录仍走表单 + 登录 CSRF Cookie + 限流这条已验收的路径，控制台不接管它：
// 少一个未鉴权可达的 JSON 端点，就少一处需要重新论证的攻击面。
// 样式内联在这里（CSP 的 style-src 允许），配色与控制台保持一致。
const loginPageTemplate = `<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>登录 ark-hub</title>
  <style>body{font-family:system-ui,-apple-system,"Noto Sans SC",sans-serif;max-width:24rem;margin:14vh auto;padding:0 1rem;color:#1e293b;background:#f8fafc}h1{font-size:1.25rem;margin-bottom:.25rem}p.sub{color:#64748b;font-size:.875rem;margin-top:0}form{display:grid;gap:.75rem;background:#fff;border:1px solid #e2e8f0;border-radius:.5rem;padding:1.25rem}label{display:grid;gap:.3rem;font-size:.875rem;color:#475569}input{font:inherit;padding:.55rem;border:1px solid #cbd5e1;border-radius:.375rem}button{font:inherit;padding:.55rem;border:0;border-radius:.375rem;background:#1e293b;color:#fff;cursor:pointer}button:hover{background:#334155}p.error{color:#b91c1c;font-size:.875rem}</style>
</head>
<body>
  <h1>ark-hub</h1>
  <p class="sub">备份与重建控制台</p>
  {{if .Error}}<p class="error">{{.Error}}</p>{{end}}
  <form method="post" action="/login">
    <input type="hidden" name="csrf_token" value="{{.CSRFToken}}">
    <label>用户名<input name="username" autocomplete="username" required></label>
    <label>密码<input type="password" name="password" autocomplete="current-password" required></label>
    <button type="submit">登录</button>
  </form>
</body>
</html>`

type application struct {
	authFile        string
	secureCookie    bool
	sessions        *sessionManager
	limiter         *loginLimiter
	random          io.Reader
	now             func() time.Time
	dummyHash       string
	loginTemplate   *template.Template
	webHandler      http.Handler
	state           apiStore
	configPath      string
	arkBinaryPath   string
	loadConfig      func(string) (*config.Config, error)
	analyzeSchedule func(context.Context, string, time.Time) (schedule.Window, error)
	operations      *operationManager
}

func (application *application) configureRuntime(
	state stateStore,
	configPath string,
	arkBinaryPath string,
	loadConfig func(string) (*config.Config, error),
	operations *operationManager,
) {
	apiState, ok := state.(apiStore)
	if !ok {
		return
	}
	application.state = apiState
	application.configPath = configPath
	application.arkBinaryPath = arkBinaryPath
	application.loadConfig = loadConfig
	application.analyzeSchedule = schedule.Analyze
	application.operations = operations
}

type loginPageData struct {
	CSRFToken string
	Error     string
}

func newApplication(authFile string, secureCookie bool, randomSource io.Reader, now func() time.Time) (*application, error) {
	if randomSource == nil || now == nil {
		return nil, fmt.Errorf("创建 HTTP 应用失败: 内部依赖不完整")
	}
	if _, err := loadCredential(authFile); err != nil {
		return nil, err
	}
	loginTemplate, err := template.New("login").Parse(loginPageTemplate)
	if err != nil {
		return nil, fmt.Errorf("解析登录页模板失败: %w", err)
	}
	// 前端产物在这里就校验：内嵌资源损坏必须在监听前失败，而不是等管理员打开页面。
	webHandler, err := webui.NewHandler()
	if err != nil {
		return nil, fmt.Errorf("加载内嵌控制台失败: %w", err)
	}
	// 未知用户名也执行同成本 Argon2id，避免明显的账号存在性时序差异。
	dummyHash := hashPasswordWithSalt([]byte("ark-hub-dummy-password"), []byte("ark-hub-dummy-v1"))
	return &application{
		authFile:      authFile,
		secureCookie:  secureCookie,
		sessions:      newSessionManager(randomSource, now),
		limiter:       newLoginLimiter(now),
		random:        randomSource,
		now:           now,
		dummyHash:     dummyHash,
		loginTemplate: loginTemplate,
		webHandler:    webHandler,
	}, nil
}

func (application *application) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", application.handleHealth)
	mux.HandleFunc("GET /login", application.handleLoginPage)
	mux.HandleFunc("POST /login", application.handleLogin)
	mux.HandleFunc("GET /{$}", application.handleShell)
	mux.HandleFunc("POST /logout", application.handleLogout)
	mux.HandleFunc("GET /api/session", application.handleSessionAPI)
	mux.HandleFunc("GET /api/hosts", application.handleHostsAPI)
	mux.HandleFunc("GET /api/hosts/{host}", application.handleHostAPI)
	mux.HandleFunc("GET /api/runs", application.handleRunsAPI)
	mux.HandleFunc("GET /api/alerts", application.handleAlertsAPI)
	mux.HandleFunc("GET /api/operations", application.handleOperationsAPI)
	mux.HandleFunc("GET /api/operations/{id}", application.handleOperationAPI)
	mux.HandleFunc("POST /api/hosts/{host}/backup", application.handleBackupAction)
	mux.HandleFunc("POST /api/hosts/{host}/verify", application.handleVerifyAction)
	mux.HandleFunc("POST /api/hosts/{host}/restore", application.handleRestoreAction)
	mux.HandleFunc("/", application.handleProtectedNotFound)
	return securityHeaders(mux)
}

func (application *application) handleHealth(writer http.ResponseWriter, _ *http.Request) {
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	writer.WriteHeader(http.StatusOK)
	writeBody(writer, []byte("ok\n"))
}

func (application *application) handleLoginPage(writer http.ResponseWriter, request *http.Request) {
	_, _, ok, err := application.authenticatedSession(request)
	if err != nil {
		application.serviceUnavailable(writer, request)
		return
	}
	if ok {
		http.Redirect(writer, request, "/", http.StatusSeeOther)
		return
	}
	application.renderLogin(writer, request, "", http.StatusOK)
}

func (application *application) handleLogin(writer http.ResponseWriter, request *http.Request) {
	if err := parseForm(writer, request); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	csrfCookie, err := request.Cookie(loginCSRFCookieName)
	if err != nil || !constantTimeStringEqual(csrfCookie.Value, request.PostForm.Get("csrf_token")) {
		http.Error(writer, "CSRF 校验失败", http.StatusForbidden)
		return
	}

	username := request.PostForm.Get("username")
	password := []byte(request.PostForm.Get("password"))
	defer clearBytes(password)
	limiterKey := loginLimiterKey(request.RemoteAddr, username)
	attempt, retryAfter, allowed := application.limiter.begin(limiterKey)
	if !allowed {
		seconds := int64((retryAfter + time.Second - 1) / time.Second)
		writer.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
		http.Error(writer, "登录尝试过多，请稍后重试", http.StatusTooManyRequests)
		return
	}
	// 任何内部失败都要释放预占名额；failure/success 会幂等完成同一次尝试。
	defer attempt.cancel()

	current, err := loadCredential(application.authFile)
	if err != nil {
		application.serviceUnavailable(writer, request)
		return
	}
	encodedHash := application.dummyHash
	usernameMatches := username == current.Username
	if usernameMatches {
		encodedHash = current.PasswordHash
	}
	passwordValid, err := verifyPassword(password, encodedHash)
	if err != nil {
		application.serviceUnavailable(writer, request)
		return
	}
	if !usernameMatches || !passwordValid {
		attempt.failure()
		application.renderLogin(writer, request, "用户名或密码错误", http.StatusUnauthorized)
		return
	}

	encodedToken, value, err := application.sessions.create(current.Username, current.Revision)
	if err != nil {
		application.serviceUnavailable(writer, request)
		return
	}
	attempt.success()
	application.clearLoginCSRFCookie(writer)
	http.SetCookie(writer, &http.Cookie{
		Name:     sessionCookieName,
		Value:    encodedToken,
		Path:     "/",
		Expires:  value.expiresAt,
		MaxAge:   int(sessionTTL / time.Second),
		HttpOnly: true,
		Secure:   application.secureCookie,
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(writer, request, "/", http.StatusSeeOther)
}

// handleShell 返回内嵌控制台的入口 HTML。
//
// 用户名与 CSRF token 不再由服务端注入模板，前端启动后自己调 GET /api/session 取。
// 这样入口 HTML 是一份与会话无关的静态文件，可以直接由 webui 提供。
func (application *application) handleShell(writer http.ResponseWriter, request *http.Request) {
	if _, _, ok := application.requireSession(writer, request); !ok {
		return
	}
	application.webHandler.ServeHTTP(writer, request)
}

func (application *application) handleLogout(writer http.ResponseWriter, request *http.Request) {
	value, encodedToken, ok := application.requireSession(writer, request)
	if !ok {
		return
	}
	if err := parseForm(writer, request); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	if !constantTimeStringEqual(value.csrfToken, request.PostForm.Get("csrf_token")) {
		http.Error(writer, "CSRF 校验失败", http.StatusForbidden)
		return
	}
	application.sessions.revoke(encodedToken)
	application.clearSessionCookie(writer)
	http.Redirect(writer, request, "/login", http.StatusSeeOther)
}

func (application *application) handleSessionAPI(writer http.ResponseWriter, request *http.Request) {
	value, _, ok := application.requireSession(writer, request)
	if !ok {
		return
	}
	response, err := json.Marshal(struct {
		Authenticated bool   `json:"authenticated"`
		Username      string `json:"username"`
		CSRFToken     string `json:"csrf_token"`
	}{Authenticated: true, Username: value.username, CSRFToken: value.csrfToken})
	if err != nil {
		application.serviceUnavailable(writer, request)
		return
	}
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(http.StatusOK)
	writeBody(writer, append(response, '\n'))
}

// handleProtectedNotFound 处理所有未显式注册的路径。
//
// 鉴权在最前面，因此未登录时的行为与改造前完全一致：页面请求重定向登录，
// /api/ 请求返回 401 JSON。登录之后才按路径分流——API 的未知路径必须保持
// 404 JSON，否则客户端会拿到一份 HTML 并把它当成数据；其余路径交给内嵌控制台，
// 由它返回静态资源或 SPA 入口（history 模式路由的前提）。
func (application *application) handleProtectedNotFound(writer http.ResponseWriter, request *http.Request) {
	if _, _, ok := application.requireSession(writer, request); !ok {
		return
	}
	if strings.HasPrefix(request.URL.Path, "/api/") {
		application.writeAPIError(writer, http.StatusNotFound, "not_found", "接口不存在")
		return
	}
	application.webHandler.ServeHTTP(writer, request)
}

// renderLogin 渲染登录页并确保存在可用的登录 CSRF token。
//
// 两条都必须做到，缺一个真实浏览器就登不进来：
//
//  1. **已有合法 Cookie 时复用它的值，不要轮换。** 浏览器会在登录页之外顺带请求子资源
//     （最典型的是 /favicon.ico），这些请求同样被重定向到登录页并触发一次渲染；
//     每次渲染都换新值的话，用户点提交时带的 Cookie 已被后来那次请求覆盖，
//     而表单里还是旧 token，登录稳定失败在 403。
//  2. **每次渲染都重新下发 Cookie 以续期。** 只复用值却不刷新过期时间，会把一个
//     即将到期的 Cookie 连同 token 一起交给用户——页面停留几十秒再提交，
//     Cookie 已被浏览器丢弃，请求根本不带 Cookie，同样是 403。
//
// 复用值不降低防护强度：CSRF 依赖的是攻击者读不到 HttpOnly Cookie，而不是频繁轮换。
func (application *application) renderLogin(
	writer http.ResponseWriter,
	request *http.Request,
	errorMessage string,
	status int,
) {
	csrfToken := ""
	if cookie, err := request.Cookie(loginCSRFCookieName); err == nil && validLoginCSRFToken(cookie.Value) {
		csrfToken = cookie.Value
	}
	if csrfToken == "" {
		generated, err := randomToken(application.random)
		if err != nil {
			http.Error(writer, "服务暂不可用", http.StatusServiceUnavailable)
			return
		}
		csrfToken = generated
	}
	now := application.now().UTC()
	http.SetCookie(writer, &http.Cookie{
		Name:     loginCSRFCookieName,
		Value:    csrfToken,
		Path:     "/",
		Expires:  now.Add(loginCSRFTTL),
		MaxAge:   int(loginCSRFTTL / time.Second),
		HttpOnly: true,
		Secure:   application.secureCookie,
		SameSite: http.SameSiteStrictMode,
	})
	body, err := executeTemplate(application.loginTemplate, loginPageData{
		CSRFToken: csrfToken,
		Error:     errorMessage,
	})
	if err != nil {
		http.Error(writer, "服务暂不可用", http.StatusServiceUnavailable)
		return
	}
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.WriteHeader(status)
	writeBody(writer, body)
}

// validLoginCSRFToken 判断 Cookie 里的值是否是本服务签发的 token 形态。
// 只做长度与字符集校验：真正的比对发生在提交时的 constant-time 比较。
func validLoginCSRFToken(value string) bool {
	if len(value) != loginCSRFTokenLength {
		return false
	}
	for _, character := range value {
		switch {
		case character >= 'A' && character <= 'Z',
			character >= 'a' && character <= 'z',
			character >= '0' && character <= '9',
			character == '-', character == '_':
		default:
			return false
		}
	}
	return true
}

func (application *application) requireSession(writer http.ResponseWriter, request *http.Request) (session, string, bool) {
	value, encodedToken, ok, err := application.authenticatedSession(request)
	if err != nil {
		application.serviceUnavailable(writer, request)
		return session{}, "", false
	}
	if ok {
		return value, encodedToken, true
	}
	application.clearSessionCookie(writer)
	if strings.HasPrefix(request.URL.Path, "/api/") {
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		writer.WriteHeader(http.StatusUnauthorized)
		writeBody(writer, []byte("{\"authenticated\":false}\n"))
		return session{}, "", false
	}
	http.Redirect(writer, request, "/login", http.StatusSeeOther)
	return session{}, "", false
}

func (application *application) authenticatedSession(request *http.Request) (session, string, bool, error) {
	cookie, err := request.Cookie(sessionCookieName)
	if err != nil {
		return session{}, "", false, nil
	}
	value, ok := application.sessions.get(cookie.Value)
	if !ok {
		return session{}, cookie.Value, false, nil
	}
	current, err := loadCredential(application.authFile)
	if err != nil {
		return session{}, cookie.Value, false, err
	}
	if current.Username != value.username || current.Revision != value.revision {
		application.sessions.revoke(cookie.Value)
		return session{}, cookie.Value, false, nil
	}
	return value, cookie.Value, true, nil
}

func (application *application) serviceUnavailable(writer http.ResponseWriter, request *http.Request) {
	if strings.HasPrefix(request.URL.Path, "/api/") {
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		writer.WriteHeader(http.StatusServiceUnavailable)
		writeBody(writer, []byte("{\"error\":\"服务暂不可用\"}\n"))
		return
	}
	http.Error(writer, "服务暂不可用", http.StatusServiceUnavailable)
}

func (application *application) clearLoginCSRFCookie(writer http.ResponseWriter) {
	http.SetCookie(writer, expiredCookie(loginCSRFCookieName, application.secureCookie))
}

func (application *application) clearSessionCookie(writer http.ResponseWriter) {
	http.SetCookie(writer, expiredCookie(sessionCookieName, application.secureCookie))
}

func expiredCookie(name string, secure bool) *http.Cookie {
	return &http.Cookie{
		Name:     name,
		Path:     "/",
		Expires:  time.Unix(1, 0).UTC(),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
	}
}

func parseForm(writer http.ResponseWriter, request *http.Request) error {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/x-www-form-urlencoded" {
		return fmt.Errorf("Content-Type 必须是 application/x-www-form-urlencoded")
	}
	request.Body = http.MaxBytesReader(writer, request.Body, maximumFormBytes)
	if err := request.ParseForm(); err != nil {
		return fmt.Errorf("解析表单失败: %w", err)
	}
	return nil
}

func loginLimiterKey(remoteAddress, username string) string {
	host, _, err := net.SplitHostPort(remoteAddress)
	if err != nil {
		host = remoteAddress
	}
	return host + "\x00" + strings.ToLower(username)
}

func constantTimeStringEqual(left, right string) bool {
	return len(left) == len(right) && subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func executeTemplate(value *template.Template, data any) ([]byte, error) {
	var buffer bytes.Buffer
	if err := value.Execute(&buffer, data); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// contentSecurityPolicy 是控制台接入后的策略。
//
// P4-1/P4-2 阶段这里是 `default-src 'none'`，完全禁止脚本；内嵌 Vue 控制台之后
// 必须放开同源脚本。放开的只有同源，没有 CDN、没有 'unsafe-eval'、没有 data: 脚本。
//
// style-src 保留 'unsafe-inline' 是一个明确的取舍，不是疏漏：Vue 的 :style 绑定和
// 过渡动画会写 style 属性，而入口 HTML 是静态嵌入文件，无法为每次请求注入 nonce。
// 在 script-src 'self' + default-src 'none' 已经堵死脚本注入的前提下，
// 内联 style 的剩余风险对一个单管理员、默认只监听 127.0.0.1 的运维台可以接受。
// 修改本策略前先想清楚会不会让脚本来源变宽——这是控制台唯一实质扩大的攻击面。
const contentSecurityPolicy = "default-src 'none'; script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; " +
	"connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Security-Policy", contentSecurityPolicy)
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("Referrer-Policy", "no-referrer")
		writer.Header().Set("X-Frame-Options", "DENY")
		// 默认禁止缓存；带内容 hash 的静态资源由 webui handler 显式覆盖为长期缓存。
		writer.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(writer, request)
	})
}

func writeBody(writer http.ResponseWriter, body []byte) {
	// 客户端断开后的写错误已无法改变响应；本任务也不引入日志框架，因此显式忽略。
	_, _ = writer.Write(body)
}
