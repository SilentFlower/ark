package monitoring

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

func writeSettingsFile(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "monitoring.env")
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("写入监控凭证文件失败: %v", err)
	}
	return path
}

func response(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func testClient(doer httpDoer, now time.Time) deliveryClient {
	return deliveryClient{
		httpClient: doer,
		now:        func() time.Time { return now },
		wait:       func(context.Context, time.Duration) error { return nil },
		attempts:   3,
	}
}

func TestLoad_解析完整配置(t *testing.T) {
	path := writeSettingsFile(t, strings.Join([]string{
		"ARK_DINGTALK_WEBHOOK_URL=https://oapi.dingtalk.com/robot/send?access_token=token",
		"ARK_DINGTALK_SECRET=secret",
		"ARK_HEARTBEAT_SUCCESS_URL=https://health.example/ping/token",
		"ARK_HEARTBEAT_FAILURE_URL=https://health.example/ping/token/fail",
		"",
	}, "\n"), 0o600)

	settings, err := Load(path)
	if err != nil {
		t.Fatalf("Load 返回错误: %v", err)
	}
	if settings.DingTalk == nil || settings.Heartbeat == nil {
		t.Fatalf("监控配置不完整: %#v", settings)
	}
}

func TestLoad_拒绝未知键与非法组合(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantSub string
	}{
		{name: "未知键", content: "ARK_DINGTALK_WEBHOOK_UR=https://example.com\n", wantSub: "不支持的键"},
		{name: "签名密钥缺少 webhook", content: "ARK_DINGTALK_SECRET=secret\n", wantSub: "同时配置"},
		{name: "心跳缺少失败端点", content: "ARK_HEARTBEAT_SUCCESS_URL=https://example.com/ok\n", wantSub: "同时配置"},
		{name: "非 loopback HTTP", content: "ARK_DINGTALK_WEBHOOK_URL=http://example.com/hook\n", wantSub: "只允许 HTTPS"},
		{name: "URL 含 userinfo", content: "ARK_DINGTALK_WEBHOOK_URL=https://user@example.com/hook\n", wantSub: "userinfo"},
		{name: "URL 含 fragment", content: "ARK_DINGTALK_WEBHOOK_URL=https://example.com/hook#secret\n", wantSub: "fragment"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeSettingsFile(t, tc.content, 0o600))
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("Load 错误 = %v，期望包含 %q", err, tc.wantSub)
			}
		})
	}
}

func TestLoad_拒绝过宽权限与符号链接(t *testing.T) {
	wide := writeSettingsFile(t, "", 0o640)
	if _, err := Load(wide); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("过宽权限错误 = %v", err)
	}

	target := writeSettingsFile(t, "", 0o600)
	link := filepath.Join(t.TempDir(), "monitoring.env")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("创建符号链接失败: %v", err)
	}
	if _, err := Load(link); err == nil {
		t.Fatal("符号链接应被拒绝")
	}
}

func TestLoad_允许LoopbackHTTP(t *testing.T) {
	path := writeSettingsFile(t, strings.Join([]string{
		"ARK_HEARTBEAT_SUCCESS_URL=http://127.0.0.1:8080/ok",
		"ARK_HEARTBEAT_FAILURE_URL=http://localhost:8080/fail",
		"",
	}, "\n"), 0o600)
	if _, err := Load(path); err != nil {
		t.Fatalf("loopback HTTP 应被允许: %v", err)
	}
}

func TestSignedDingTalkURL_固定向量(t *testing.T) {
	endpoint, err := url.Parse("https://oapi.dingtalk.com/robot/send?access_token=token")
	if err != nil {
		t.Fatalf("解析测试 URL 失败: %v", err)
	}
	got := signedDingTalkURL(endpoint, "SEC123", time.UnixMilli(1700000000123))
	if got.Query().Get("timestamp") != "1700000000123" {
		t.Fatalf("timestamp = %q", got.Query().Get("timestamp"))
	}
	if got.Query().Get("sign") != "FVgVLxEM+JweBRCq6YFMJ4KCCc7PFGREuLoDL/V5GSc=" {
		t.Fatalf("sign = %q", got.Query().Get("sign"))
	}
	if endpoint.Query().Get("sign") != "" {
		t.Fatal("签名过程不应修改原始 URL")
	}
}

