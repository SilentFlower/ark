// Package endpoint 校验可能携带认证信息的 HTTP 出站端点。
package endpoint

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ParseHTTPSOrLoopback 解析绝对 URL，并只允许 HTTPS 或 loopback HTTP。
// @param name 配置字段名，用于错误定位。
// @param value 待校验的 URL。
// @return *url.URL 已校验的绝对 URL。
// @return error URL 非法、含 userinfo/fragment 或协议不安全时的错误。
func ParseHTTPSOrLoopback(name string, value string) (*url.URL, error) {
	if value == "" {
		return nil, fmt.Errorf("%s 不能为空", name)
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.Opaque != "" {
		return nil, fmt.Errorf("%s 不是合法的绝对 URL", name)
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("%s 不允许包含 userinfo", name)
	}
	if parsed.Fragment != "" {
		return nil, fmt.Errorf("%s 不允许包含 fragment", name)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https":
	case "http":
		if !isLoopbackHost(parsed.Hostname()) {
			return nil, fmt.Errorf("%s 只允许 HTTPS；HTTP 仅可用于 loopback 地址", name)
		}
	default:
		return nil, fmt.Errorf("%s 只允许 HTTPS；HTTP 仅可用于 loopback 地址", name)
	}
	return parsed, nil
}

// ParseBaseURL 校验不含查询参数的 HTTPS 或 loopback HTTP 服务根地址。
// @param name 配置字段名，用于错误定位。
// @param value 待校验的服务根地址。
// @return *url.URL 已校验的服务根地址，可包含反向代理路径前缀。
// @return error 基础 URL 非法或包含查询参数时的错误。
func ParseBaseURL(name string, value string) (*url.URL, error) {
	parsed, err := ParseHTTPSOrLoopback(name, value)
	if err != nil {
		return nil, err
	}
	if parsed.RawQuery != "" {
		return nil, fmt.Errorf("%s 不允许包含 query", name)
	}
	return parsed, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
