package hub

import (
	"context"
	"crypto/rand"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/silentflower/ark/internal/store"
	arksystemd "github.com/silentflower/ark/internal/systemd"
)

func TestRun_监听后响应Context取消并优雅退出(t *testing.T) {
	directory := t.TempDir()
	authFile := filepath.Join(directory, "auth", "auth.json")
	if err := initializeCredential(authFile, "admin", append([]byte(nil), testPassword...)); err != nil {
		t.Fatalf("初始化凭证失败: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	listenerReady := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, ServeOptions{
			ListenAddress: "127.0.0.1:0",
			StateDBPath:   filepath.Join(directory, "ark.db"),
			AuthFile:      authFile,
		}, serveDependencies{
			openStore: func(ctx context.Context, path string) (stateStore, error) {
				return store.Open(ctx, path)
			},
			listen: func(network, address string) (net.Listener, error) {
				listener, err := net.Listen(network, address)
				if err == nil {
					close(listenerReady)
				}
				return listener, err
			},
			newApplication: func(path string, secure bool) (*application, error) {
				return newApplication(path, secure, rand.Reader, time.Now)
			},
			newServer: newHTTPServer,
		})
	}()
	select {
	case <-listenerReady:
		cancel()
	case <-time.After(5 * time.Second):
		t.Fatal("ark-hub 未进入监听")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("优雅退出错误: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ark-hub 未在取消后退出")
	}
}

func TestRun_凭证失败时不打开状态库或监听(t *testing.T) {
	opened := false
	listened := false
	err := run(context.Background(), ServeOptions{
		ListenAddress: "127.0.0.1:0",
		StateDBPath:   filepath.Join(t.TempDir(), "ark.db"),
		AuthFile:      testCredentialPath(t),
	}, serveDependencies{
		openStore: func(context.Context, string) (stateStore, error) {
			opened = true
			return nil, errors.New("不应调用")
		},
		listen: func(string, string) (net.Listener, error) {
			listened = true
			return nil, errors.New("不应调用")
		},
		newApplication: func(path string, secure bool) (*application, error) {
			return newApplication(path, secure, rand.Reader, time.Now)
		},
		newServer: newHTTPServer,
	})
	if err == nil || !strings.Contains(err.Error(), "admin init") {
		t.Fatalf("凭证失败错误 = %v", err)
	}
	if opened || listened {
		t.Fatalf("凭证失败后 opened=%v listened=%v", opened, listened)
	}
}

func TestRun_状态库与监听失败没有静默成功(t *testing.T) {
	openFailure := errors.New("open store failure")
	listened := false
	err := run(context.Background(), ServeOptions{ListenAddress: "127.0.0.1:0"}, serveDependencies{
		openStore: func(context.Context, string) (stateStore, error) { return nil, openFailure },
		listen: func(string, string) (net.Listener, error) {
			listened = true
			return nil, errors.New("不应调用")
		},
		newApplication: func(string, bool) (*application, error) { return &application{}, nil },
		newServer:      newHTTPServer,
	})
	if !errors.Is(err, openFailure) || listened {
		t.Fatalf("状态库失败 err=%v listened=%v", err, listened)
	}

	listenFailure := errors.New("listen failure")
	closeFailure := errors.New("store close failure")
	state := &fakeStateStore{closeErr: closeFailure}
	err = run(context.Background(), ServeOptions{ListenAddress: "127.0.0.1:0"}, serveDependencies{
		openStore:      func(context.Context, string) (stateStore, error) { return state, nil },
		listen:         func(string, string) (net.Listener, error) { return nil, listenFailure },
		newApplication: func(string, bool) (*application, error) { return &application{}, nil },
		newServer:      newHTTPServer,
	})
	if !errors.Is(err, listenFailure) || !errors.Is(err, closeFailure) || !state.closed {
		t.Fatalf("监听失败聚合 err=%v state=%#v", err, state)
	}
}

func TestRun_聚合ShutdownClose与StoreClose错误(t *testing.T) {
	shutdownFailure := errors.New("shutdown failure")
	serverCloseFailure := errors.New("server close failure")
	storeCloseFailure := errors.New("store close failure")
	server := newFakeHTTPServer(http.ErrServerClosed, shutdownFailure, serverCloseFailure)
	state := &fakeStateStore{closeErr: storeCloseFailure}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, ServeOptions{ListenAddress: "127.0.0.1:0"}, serveDependencies{
			openStore:      func(context.Context, string) (stateStore, error) { return state, nil },
			listen:         func(string, string) (net.Listener, error) { return stubListener{}, nil },
			newApplication: func(string, bool) (*application, error) { return &application{}, nil },
			newServer:      func(http.Handler) httpServerLifecycle { return server },
		})
	}()
	select {
	case <-server.started:
		cancel()
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP server 未启动")
	}
	err := <-done
	for _, expected := range []error{shutdownFailure, serverCloseFailure, storeCloseFailure} {
		if !errors.Is(err, expected) {
			t.Fatalf("聚合错误缺少 %v: %v", expected, err)
		}
	}
}

