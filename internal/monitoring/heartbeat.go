package monitoring

import (
	"context"
	"errors"
	"net/http"
)

// SendHeartbeat 向成功或失败端点发送一次无 body 的 GET 请求。
// @param ctx 控制请求、重试等待和取消。
// @param settings Load 返回的心跳双端点配置。
// @param failed 为 true 时选择失败端点，否则选择成功端点。
// @return error 超时、HTTP、响应读取或响应上限错误；错误不会包含端点 URL。
func SendHeartbeat(ctx context.Context, settings HeartbeatSettings, failed bool) error {
	return newDeliveryClient().sendHeartbeat(ctx, settings, failed)
}

func (c deliveryClient) sendHeartbeat(ctx context.Context, settings HeartbeatSettings, failed bool) error {
	endpoint := settings.successURL
	if failed {
		endpoint = settings.failureURL
	}
	if endpoint == nil {
		return errors.New("外部心跳投递失败: 端点配置为空")
	}
	return c.send(ctx, "外部心跳", http.MethodGet, endpoint, nil, "", nil)
}
