package doctor

import (
	"context"

	"github.com/silentflower/ark/internal/config"
	"github.com/silentflower/ark/internal/dnsmgr"
)

type dnsAuthChecker interface {
	CheckAuth(context.Context) error
}

type newDNSAuthCheckerFunc func(string, string) (dnsAuthChecker, error)

// RunDNSMgr 检查可选 dnsmgr 配置、凭证文件与 AuthApi 认证端点。
// @param ctx 控制认证 HTTP 请求的取消与超时。
// @param cfg 已完成静态校验的 ark 清单。
// @return *Report 独立的 dnsmgr 检查报告；未配置 dnsmgr 时为空报告。
func RunDNSMgr(ctx context.Context, cfg *config.Config) *Report {
	return runDNSMgr(ctx, cfg, func(baseURL string, envFile string) (dnsAuthChecker, error) {
		return dnsmgr.New(baseURL, envFile)
	})
}

func runDNSMgr(ctx context.Context, cfg *config.Config, newChecker newDNSAuthCheckerFunc) *Report {
	report := &Report{}
	if cfg == nil {
		report.add("config", StatusFail, "清单为空")
		return report
	}
	if cfg.DNSMgr == nil {
		return report
	}
	if newChecker == nil {
		report.add("dnsmgr.auth", StatusFail, "内部依赖不完整")
		return report
	}
	checker, err := newChecker(cfg.DNSMgr.BaseURL, cfg.DNSMgr.EnvFile)
	if err != nil {
		report.add("dnsmgr.auth", StatusFail, "配置或凭证不可用: %v", err)
		return report
	}
	if err := checker.CheckAuth(ctx); err != nil {
		// AuthApi 的外部错误不进入报告，避免代理或上游错误正文泄漏认证信息。
		report.add("dnsmgr.auth", StatusFail, "AuthApi 认证检查失败")
		return report
	}
	report.add("dnsmgr.auth", StatusOK, "AuthApi 认证成功")
	return report
}
