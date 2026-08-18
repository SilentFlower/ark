// Package monitoring 加载告警秘密配置，并提供钉钉与外部心跳的受控 HTTP 出站边界。
//
// 本包不理解备份、Hub 或状态库语义。调用方只传递已经形成的终态或 Markdown，
// 本包负责秘密文件安全、端点校验、请求超时、有限重试和响应上限。
package monitoring

import (
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/silentflower/ark/internal/endpoint"
	"github.com/silentflower/ark/internal/envfile"
	"github.com/silentflower/ark/internal/secretfile"
)

const (
	dingTalkWebhookKey     = "ARK_DINGTALK_WEBHOOK_URL"
	dingTalkSecretKey      = "ARK_DINGTALK_SECRET"
	heartbeatSuccessURLKey = "ARK_HEARTBEAT_SUCCESS_URL"
	heartbeatFailureURLKey = "ARK_HEARTBEAT_FAILURE_URL"
)

var allowedKeys = map[string]struct{}{
	dingTalkWebhookKey:     {},
	dingTalkSecretKey:      {},
	heartbeatSuccessURLKey: {},
	heartbeatFailureURLKey: {},
}

// Settings 是秘密文件中启用的监控能力。
type Settings struct {
	DingTalk  *DingTalkSettings
	Heartbeat *HeartbeatSettings
}

// DingTalkSettings 是已经校验过的钉钉 Webhook 配置。
//
// 字段保持包内私有，避免调用方意外把完整 URL 或签名密钥序列化到日志与状态库。
type DingTalkSettings struct {
	webhookURL *url.URL
	secret     string
}

// HeartbeatSettings 是已经校验过的成功/失败双端点配置。
//
// 字段保持包内私有，避免 URL 中的认证材料越过 HTTP 出站边界。
type HeartbeatSettings struct {
	successURL *url.URL
	failureURL *url.URL
}

// HeartbeatStatus 是 backup 摘要中的心跳投递状态。
type HeartbeatStatus string

const (
	// HeartbeatDisabled 表示未配置心跳端点。
	HeartbeatDisabled HeartbeatStatus = "disabled"
	// HeartbeatSent 表示心跳端点已成功接收请求。
	HeartbeatSent HeartbeatStatus = "sent"
	// HeartbeatFailed 表示配置或网络错误导致心跳未成功投递。
	HeartbeatFailed HeartbeatStatus = "failed"
)

// Load 安全打开并解析监控秘密文件。
// @param path 监控秘密文件的绝对路径。
// @return Settings 已校验且不会向外暴露秘密值的监控配置。
// @return error 文件安全、语法、键组合或 URL 校验错误；错误不会包含秘密值。
func Load(path string) (Settings, error) {
	file, err := secretfile.Open(path, "monitoring.env_file")
	if err != nil {
		return Settings{}, err
	}
	defer func() { _ = file.Close() }()

	values, err := envfile.ParseReader(file, path)
	if err != nil {
		return Settings{}, err
	}
	if err := rejectUnknownKeys(path, values); err != nil {
		return Settings{}, err
	}

	var settings Settings
	webhook, hasWebhook := values[dingTalkWebhookKey]
	secret, hasSecret := values[dingTalkSecretKey]
	if hasSecret && secret == "" {
		return Settings{}, fmt.Errorf("监控凭证文件 %s 中的 %s 不能为空", path, dingTalkSecretKey)
	}
	if hasSecret && !hasWebhook {
		return Settings{}, fmt.Errorf("监控凭证文件 %s 中的 %s 必须与 %s 同时配置", path, dingTalkSecretKey, dingTalkWebhookKey)
	}
	if hasWebhook {
		endpoint, err := parseEndpoint(dingTalkWebhookKey, webhook)
		if err != nil {
			return Settings{}, fmt.Errorf("监控凭证文件 %s 配置无效: %w", path, err)
		}
		settings.DingTalk = &DingTalkSettings{webhookURL: endpoint, secret: secret}
	}

	success, hasSuccess := values[heartbeatSuccessURLKey]
	failure, hasFailure := values[heartbeatFailureURLKey]
	if hasSuccess != hasFailure {
		return Settings{}, fmt.Errorf("监控凭证文件 %s 中的 %s 与 %s 必须同时配置", path, heartbeatSuccessURLKey, heartbeatFailureURLKey)
	}
	if hasSuccess {
		successEndpoint, err := parseEndpoint(heartbeatSuccessURLKey, success)
		if err != nil {
			return Settings{}, fmt.Errorf("监控凭证文件 %s 配置无效: %w", path, err)
		}
		failureEndpoint, err := parseEndpoint(heartbeatFailureURLKey, failure)
		if err != nil {
			return Settings{}, fmt.Errorf("监控凭证文件 %s 配置无效: %w", path, err)
		}
		settings.Heartbeat = &HeartbeatSettings{successURL: successEndpoint, failureURL: failureEndpoint}
	}
	return settings, nil
}

func rejectUnknownKeys(path string, values map[string]string) error {
	var unknown []string
	for key := range values {
		if _, ok := allowedKeys[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return fmt.Errorf("监控凭证文件 %s 包含不支持的键 %s", path, strings.Join(unknown, ", "))
}

func parseEndpoint(name, value string) (*url.URL, error) {
	return endpoint.ParseHTTPSOrLoopback(name, value)
}
