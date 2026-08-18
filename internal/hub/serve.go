package hub

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/silentflower/ark/internal/config"
	"github.com/silentflower/ark/internal/monitoring"
	"github.com/silentflower/ark/internal/store"
)

const shutdownTimeout = 10 * time.Second

// ServeOptions 描述 ark-hub HTTP 服务的本机运行参数。
type ServeOptions struct {
	// ListenAddress 是 net/http 监听地址，默认只绑定 loopback。
	ListenAddress string
	// StateDBPath 是通过 store.Store 打开的 ark 状态库路径。
	StateDBPath string
	// AuthFile 是独立于状态库的 root-only 管理员凭证文件。
	AuthFile string
	// ConfigPath 是每次业务 API 请求重新严格加载的 v2 清单绝对路径。
	ConfigPath string
	// ArkBinaryPath 是手工操作使用的 ark 普通可执行文件绝对路径。
	ArkBinaryPath string
	// SecureCookie 控制浏览器 Cookie 的 Secure 属性。
	SecureCookie bool
}

type serveDependencies struct {
	openStore      func(context.Context, string) (stateStore, error)
	listen         func(string, string) (net.Listener, error)
	newApplication func(string, bool) (*application, error)
	newServer      func(http.Handler) httpServerLifecycle
	loadConfig     func(string) (*config.Config, error)
	loadMonitoring func(string) (monitoring.Settings, error)
	sendDingTalk   func(context.Context, monitoring.DingTalkSettings, monitoring.MarkdownMessage) error
	reportAlert    func(error)
	stat           func(string) (os.FileInfo, error)
	now            func() time.Time
}

type stateStore interface {
	Close() error
}

type operationRecoveryStore interface {
	InterruptRunningOperations(context.Context, time.Time) (int64, error)
}

type httpServerLifecycle interface {
	Serve(net.Listener) error
	Shutdown(context.Context) error
	Close() error
}

// Run 启动 ark-hub HTTP 服务，并在 context 取消后执行有界优雅停止。
// @param ctx 控制状态库初始化、服务运行和停止时机。
// @param options 监听地址、状态库、凭证文件与 Cookie 策略。
// @return error 初始化、监听、HTTP 运行、停止或状态库关闭失败时的聚合错误。
func Run(ctx context.Context, options ServeOptions) error {
	if strings.TrimSpace(options.ConfigPath) == "" {
		options.ConfigPath = defaultConfigPath
	}
	if strings.TrimSpace(options.ArkBinaryPath) == "" {
		options.ArkBinaryPath = defaultArkBinaryPath
	}
	return run(ctx, options, serveDependencies{
		openStore: func(ctx context.Context, path string) (stateStore, error) {
			return store.Open(ctx, path)
		},
		listen: net.Listen,
		newApplication: func(authFile string, secureCookie bool) (*application, error) {
			return newApplication(authFile, secureCookie, rand.Reader, time.Now)
		},
		newServer:      newHTTPServer,
		loadConfig:     config.LoadAndValidate,
		loadMonitoring: monitoring.Load,
		sendDingTalk:   monitoring.SendDingTalk,
		reportAlert: func(err error) {
			fmt.Fprintln(os.Stderr, "ark-hub 告警评估失败:", err)
		},
		stat: os.Stat,
		now:  time.Now,
	})
}