func TestSendDingTalk_发送Markdown并校验业务响应(t *testing.T) {
	endpoint, _ := url.Parse("https://oapi.dingtalk.com/robot/send?access_token=token")
	var captured string
	client := testClient(doerFunc(func(request *http.Request) (*http.Response, error) {
		payload, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatalf("读取请求体失败: %v", err)
		}
		captured = string(payload)
		return response(http.StatusOK, `{"errcode":0,"errmsg":"ok"}`), nil
	}), time.UnixMilli(1700000000123))

	err := client.sendDingTalk(context.Background(), DingTalkSettings{
		webhookURL: endpoint,
		secret:     "SEC123",
	}, MarkdownMessage{Title: "Ark 告警", Text: "### host: web-01"})
	if err != nil {
		t.Fatalf("sendDingTalk 返回错误: %v", err)
	}
	for _, part := range []string{`"msgtype":"markdown"`, `"title":"Ark 告警"`, `"text":"### host: web-01"`} {
		if !strings.Contains(captured, part) {
			t.Fatalf("请求体 %s 缺少 %s", captured, part)
		}
	}
}

func TestSendDingTalk_重试且不泄漏秘密(t *testing.T) {
	endpoint, _ := url.Parse("https://oapi.dingtalk.com/robot/send?access_token=TOKEN_SHOULD_NOT_LEAK")
	attempts := 0
	client := testClient(doerFunc(func(request *http.Request) (*http.Response, error) {
		attempts++
		if attempts < 3 {
			return nil, errors.New(request.URL.String())
		}
		return response(http.StatusOK, `{"errcode":0}`), nil
	}), time.UnixMilli(1700000000123))

	err := client.sendDingTalk(context.Background(), DingTalkSettings{
		webhookURL: endpoint,
		secret:     "SECRET_SHOULD_NOT_LEAK",
	}, MarkdownMessage{Title: "告警", Text: "正文"})
	if err != nil {
		t.Fatalf("第三次尝试应成功: %v", err)
	}
	if attempts != 3 {
		t.Fatalf("尝试次数 = %d，期望 3", attempts)
	}

	failedClient := testClient(doerFunc(func(request *http.Request) (*http.Response, error) {
		return nil, errors.New(request.URL.String())
	}), time.UnixMilli(1700000000123))
	err = failedClient.sendDingTalk(context.Background(), DingTalkSettings{
		webhookURL: endpoint,
		secret:     "SECRET_SHOULD_NOT_LEAK",
	}, MarkdownMessage{Title: "告警", Text: "正文"})
	for _, secret := range []string{"TOKEN_SHOULD_NOT_LEAK", "SECRET_SHOULD_NOT_LEAK"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("错误信息泄漏秘密 %q: %v", secret, err)
		}
	}
}

func TestSendDingTalk_拒绝业务错误与超大响应(t *testing.T) {
	endpoint, _ := url.Parse("https://oapi.dingtalk.com/robot/send?access_token=token")
	tests := []struct {
		name    string
		body    string
		wantSub string
	}{
		{name: "业务错误", body: `{"errcode":310000,"errmsg":"keywords not in content"}`, wantSub: "业务错误码 310000"},
		{name: "非法 JSON", body: `not-json`, wantSub: "业务 JSON"},
		{name: "超大响应", body: strings.Repeat("x", maximumResponseBody+1), wantSub: "超过"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := testClient(doerFunc(func(*http.Request) (*http.Response, error) {
				return response(http.StatusOK, tc.body), nil
			}), time.Now())
			err := client.sendDingTalk(context.Background(), DingTalkSettings{webhookURL: endpoint}, MarkdownMessage{Title: "告警", Text: "正文"})
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("sendDingTalk 错误 = %v，期望包含 %q", err, tc.wantSub)
			}
		})
	}
}

func TestNewDeliveryClient_配置固定超时(t *testing.T) {
	client, ok := newDeliveryClient().httpClient.(*http.Client)
	if !ok {
		t.Fatalf("HTTP 客户端类型 = %T，期望 *http.Client", newDeliveryClient().httpClient)
	}
	if client.Timeout != httpTimeout {
		t.Fatalf("HTTP 超时 = %s，期望 %s", client.Timeout, httpTimeout)
	}
}

