package doctor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silentflower/ark/internal/config"
)

type fakeDNSAuthChecker struct {
	err error
}

func (f fakeDNSAuthChecker) CheckAuth(context.Context) error {
	return f.err
}

func TestRunDNSMgr_未配置时不创建客户端(t *testing.T) {
	called := false
	report := runDNSMgr(context.Background(), &config.Config{}, func(string, string) (dnsAuthChecker, error) {
		called = true
		return fakeDNSAuthChecker{}, nil
	})
	if called || len(report.Checks) != 0 {
		t.Fatalf("未配置 dnsmgr 时结果 = %#v, called=%v", report.Checks, called)
	}
}

func TestRunDNSMgr_认证成功(t *testing.T) {
	cfg := &config.Config{DNSMgr: &config.DNSMgr{BaseURL: "https://dns.example", EnvFile: "/etc/ark/dnsmgr.env"}}
	report := runDNSMgr(context.Background(), cfg, func(baseURL string, envFile string) (dnsAuthChecker, error) {
		if baseURL != cfg.DNSMgr.BaseURL || envFile != cfg.DNSMgr.EnvFile {
			t.Fatalf("客户端参数 = %q %q", baseURL, envFile)
		}
		return fakeDNSAuthChecker{}, nil
	})
	if report.Failed() || len(report.Checks) != 1 || report.Checks[0].Status != StatusOK {
		t.Fatalf("dnsmgr doctor 报告 = %#v", report.Checks)
	}
}

func TestRunDNSMgr_认证失败不泄漏底层错误(t *testing.T) {
	const secret = "secret-api-key"
	cfg := &config.Config{DNSMgr: &config.DNSMgr{BaseURL: "https://dns.example", EnvFile: "/etc/ark/dnsmgr.env"}}
	report := runDNSMgr(context.Background(), cfg, func(string, string) (dnsAuthChecker, error) {
		return fakeDNSAuthChecker{err: errors.New("upstream: " + secret)}, nil
	})
	if !report.Failed() || len(report.Checks) != 1 || strings.Contains(report.Checks[0].Detail, secret) {
		t.Fatalf("dnsmgr doctor 失败报告 = %#v", report.Checks)
	}
}

func TestRunDNSMgr_真实Client覆盖凭证与网络边界(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/auth/check" {
			t.Fatalf("认证路径 = %q", request.URL.Path)
		}
		_, _ = writer.Write([]byte(`{"code":0}`))
	}))
	credentialsPath := filepath.Join(t.TempDir(), "dnsmgr.env")
	if err := os.WriteFile(credentialsPath, []byte("ARK_DNSMGR_UID=12\nARK_DNSMGR_API_KEY=secret\n"), 0o600); err != nil {
		t.Fatalf("写入凭证失败: %v", err)
	}
	cfg := &config.Config{DNSMgr: &config.DNSMgr{BaseURL: server.URL, EnvFile: credentialsPath}}
	if report := RunDNSMgr(context.Background(), cfg); report.Failed() || len(report.Checks) != 1 {
		t.Fatalf("真实认证报告 = %#v", report.Checks)
	}

	server.Close()
	report := RunDNSMgr(context.Background(), cfg)
	if !report.Failed() || strings.Contains(report.Checks[0].Detail, "secret") ||
		!strings.Contains(report.Checks[0].Detail, "认证检查失败") {
		t.Fatalf("网络失败报告 = %#v", report.Checks)
	}

	if err := os.Chmod(credentialsPath, 0o640); err != nil {
		t.Fatalf("修改凭证权限失败: %v", err)
	}
	report = RunDNSMgr(context.Background(), cfg)
	if !report.Failed() || !strings.Contains(report.Checks[0].Detail, "0600") {
		t.Fatalf("凭证权限报告 = %#v", report.Checks)
	}
}
