package hub

import (
	"bytes"
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
)

const (
	sessionCookieName   = "ark_hub_session"
	loginCSRFCookieName = "ark_hub_login_csrf"
	maximumFormBytes    = 4 * 1024
	loginCSRFTTL        = 10 * time.Minute
)

const loginPageTemplate = `<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>登录 ark-hub</title>
  <style>body{font-family:system-ui,sans-serif;max-width:28rem;margin:12vh auto;padding:0 1rem;color:#17202a}form{display:grid;gap:.75rem}label{display:grid;gap:.3rem}input,button{font:inherit;padding:.65rem}p{color:#a93226}</style>
</head>
<body>
  <h1>ark-hub</h1>
  {{if .Error}}<p>{{.Error}}</p>{{end}}
  <form method="post" action="/login">
    <input type="hidden" name="csrf_token" value="{{.CSRFToken}}">
    <label>用户名<input name="username" autocomplete="username" required></label>
    <label>密码<input type="password" name="password" autocomplete="current-password" required></label>
    <button type="submit">登录</button>
  </form>
</body>
</html>`

const shellPageTemplate = `<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>ark-hub</title>
  <style>body{font-family:system-ui,sans-serif;max-width:48rem;margin:10vh auto;padding:0 1rem;color:#17202a}form{margin-top:2rem}button{font:inherit;padding:.65rem 1rem}</style>
</head>
<body>
  <h1>ark-hub</h1>
  <p>当前管理员：{{.Username}}</p>
  <p>控制台功能将在后续 P4 任务中接入。</p>
  <form method="post" action="/logout">
    <input type="hidden" name="csrf_token" value="{{.CSRFToken}}">
    <button type="submit">退出登录</button>
  </form>
</body>
</html>`

type application struct {
	authFile      string
	secureCookie  bool
	sessions      *sessionManager
	limiter       *loginLimiter
	random        io.Reader
	now           func() time.Time
	dummyHash     string
	loginTemplate *template.Template
	shellTemplate *template.Template
}

type loginPageData struct {
	CSRFToken string
	Error     string
}

type shellPageData struct {
	Username  string
	CSRFToken string
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
	shellTemplate, err := template.New("shell").Parse(shellPageTemplate)
	if err != nil {
		return nil, fmt.Errorf("解析受保护页面模板失败: %w", err)
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
		shellTemplate: shellTemplate,
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
	application.renderLogin(writer, "", http.StatusOK)
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
		application.renderLogin(writer, "用户名或密码错误", http.StatusUnauthorized)
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

func (application *application) handleShell(writer http.ResponseWriter, request *http.Request) {
	value, _, ok := application.requireSession(writer, request)
	if !ok {
		return
	}
	body, err := executeTemplate(application.shellTemplate, shellPageData{
		Username:  value.username,
		CSRFToken: value.csrfToken,
	})
	if err != nil {
		application.serviceUnavailable(writer, request)
		return
	}
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.WriteHeader(http.StatusOK)
	writeBody(writer, body)
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
	}{Authenticated: true, Username: value.username})
	if err != nil {
		application.serviceUnavailable(writer, request)
		return
	}
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(http.StatusOK)
	writeBody(writer, append(response, '\n'))
}

func (application *application) handleProtectedNotFound(writer http.ResponseWriter, request *http.Request) {
	if _, _, ok := application.requireSession(writer, request); !ok {
		return
	}
	http.NotFound(writer, request)
}

func (application *application) renderLogin(writer http.ResponseWriter, errorMessage string, status int) {
	csrfToken, err := randomToken(application.random)
	if err != nil {
		http.Error(writer, "服务暂不可用", http.StatusServiceUnavailable)
		return
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

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("Referrer-Policy", "no-referrer")
		writer.Header().Set("X-Frame-Options", "DENY")
		writer.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(writer, request)
	})
}

func writeBody(writer http.ResponseWriter, body []byte) {
	// 客户端断开后的写错误已无法改变响应；本任务也不引入日志框架，因此显式忽略。
	_, _ = writer.Write(body)
}