func run(ctx context.Context, options ServeOptions, dependencies serveDependencies) error {
	if ctx == nil {
		return fmt.Errorf("启动 ark-hub 失败: context 不能为空")
	}
	if strings.TrimSpace(options.ListenAddress) == "" {
		return fmt.Errorf("启动 ark-hub 失败: listen 地址不能为空")
	}
	if dependencies.openStore == nil || dependencies.listen == nil || dependencies.newApplication == nil ||
		dependencies.newServer == nil {
		return fmt.Errorf("启动 ark-hub 失败: 内部依赖不完整")
	}
	application, err := dependencies.newApplication(options.AuthFile, options.SecureCookie)
	if err != nil {
		return fmt.Errorf("启动 ark-hub 失败: %w", err)
	}
	if strings.TrimSpace(options.ConfigPath) != "" || strings.TrimSpace(options.ArkBinaryPath) != "" {
		if dependencies.loadConfig == nil || dependencies.loadMonitoring == nil ||
			dependencies.sendDingTalk == nil || dependencies.reportAlert == nil ||
			dependencies.stat == nil || dependencies.now == nil {
			return fmt.Errorf("启动 ark-hub 失败: 配置校验依赖不完整")
		}
		if err := validateRuntimePaths(options, dependencies); err != nil {
			return err
		}
	}
	state, err := dependencies.openStore(ctx, options.StateDBPath)
	if err != nil {
		return fmt.Errorf("启动 ark-hub 失败: %w", err)
	}
	var alerts *alertManager
	if options.ConfigPath != "" {
		recovery, ok := state.(operationRecoveryStore)
		if !ok {
			return errors.Join(fmt.Errorf("启动 ark-hub 失败: 状态库不支持恢复手工任务"), state.Close())
		}
		if _, err := recovery.InterruptRunningOperations(ctx, dependencies.now().UTC()); err != nil {
			return errors.Join(fmt.Errorf("启动 ark-hub 失败: %w", err), state.Close())
		}
		apiState, ok := state.(apiStore)
		if !ok {
			return errors.Join(fmt.Errorf("启动 ark-hub 失败: 状态库不支持 Hub API"), state.Close())
		}
		manager, managerErr := newOperationManager(
			apiState, options.ArkBinaryPath, options.ConfigPath, application.random, application.now,
		)
		if managerErr != nil {
			return errors.Join(fmt.Errorf("启动 ark-hub 失败: %w", managerErr), state.Close())
		}
		application.configureRuntime(
			state, options.ConfigPath, options.ArkBinaryPath, dependencies.loadConfig, manager,
		)
		alertState, ok := state.(alertStore)
		if !ok {
			return errors.Join(fmt.Errorf("启动 ark-hub 失败: 状态库不支持告警状态"), closeOperations(application), state.Close())
		}
		alerts, err = newAlertManager(
			alertState,
			options.ConfigPath,
			dependencies.loadConfig,
			dependencies.loadMonitoring,
			func(ctx context.Context, cfg *config.Config) ([]alertResponse, error) {
				_, currentAlerts, projectErr := application.projectHosts(ctx, cfg)
				return currentAlerts, projectErr
			},
			dependencies.sendDingTalk,
			application.now,
			alertEvaluationInterval,
			dependencies.reportAlert,
		)
		if err != nil {
			return errors.Join(fmt.Errorf("启动 ark-hub 失败: %w", err), closeOperations(application), state.Close())
		}
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(err, closeOperations(application), state.Close())
	}
	listener, err := dependencies.listen("tcp", options.ListenAddress)
	if err != nil {
		return errors.Join(
			fmt.Errorf("监听 %q 失败: %w", options.ListenAddress, err),
			closeOperations(application), state.Close(),
		)
	}

	server := dependencies.newServer(application.handler())
	if server == nil {
		return errors.Join(
			fmt.Errorf("启动 ark-hub 失败: HTTP server 不能为空"), listener.Close(),
			closeOperations(application), state.Close(),
		)
	}
	var stopAlerts context.CancelFunc
	var alertsDone chan struct{}
	if alerts != nil {
		alertContext, cancelAlerts := context.WithCancel(ctx)
		stopAlerts = cancelAlerts
		alertsDone = make(chan struct{})
		go func() {
			defer close(alertsDone)
			alerts.run(alertContext)
		}()
	}
	serveErrors := make(chan error, 1)
	go func() {
		serveErrors <- server.Serve(listener)
	}()

	var runErr error
	select {
	case serveErr := <-serveErrors:
		if serveErr == nil {
			runErr = fmt.Errorf("HTTP 服务意外停止")
		} else {
			runErr = fmt.Errorf("HTTP 服务意外停止: %w", serveErr)
		}
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		shutdownErr := server.Shutdown(shutdownContext)
		cancel()
		var closeErr error
		if shutdownErr != nil {
			closeErr = server.Close()
		}
		serveErr := <-serveErrors
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		runErr = errors.Join(shutdownErr, closeErr, serveErr)
	}
	if stopAlerts != nil {
		stopAlerts()
		<-alertsDone
	}
	return errors.Join(runErr, closeOperations(application), state.Close())
}

func closeOperations(application *application) error {
	if application == nil || application.operations == nil {
		return nil
	}
	return application.operations.close()
}

func validateRuntimePaths(options ServeOptions, dependencies serveDependencies) error {
	if !filepath.IsAbs(options.ConfigPath) {
		return fmt.Errorf("启动 ark-hub 失败: config 路径必须是绝对路径")
	}
	cfg, err := dependencies.loadConfig(options.ConfigPath)
	if err != nil {
		return fmt.Errorf("启动 ark-hub 失败: 清单校验失败: %w", err)
	}
	if cfg.Monitoring != nil {
		if _, err := dependencies.loadMonitoring(cfg.Monitoring.EnvFile); err != nil {
			return fmt.Errorf("启动 ark-hub 失败: 监控配置校验失败: %w", err)
		}
	}
	if !filepath.IsAbs(options.ArkBinaryPath) {
		return fmt.Errorf("启动 ark-hub 失败: ark binary 路径必须是绝对路径")
	}
	info, err := dependencies.stat(options.ArkBinaryPath)
	if err != nil {
		return fmt.Errorf("启动 ark-hub 失败: 访问 ark binary 失败: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("启动 ark-hub 失败: ark binary 必须是可执行普通文件")
	}
	return nil
}

func newHTTPServer(handler http.Handler) httpServerLifecycle {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
}

func newServeCmd(dependencies commandDependencies) *cobra.Command {
	options := ServeOptions{
		ListenAddress: defaultListenAddress,
		StateDBPath:   store.DefaultPath,
		AuthFile:      defaultAuthFile,
		ConfigPath:    defaultConfigPath,
		ArkBinaryPath: defaultArkBinaryPath,
	}
	command := &cobra.Command{
		Use:   "serve",
		Short: "启动 ark-hub HTTP 服务",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if dependencies.run == nil {
				return fmt.Errorf("启动 ark-hub 失败: 内部依赖不完整")
			}
			return dependencies.run(command.Context(), options)
		},
	}
	command.Flags().StringVar(&options.ListenAddress, "listen", defaultListenAddress, "HTTP 监听地址")
	command.Flags().StringVar(&options.StateDBPath, "state-db", store.DefaultPath, "ark 状态库路径")
	command.Flags().StringVar(&options.AuthFile, "auth-file", defaultAuthFile, "管理员凭证文件路径")
	command.Flags().StringVar(&options.ConfigPath, "config", defaultConfigPath, "ark v2 清单绝对路径")
	command.Flags().StringVar(&options.ArkBinaryPath, "ark-binary", defaultArkBinaryPath, "ark 可执行文件绝对路径")
	command.Flags().BoolVar(&options.SecureCookie, "secure-cookie", false, "为浏览器 Cookie 设置 Secure")
	return command
}
