package dnsmgr

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(request *http.Request) (*http.Response, error) {
	return f(request)
}

func apiHTTPResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}
}

func writeCredentials(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dnsmgr.env")
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("写入 dnsmgr 凭证失败: %v", err)
	}
	return path
}

func TestAuthSign_固定向量(t *testing.T) {
	if got := authSign("12", "1700000000", "secret"); got != "58318767c05048ea6877618b09755c54" {
		t.Fatalf("签名 = %q", got)
	}
}

func TestLoadCredentials_校验键与权限(t *testing.T) {
	valid := writeCredentials(t, "ARK_DNSMGR_UID=12\nARK_DNSMGR_API_KEY=secret\n", 0o600)
	loaded, err := loadCredentials(valid)
	if err != nil || loaded.uid != "12" || loaded.apiKey != "secret" {
		t.Fatalf("凭证 = %#v, err=%v", loaded, err)
	}

	tests := []struct {
		name    string
		content string
		mode    os.FileMode
		wantSub string
	}{
		{name: "未知键", content: "ARK_DNSMGR_UID=12\nARK_DNSMGR_API_KEY=secret\nEXTRA=x\n", mode: 0o600, wantSub: "不支持的键"},
		{name: "UID 非法", content: "ARK_DNSMGR_UID=abc\nARK_DNSMGR_API_KEY=secret\n", mode: 0o600, wantSub: "正整数"},
		{name: "API key 缺失", content: "ARK_DNSMGR_UID=12\n", mode: 0o600, wantSub: apiKeyKey},
		{name: "权限过宽", content: "ARK_DNSMGR_UID=12\nARK_DNSMGR_API_KEY=secret\n", mode: 0o640, wantSub: "0600"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadCredentials(writeCredentials(t, tc.content, tc.mode))
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("错误 = %v，期望包含 %q", err, tc.wantSub)
			}
		})
	}
}

func TestClientCheckAuth_发送签名表单(t *testing.T) {
	baseURL, _ := url.Parse("https://dns.example.com/root")
	client := newClient(baseURL, credentials{uid: "12", apiKey: "secret"}, doerFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://dns.example.com/root/api/auth/check" {
			t.Fatalf("请求 URL = %s", request.URL.String())
		}
		if err := request.ParseForm(); err != nil {
			t.Fatalf("解析表单失败: %v", err)
		}
		if request.Form.Get("uid") != "12" || request.Form.Get("timestamp") != "1700000000" ||
			request.Form.Get("sign") != authSign("12", "1700000000", "secret") {
			t.Fatalf("认证表单 = %#v", request.Form)
		}
		return apiHTTPResponse(http.StatusOK, `{"code":0,"msg":"认证成功"}`), nil
	}), func() time.Time { return time.Unix(1700000000, 0) })

	if err := client.CheckAuth(context.Background()); err != nil {
		t.Fatalf("认证检查失败: %v", err)
	}
}

func TestClientSetRecordValue_校验返回契约(t *testing.T) {
	baseURL, _ := url.Parse("https://dns.example.com")
	client := newClient(baseURL, credentials{uid: "12", apiKey: "secret"}, doerFunc(func(request *http.Request) (*http.Response, error) {
		if err := request.ParseForm(); err != nil {
			t.Fatalf("解析表单失败: %v", err)
		}
		if request.URL.Path != "/api/record/value/7" || request.Form.Get("recordid") != "record-1" ||
			request.Form.Get("value") != "203.0.113.10" || request.Form.Get("expected_value") != "198.51.100.20" {
			t.Fatalf("Value-only 请求 = %s %#v", request.URL.Path, request.Form)
		}
		return apiHTTPResponse(http.StatusOK, `{"code":0,"data":{"recordid":"record-1","previous_value":"198.51.100.20","value":"203.0.113.10","changed":true}}`), nil
	}), time.Now)
	expected := "198.51.100.20"
	result, err := client.SetRecordValue(context.Background(), 7, "record-1", "203.0.113.10", &expected)
	if err != nil || !result.Changed || result.PreviousValue != expected {
		t.Fatalf("结果 = %#v, err=%v", result, err)
	}
}

