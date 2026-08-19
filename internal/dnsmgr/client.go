// Package dnsmgr 提供 dnsmgr AuthApi 客户端、dmonitor 维护窗口与恢复后 DNS 切换编排。
package dnsmgr

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/silentflower/ark/internal/endpoint"
	"github.com/silentflower/ark/internal/envfile"
	"github.com/silentflower/ark/internal/secretfile"
)

const (
	uidKey              = "ARK_DNSMGR_UID"
	apiKeyKey           = "ARK_DNSMGR_API_KEY"
	defaultHTTPTimeout  = 10 * time.Second
	maximumResponseBody = 64 * 1024
)

var allowedCredentialKeys = map[string]struct{}{
	uidKey:    {},
	apiKeyKey: {},
}

type credentials struct {
	uid    string
	apiKey string
}

type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// Client 是已校验配置与凭证的 dnsmgr AuthApi 客户端。
type Client struct {
	baseURL     *url.URL
	credentials credentials
	httpClient  httpDoer
	now         func() time.Time
}

// ValueResult 是 Value-only API 对一条记录的成功响应。
type ValueResult struct {
	// RecordID 是 provider 的稳定记录 ID。
	RecordID string `json:"record_id"`
	// PreviousValue 是调用前的记录 IP。
	PreviousValue string `json:"previous_value"`
	// Value 是调用后的记录 IP。
	Value string `json:"value"`
	// Changed 表示本次是否实际调用 provider 更新。
	Changed bool `json:"changed"`
}

type apiResponse struct {
	Code *int `json:"code"`
	Data struct {
		RecordID      string `json:"recordid"`
		PreviousValue string `json:"previous_value"`
		Value         string `json:"value"`
		Changed       bool   `json:"changed"`
	} `json:"data"`
}

