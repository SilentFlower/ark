package monitoring

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const (
	httpTimeout         = 5 * time.Second
	maximumResponseBody = 64 * 1024
	defaultAttempts     = 3
	initialRetryDelay   = 200 * time.Millisecond
)

type httpDoer interface {
	Do(request *http.Request) (*http.Response, error)
}

type deliveryClient struct {
	httpClient httpDoer
	now        func() time.Time
	wait       func(context.Context, time.Duration) error
	attempts   int
}

func newDeliveryClient() deliveryClient {
	return deliveryClient{
		httpClient: &http.Client{
			Timeout: httpTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				// 端点只在加载秘密配置时校验一次；禁止重定向，避免 3xx 绕过 HTTPS/loopback 边界。
				return http.ErrUseLastResponse
			},
		},
		now:      time.Now,
		wait:     waitForRetry,
		attempts: defaultAttempts,
	}
}

func (c deliveryClient) send(
	ctx context.Context,
	label string,
	method string,
	endpoint *url.URL,
	payload []byte,
	contentType string,
	validate func([]byte) error,
) error {
	if c.httpClient == nil || c.wait == nil || c.attempts < 1 {
		return fmt.Errorf("%s投递失败: 内部 HTTP 依赖不完整", label)
	}
	for attempt := 1; attempt <= c.attempts; attempt++ {
		request, err := newRequest(ctx, method, endpoint, payload, contentType)
		if err != nil {
			return fmt.Errorf("%s投递失败: 无法构造请求", label)
		}
		response, err := c.httpClient.Do(request)
		if err == nil {
			body, readErr := readBoundedBody(response.Body)
			if readErr != nil {
				return fmt.Errorf("%s响应无效: %w", label, readErr)
			}
			switch {
			case response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices:
				if validate != nil {
					if err := validate(body); err != nil {
						return err
					}
				}
				return nil
			case !retryableStatus(response.StatusCode):
				return fmt.Errorf("%s端点返回 HTTP 状态 %d", label, response.StatusCode)
			}
		} else if ctx.Err() != nil {
			return fmt.Errorf("%s请求已取消: %w", label, ctx.Err())
		}

		if attempt == c.attempts {
			break
		}
		delay := initialRetryDelay << (attempt - 1)
		if err := c.wait(ctx, delay); err != nil {
			return fmt.Errorf("%s重试已取消: %w", label, err)
		}
	}
	return fmt.Errorf("%s投递失败，已完成 %d 次尝试", label, c.attempts)
}

func newRequest(
	ctx context.Context,
	method string,
	endpoint *url.URL,
	payload []byte,
	contentType string,
) (*http.Request, error) {
	if endpoint == nil {
		return nil, errors.New("端点为空")
	}
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	return request, nil
}

func readBoundedBody(body io.ReadCloser) ([]byte, error) {
	if body == nil {
		return nil, errors.New("响应体为空")
	}
	payload, readErr := io.ReadAll(io.LimitReader(body, maximumResponseBody+1))
	closeErr := body.Close()
	if readErr != nil {
		return nil, errors.New("读取响应体失败")
	}
	if closeErr != nil {
		return nil, errors.New("关闭响应体失败")
	}
	if len(payload) > maximumResponseBody {
		return nil, fmt.Errorf("响应体超过 %d 字节上限", maximumResponseBody)
	}
	return payload, nil
}

func retryableStatus(statusCode int) bool {
	return statusCode == http.StatusTooManyRequests || statusCode >= http.StatusInternalServerError
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