func TestClientSetRecordValue_拒绝跨地址族参数和响应(t *testing.T) {
	baseURL, _ := url.Parse("https://dns.example.com")
	called := false
	client := newClient(baseURL, credentials{uid: "12", apiKey: "secret"}, doerFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return apiHTTPResponse(http.StatusOK, `{"code":0,"data":{"recordid":"record-1","previous_value":"2001:db8::1","value":"203.0.113.10","changed":true}}`), nil
	}), time.Now)
	expected := "2001:db8::2"
	if _, err := client.SetRecordValue(context.Background(), 7, "record-1", "203.0.113.10", &expected); err == nil ||
		!strings.Contains(err.Error(), "地址族") || called {
		t.Fatalf("跨地址族 expected 错误 = %v, called=%v", err, called)
	}

	if _, err := client.SetRecordValue(context.Background(), 7, "record-1", "203.0.113.10", nil); err == nil ||
		!strings.Contains(err.Error(), "不符合契约") || !called {
		t.Fatalf("跨地址族响应错误 = %v, called=%v", err, called)
	}
}

func TestClient_拒绝不可信响应且不泄漏秘密(t *testing.T) {
	baseURL, _ := url.Parse("https://dns.example.com/token-should-not-leak")
	tests := []struct {
		name    string
		doer    httpDoer
		wantSub string
	}{
		{name: "网络错误", doer: doerFunc(func(request *http.Request) (*http.Response, error) {
			return nil, errors.New(request.URL.String() + " api-key-should-not-leak")
		}), wantSub: "HTTP 请求未完成"},
		{name: "HTTP 403", doer: doerFunc(func(*http.Request) (*http.Response, error) {
			return apiHTTPResponse(http.StatusForbidden, `{"code":-1,"msg":"api-key-should-not-leak"}`), nil
		}), wantSub: "HTTP 状态 403"},
		{name: "非法 JSON", doer: doerFunc(func(*http.Request) (*http.Response, error) {
			return apiHTTPResponse(http.StatusOK, "not-json"), nil
		}), wantSub: "无效 JSON"},
		{name: "业务错误", doer: doerFunc(func(*http.Request) (*http.Response, error) {
			return apiHTTPResponse(http.StatusOK, `{"code":-1,"msg":"api-key-should-not-leak"}`), nil
		}), wantSub: "业务错误码 -1"},
		{name: "超大响应", doer: doerFunc(func(*http.Request) (*http.Response, error) {
			return apiHTTPResponse(http.StatusOK, strings.Repeat("x", maximumResponseBody+1)), nil
		}), wantSub: "超过"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := newClient(baseURL, credentials{uid: "12", apiKey: "api-key-should-not-leak"}, tc.doer, time.Now)
			err := client.CheckAuth(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("错误 = %v，期望包含 %q", err, tc.wantSub)
			}
			for _, secret := range []string{"api-key-should-not-leak", "token-should-not-leak"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("错误泄漏秘密 %q: %v", secret, err)
				}
			}
		})
	}
}

func TestNew_禁止跟随重定向(t *testing.T) {
	var reached atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached.Store(true)
	}))
	defer target.Close()
	initial := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL, http.StatusTemporaryRedirect)
	}))
	defer initial.Close()
	credentialsPath := writeCredentials(t, "ARK_DNSMGR_UID=12\nARK_DNSMGR_API_KEY=secret\n", 0o600)
	client, err := New(initial.URL, credentialsPath)
	if err != nil {
		t.Fatalf("创建 client 失败: %v", err)
	}
	if err := client.CheckAuth(context.Background()); err == nil || !strings.Contains(err.Error(), "HTTP 状态 307") {
		t.Fatalf("重定向错误 = %v", err)
	}
	if reached.Load() {
		t.Fatal("认证表单不应到达重定向目标")
	}
	httpClient, ok := client.httpClient.(*http.Client)
	if !ok || httpClient.Timeout != defaultHTTPTimeout {
		t.Fatalf("HTTP client = %#v", client.httpClient)
	}
}

func TestClient_实际超时且错误脱敏(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	defer func() {
		close(release)
		server.Close()
	}()
	baseURL, err := url.Parse(server.URL + "/token-should-not-leak")
	if err != nil {
		t.Fatalf("解析测试 URL 失败: %v", err)
	}
	client := newClient(
		baseURL,
		credentials{uid: "12", apiKey: "api-key-should-not-leak"},
		&http.Client{Timeout: 20 * time.Millisecond},
		time.Now,
	)

	started := time.Now()
	err = client.CheckAuth(context.Background())
	if err == nil || !strings.Contains(err.Error(), "HTTP 请求未完成") {
		t.Fatalf("超时错误 = %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatalf("HTTP client 未在超时边界内返回: %s", time.Since(started))
	}
	for _, secret := range []string{"api-key-should-not-leak", "token-should-not-leak"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("超时错误泄漏秘密 %q: %v", secret, err)
		}
	}
}
