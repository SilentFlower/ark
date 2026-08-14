package hub

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

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
	// SecureCookie 控制浏览器 Cookie 的 Secure 属性。
	SecureCookie bool
}

type serveDependencies struct {
	openStore      func(context.Context, string) (stateStore, error)
	listen         func(string, string) (net.Listener, error)
	newApplication func(string, bool) (*application, error)
	newServer      func(http.Handler) httpServerLifecycle
}

type stateStore interface {
	Close() error
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
	return run(ctx, options, serveDependencies{
		openStore: func(ctx context.Context, path string) (stateStore, error) {
			return store.Open(ctx, path)
		},
		listen: net.Listen,
		newApplication: func(authFile string, secureCookie bool) (*application, error) {
			return newApplication(authFile, secureCookie, rand.Reader, time.Now)
		},
		newServer: newHTTPServer,
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
	state, err := dependencies.openStore(ctx, options.StateDBPath)
	if err != nil {
		return fmt.Errorf("启动 ark-hub 失败: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(err, state.Close())
	}
	listener, err := dependencies.listen("tcp", options.ListenAddress)
	if err != nil {
		return errors.Join(fmt.Errorf("监听 %q 失败: %w", options.ListenAddress, err), state.Close())
	}

	server := dependencies.newServer(application.handler())
	if server == nil {
		return errors.Join(fmt.Errorf("启动 ark-hub 失败: HTTP server 不能为空"), listener.Close(), state.Close())
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
	return errors.Join(runErr, state.Close())
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
	command.Flags().BoolVar(&options.SecureCookie, "secure-cookie", false, "为浏览器 Cookie 设置 Secure")
	return command
}