func TestRun_HTTP服务意外停止仍关闭状态库(t *testing.T) {
	serveFailure := errors.New("serve failure")
	server := newFakeHTTPServer(serveFailure, nil, nil)
	server.release()
	state := &fakeStateStore{}
	err := run(context.Background(), ServeOptions{ListenAddress: "127.0.0.1:0"}, serveDependencies{
		openStore:      func(context.Context, string) (stateStore, error) { return state, nil },
		listen:         func(string, string) (net.Listener, error) { return stubListener{}, nil },
		newApplication: func(string, bool) (*application, error) { return &application{}, nil },
		newServer:      func(http.Handler) httpServerLifecycle { return server },
	})
	if !errors.Is(err, serveFailure) || !state.closed {
		t.Fatalf("意外停止 err=%v state=%#v", err, state)
	}
}

func TestServeCommand_传递全部运行参数(t *testing.T) {
	var got ServeOptions
	command := newServeCmd(commandDependencies{
		run: func(_ context.Context, options ServeOptions) error {
			got = options
			return nil
		},
	})
	command.SetArgs([]string{
		"--listen", "127.0.0.1:9090",
		"--state-db", "/srv/ark.db",
		"--auth-file", "/srv/auth.json",
		"--secure-cookie",
	})
	if err := command.Execute(); err != nil {
		t.Fatalf("serve 命令失败: %v", err)
	}
	want := ServeOptions{
		ListenAddress: "127.0.0.1:9090",
		StateDBPath:   "/srv/ark.db",
		AuthFile:      "/srv/auth.json",
		SecureCookie:  true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ServeOptions=%#v，期望 %#v", got, want)
	}
}

func TestInstallCommand_只传递HubService参数(t *testing.T) {
	var got arksystemd.HubInstallOptions
	command := newInstallCmd(commandDependencies{
		executable: func() (string, error) { return "/opt/ark/bin/ark-hub", nil },
		installService: func(_ context.Context, options arksystemd.HubInstallOptions) (arksystemd.InstallResult, error) {
			got = options
			return arksystemd.InstallResult{Written: []string{"ark-hub.service"}}, nil
		},
	})
	command.SetArgs([]string{
		"--unit-dir", "/tmp/systemd",
		"--listen", "127.0.0.1:9090",
		"--state-db", "/srv/ark.db",
		"--auth-file", "/srv/auth.json",
		"--secure-cookie",
	})
	if err := command.Execute(); err != nil {
		t.Fatalf("install 命令失败: %v", err)
	}
	want := arksystemd.HubInstallOptions{
		UnitDir:       "/tmp/systemd",
		BinaryPath:    "/opt/ark/bin/ark-hub",
		ListenAddress: "127.0.0.1:9090",
		StateDBPath:   "/srv/ark.db",
		AuthFile:      "/srv/auth.json",
		SecureCookie:  true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("HubInstallOptions=%#v，期望 %#v", got, want)
	}
}

type fakeStateStore struct {
	closeErr error
	closed   bool
}

func (state *fakeStateStore) Close() error {
	state.closed = true
	return state.closeErr
}

type fakeHTTPServer struct {
	started     chan struct{}
	stop        chan struct{}
	stopOnce    sync.Once
	serveErr    error
	shutdownErr error
	closeErr    error
}

func newFakeHTTPServer(serveErr, shutdownErr, closeErr error) *fakeHTTPServer {
	return &fakeHTTPServer{
		started:     make(chan struct{}),
		stop:        make(chan struct{}),
		serveErr:    serveErr,
		shutdownErr: shutdownErr,
		closeErr:    closeErr,
	}
}

func (server *fakeHTTPServer) Serve(net.Listener) error {
	close(server.started)
	<-server.stop
	return server.serveErr
}

func (server *fakeHTTPServer) Shutdown(context.Context) error {
	server.release()
	return server.shutdownErr
}

func (server *fakeHTTPServer) Close() error {
	server.release()
	return server.closeErr
}

func (server *fakeHTTPServer) release() {
	server.stopOnce.Do(func() { close(server.stop) })
}

type stubListener struct{}

func (stubListener) Accept() (net.Conn, error) {
	return nil, errors.New("stub listener 不接受连接")
}
func (stubListener) Close() error   { return nil }
func (stubListener) Addr() net.Addr { return stubAddress("stub") }

type stubAddress string

func (address stubAddress) Network() string { return string(address) }
func (address stubAddress) String() string  { return string(address) }
