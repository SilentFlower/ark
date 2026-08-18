package monitoring

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// MarkdownMessage 是发送给钉钉机器人的 Markdown 消息。
type MarkdownMessage struct {
	Title string
	Text  string
}

// SendDingTalk 向已校验的钉钉 Webhook 发送 Markdown 消息。
// @param ctx 控制请求、重试等待和取消。
// @param settings Load 返回的钉钉配置。
// @param message 不含秘密值的 Markdown 标题与正文。
// @return error 超时、HTTP、响应上限、JSON 或钉钉业务错误；错误不会包含秘密值。
func SendDingTalk(ctx context.Context, settings DingTalkSettings, message MarkdownMessage) error {
	return newDeliveryClient().sendDingTalk(ctx, settings, message)
}

func (c deliveryClient) sendDingTalk(ctx context.Context, settings DingTalkSettings, message MarkdownMessage) error {
	if settings.webhookURL == nil {
		return errors.New("钉钉投递失败: Webhook 配置为空")
	}
	if strings.TrimSpace(message.Title) == "" || strings.TrimSpace(message.Text) == "" {
		return errors.New("钉钉投递失败: Markdown 标题和正文不能为空")
	}
	endpoint := signedDingTalkURL(settings.webhookURL, settings.secret, c.now())
	payload, err := json.Marshal(struct {
		MessageType string `json:"msgtype"`
		Markdown    struct {
			Title string `json:"title"`
			Text  string `json:"text"`
		} `json:"markdown"`
	}{
		MessageType: "markdown",
		Markdown: struct {
			Title string `json:"title"`
			Text  string `json:"text"`
		}{Title: message.Title, Text: message.Text},
	})
	if err != nil {
		return errors.New("钉钉投递失败: 无法编码 Markdown")
	}
	return c.send(ctx, "钉钉", http.MethodPost, endpoint, payload, "application/json", validateDingTalkResponse)
}

func signedDingTalkURL(endpoint *url.URL, secret string, now time.Time) *url.URL {
	cloned := *endpoint
	if secret == "" {
		return &cloned
	}
	timestamp := strconv.FormatInt(now.UnixMilli(), 10)
	stringToSign := timestamp + "\n" + secret
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(stringToSign))
	signature := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	query := cloned.Query()
	query.Set("timestamp", timestamp)
	query.Set("sign", signature)
	cloned.RawQuery = query.Encode()
	return &cloned
}

func validateDingTalkResponse(payload []byte) error {
	var response struct {
		ErrorCode *int `json:"errcode"`
	}
	if err := json.Unmarshal(payload, &response); err != nil || response.ErrorCode == nil {
		return errors.New("钉钉响应不是合法的业务 JSON")
	}
	if *response.ErrorCode != 0 {
		return fmt.Errorf("钉钉返回业务错误码 %d", *response.ErrorCode)
	}
	return nil
}