// New 安全加载凭证并创建禁止重定向的 dnsmgr client。
// @param baseURL dnsmgr 服务根地址，可包含反向代理路径前缀。
// @param envFile 只包含 AuthApi UID 与 API key 的受限凭证文件。
// @return *Client 已完成 URL、文件权限和凭证键校验的客户端。
// @return error URL、文件安全、语法或凭证内容无效时的错误。
func New(baseURL string, envFile string) (*Client, error) {
	parsed, err := endpoint.ParseBaseURL("dnsmgr.base_url", baseURL)
	if err != nil {
		return nil, err
	}
	loaded, err := loadCredentials(envFile)
	if err != nil {
		return nil, err
	}
	return newClient(parsed, loaded, &http.Client{
		Timeout: defaultHTTPTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, time.Now), nil
}

func newClient(baseURL *url.URL, loaded credentials, doer httpDoer, now func() time.Time) *Client {
	return &Client{baseURL: baseURL, credentials: loaded, httpClient: doer, now: now}
}

func loadCredentials(path string) (credentials, error) {
	file, err := secretfile.Open(path, "dnsmgr.env_file")
	if err != nil {
		return credentials{}, err
	}
	defer func() { _ = file.Close() }()

	values, err := envfile.ParseReader(file, path)
	if err != nil {
		return credentials{}, err
	}
	var unknown []string
	for key := range values {
		if _, ok := allowedCredentialKeys[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return credentials{}, fmt.Errorf("dnsmgr 凭证文件 %s 包含不支持的键 %s", path, strings.Join(unknown, ", "))
	}

	uidValue := strings.TrimSpace(values[uidKey])
	uid, err := strconv.ParseInt(uidValue, 10, 64)
	if err != nil || uid <= 0 {
		return credentials{}, fmt.Errorf("dnsmgr 凭证文件 %s 中的 %s 必须是正整数", path, uidKey)
	}
	apiKey := values[apiKeyKey]
	if apiKey == "" {
		return credentials{}, fmt.Errorf("dnsmgr 凭证文件 %s 中的 %s 不能为空", path, apiKeyKey)
	}
	return credentials{uid: strconv.FormatInt(uid, 10), apiKey: apiKey}, nil
}

// CheckAuth 调用无副作用认证端点验证当前 AuthApi 凭证。
// @param ctx 控制 HTTP 请求取消与超时。
// @return error 请求、HTTP 状态、JSON 或业务码无效时的脱敏错误。
func (c *Client) CheckAuth(ctx context.Context) error {
	return c.post(ctx, "/api/auth/check", nil, nil)
}

// SetTaskActive 启用或暂停单个 dnsmgr dmonitor 任务。
// @param ctx 控制 HTTP 请求取消与超时。
// @param taskID dnsmgr dmtask 表中的任务 ID。
// @param active true 表示启用，false 表示暂停。
// @return error 参数、请求、响应或业务码无效时的脱敏错误。
func (c *Client) SetTaskActive(ctx context.Context, taskID int64, active bool) error {
	if taskID <= 0 {
		return fmt.Errorf("dnsmgr dmonitor task_id 必须大于 0")
	}
	activeValue := "0"
	if active {
		activeValue = "1"
	}
	return c.post(ctx, "/api/dmonitor/task/setactive", url.Values{
		"id":     {strconv.FormatInt(taskID, 10)},
		"active": {activeValue},
	}, nil)
}

// SetRecordValue 只修改一条 A 或 AAAA 记录的 IP。
// @param ctx 控制 HTTP 请求取消与超时。
// @param domainID dnsmgr 本地 domain ID。
// @param recordID provider 记录 ID。
// @param value 新 IPv4 或 IPv6。
// @param expectedValue 可选当前值约束，用于补偿时避免覆盖并发切换。
// @return ValueResult dnsmgr 返回的旧值、当前值和幂等状态。
// @return error 参数、请求、响应或返回契约不满足要求时的脱敏错误。
func (c *Client) SetRecordValue(
	ctx context.Context,
	domainID int64,
	recordID string,
	value string,
	expectedValue *string,
) (ValueResult, error) {
	if domainID <= 0 {
		return ValueResult{}, fmt.Errorf("dnsmgr domain_id 必须大于 0")
	}
	recordID = strings.TrimSpace(recordID)
	if recordID == "" {
		return ValueResult{}, fmt.Errorf("dnsmgr record_id 不能为空")
	}
	value = strings.TrimSpace(value)
	valueIP := net.ParseIP(value)
	if valueIP == nil {
		return ValueResult{}, fmt.Errorf("dnsmgr 目标值不是合法 IP")
	}
	form := url.Values{
		"recordid": {recordID},
		"value":    {value},
	}
	if expectedValue != nil {
		expected := strings.TrimSpace(*expectedValue)
		expectedIP := net.ParseIP(expected)
		if expectedIP == nil {
			return ValueResult{}, fmt.Errorf("dnsmgr 期望值不是合法 IP")
		}
		if !sameAddressFamily(valueIP, expectedIP) {
			return ValueResult{}, fmt.Errorf("dnsmgr 期望值与目标值地址族不一致")
		}
		form.Set("expected_value", expected)
	}

	var response apiResponse
	if err := c.post(ctx, "/api/record/value/"+strconv.FormatInt(domainID, 10), form, &response); err != nil {
		return ValueResult{}, err
	}
	previousIP := net.ParseIP(strings.TrimSpace(response.Data.PreviousValue))
	resultIP := net.ParseIP(strings.TrimSpace(response.Data.Value))
	if response.Data.RecordID != recordID || previousIP == nil || resultIP == nil ||
		!sameAddressFamily(valueIP, previousIP) || !sameAddressFamily(valueIP, resultIP) || !resultIP.Equal(valueIP) ||
		(response.Data.Changed == previousIP.Equal(resultIP)) {
		return ValueResult{}, fmt.Errorf("dnsmgr Value-only API 返回数据不符合契约")
	}
	return ValueResult{
		RecordID:      response.Data.RecordID,
		PreviousValue: response.Data.PreviousValue,
		Value:         response.Data.Value,
		Changed:       response.Data.Changed,
	}, nil
}

func sameAddressFamily(left net.IP, right net.IP) bool {
	return (left.To4() != nil) == (right.To4() != nil)
}

func (c *Client) post(ctx context.Context, route string, form url.Values, decoded any) error {
	if c == nil || c.baseURL == nil || c.httpClient == nil || c.now == nil {
		return fmt.Errorf("dnsmgr client 未初始化")
	}
	requestForm := make(url.Values, len(form)+3)
	for key, values := range form {
		requestForm[key] = append([]string(nil), values...)
	}
	timestamp := strconv.FormatInt(c.now().Unix(), 10)
	requestForm.Set("uid", c.credentials.uid)
	requestForm.Set("timestamp", timestamp)
	requestForm.Set("sign", authSign(c.credentials.uid, timestamp, c.credentials.apiKey))

	target := *c.baseURL
	target.Path = strings.TrimRight(target.Path, "/") + route
	target.RawPath = ""
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), strings.NewReader(requestForm.Encode()))
	if err != nil {
		return fmt.Errorf("创建 dnsmgr 请求失败: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("调用 dnsmgr 失败: %w", redactHTTPError(err))
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, maximumResponseBody+1))
	if err != nil {
		return fmt.Errorf("读取 dnsmgr 响应失败")
	}
	if len(body) > maximumResponseBody {
		return fmt.Errorf("dnsmgr 响应超过 %d 字节", maximumResponseBody)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("dnsmgr 返回 HTTP 状态 %d", response.StatusCode)
	}
	var envelope apiResponse
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("dnsmgr 返回无效 JSON")
	}
	if envelope.Code == nil {
		return fmt.Errorf("dnsmgr 响应缺少业务码")
	}
	if *envelope.Code != 0 {
		return fmt.Errorf("dnsmgr 返回业务错误码 %d", *envelope.Code)
	}
	if decoded != nil {
		if err := json.Unmarshal(body, decoded); err != nil {
			return fmt.Errorf("dnsmgr 返回数据无法解码")
		}
	}
	return nil
}

func authSign(uid string, timestamp string, apiKey string) string {
	sum := md5.Sum([]byte(uid + timestamp + apiKey))
	return hex.EncodeToString(sum[:])
}

func redactHTTPError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("HTTP 请求未完成")
}