func TestSendDingTalk_HTTP状态重试策略(t *testing.T) {
	endpoint, _ := url.Parse("https://oapi.dingtalk.com/robot/send?access_token=token")
	tests := []struct {
		name         string
		statuses     []int
		wantAttempts int
		wantSub      string
	}{
		{name: "400 不重试", statuses: []int{http.StatusBadRequest}, wantAttempts: 1, wantSub: "HTTP 状态 400"},
		{name: "429 重试后成功", statuses: []int{http.StatusTooManyRequests, http.StatusNoContent}, wantAttempts: 2},
		{name: "5xx 重试到上限", statuses: []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable}, wantAttempts: 3, wantSub: "已完成 3 次尝试"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			attempts := 0
			client := testClient(doerFunc(func(*http.Request) (*http.Response, error) {
				status := tc.statuses[attempts]
				attempts++
				return response(status, `{"errcode":0}`), nil
			}), time.Now())
			err := client.sendDingTalk(context.Background(), DingTalkSettings{webhookURL: endpoint}, MarkdownMessage{Title: "告警", Text: "正文"})
			if tc.wantSub == "" {
				if err != nil {
					t.Fatalf("sendDingTalk 返回错误: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("sendDingTalk 错误 = %v，期望包含 %q", err, tc.wantSub)
			}
			if attempts != tc.wantAttempts {
				t.Fatalf("尝试次数 = %d，期望 %d", attempts, tc.wantAttempts)
			}
		})
	}
}

func TestSendDingTalk_HTTP客户端超时且不泄漏秘密(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		select {
		case <-request.Context().Done():
		case <-time.After(time.Second):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"errcode":0}`))
		}
	}))
	defer server.Close()

	const secret = "TIMEOUT_SHOULD_NOT_LEAK"
	endpoint, _ := url.Parse(server.URL + "/ping?token=" + secret)
	client := testClient(&http.Client{Timeout: 20 * time.Millisecond}, time.Now())
	client.attempts = 1
	startedAt := time.Now()
	err := client.sendDingTalk(context.Background(), DingTalkSettings{webhookURL: endpoint}, MarkdownMessage{Title: "告警", Text: "正文"})
	server.CloseClientConnections()
	if err == nil {
		t.Fatal("钉钉 HTTP 超时应返回错误")
	}
	if elapsed := time.Since(startedAt); elapsed >= 500*time.Millisecond {
		t.Fatalf("钉钉 HTTP 超时耗时 = %s，期望小于 500ms", elapsed)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("超时错误泄漏秘密: %v", err)
	}
}

func TestSendDingTalk_拒绝重定向绕过安全端点(t *testing.T) {
	var redirectedRequests atomic.Int32
	redirected := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirectedRequests.Add(1)
	}))
	defer redirected.Close()

	const secret = "REDIRECT_SHOULD_NOT_LEAK"
	initial := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		http.Redirect(w, request, redirected.URL+"/receive?token="+secret, http.StatusTemporaryRedirect)
	}))
	defer initial.Close()

	endpoint, _ := url.Parse(initial.URL + "/hook")
	err := newDeliveryClient().sendDingTalk(context.Background(), DingTalkSettings{webhookURL: endpoint}, MarkdownMessage{Title: "告警", Text: "正文"})
	if err == nil || !strings.Contains(err.Error(), "HTTP 状态 307") {
		t.Fatalf("sendDingTalk 错误 = %v，期望拒绝 307 重定向", err)
	}
	if got := redirectedRequests.Load(); got != 0 {
		t.Fatalf("重定向目标收到 %d 次请求，期望 0", got)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("重定向错误泄漏秘密: %v", err)
	}
}

func TestSendHeartbeat_按终态选择端点(t *testing.T) {
	success, _ := url.Parse("https://health.example/success")
	failure, _ := url.Parse("https://health.example/failure")
	var paths []string
	client := testClient(doerFunc(func(request *http.Request) (*http.Response, error) {
		paths = append(paths, request.URL.Path)
		return response(http.StatusNoContent, ""), nil
	}), time.Now())
	settings := HeartbeatSettings{successURL: success, failureURL: failure}
	if err := client.sendHeartbeat(context.Background(), settings, false); err != nil {
		t.Fatalf("发送成功心跳失败: %v", err)
	}
	if err := client.sendHeartbeat(context.Background(), settings, true); err != nil {
		t.Fatalf("发送失败心跳失败: %v", err)
	}
	if strings.Join(paths, ",") != "/success,/failure" {
		t.Fatalf("请求路径 = %v", paths)
	}
}
