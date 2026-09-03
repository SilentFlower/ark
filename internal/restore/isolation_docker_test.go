package restore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/silentflower/ark/internal/sshexec"
)

func TestIsolationDockerIntegration_与原项目并存并精确清理(t *testing.T) {
	if testing.Short() || os.Getenv("ARK_DOCKER_INTEGRATION") != "1" {
		t.Skip("设置 ARK_DOCKER_INTEGRATION=1 后运行真实 Docker 隔离测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	runner := sshexec.NewLocal()
	if _, err := runner.Run(ctx, "docker", "info"); err != nil {
		t.Skipf("Docker 不可用: %v", err)
	}
	if _, err := runner.Run(ctx, "docker", "image", "inspect", "redis:7-alpine"); err != nil {
		t.Skip("本机没有 redis:7-alpine，测试不会自动拉取镜像")
	}

	id := dockerIntegrationID(t)
	shortID := id[:12]
	prefix := "ark-it-" + shortID
	productionProject := prefix + "-prod"
	productionContainer := prefix + "-container"
	productionVolume := prefix + "-data"
	productionNetwork := prefix + "-net"
	isolationProject := productionProject + "-restore-" + shortID
	isolationVolume := isolationResourceName(productionVolume, IsolationPurposeRestore, shortID)
	isolationNetwork := isolationResourceName(productionNetwork, IsolationPurposeRestore, shortID)
	isolationRoot := filepath.Join(isolationBase, id)
	productionPort := dockerIntegrationFreePort(t)
	temporaryDirectory := t.TempDir()
	productionCompose := filepath.Join(temporaryDirectory, "compose.yaml")
	composeContent := fmt.Sprintf(`services:
  app:
    image: redis:7-alpine
    container_name: %s
    command: ["redis-server", "--save", ""]
    ports:
      - "127.0.0.1:%d:6379"
      - "127.0.0.1:%d:6379/udp"
    volumes:
      - data:/data
    networks:
      - app
volumes:
  data:
    name: %s
    driver: local
networks:
  app:
    name: %s
`, productionContainer, productionPort, productionPort, productionVolume, productionNetwork)
	if err := os.WriteFile(productionCompose, []byte(composeContent), 0o600); err != nil {
		t.Fatalf("写入生产 Compose 夹具失败: %v", err)
	}

	productionArgv := []string{"docker", "compose", "-f", productionCompose, "-p", productionProject}
	defer func() {
		_, _ = runner.Run(context.Background(), append(productionArgv, "down", "-v", "--remove-orphans")...)
		_, _ = runner.Run(context.Background(), "docker", "network", "rm", isolationNetwork)
		_, _ = runner.Run(context.Background(), "docker", "volume", "rm", isolationVolume)
		_ = os.RemoveAll(isolationRoot)
	}()
	if _, err := runner.Run(ctx, append(productionArgv, "up", "-d", "--no-build", "--pull", "never")...); err != nil {
		t.Fatalf("启动生产 Compose 夹具失败: %v", err)
	}
	if err := waitDockerContainerRunning(ctx, runner, productionContainer); err != nil {
		t.Fatal(err)
	}
	baseline := dockerIntegrationBaseline(t, ctx, runner, productionContainer, productionVolume, productionNetwork)

	canonical, err := runner.Run(ctx, append(productionArgv, "config", "--format", "json", "--no-env-resolution")...)
	if err != nil {
		t.Fatalf("读取生产 Compose canonical JSON 失败: %v", err)
	}
	spec := &IsolationSpec{
		SchemaVersion:        isolationSchemaVersion,
		ID:                   id,
		ShortID:              shortID,
		Purpose:              IsolationPurposeRestore,
		ProjectName:          isolationProject,
		Root:                 isolationRoot,
		FilesRoot:            filepath.Join(isolationRoot, "files"),
		GeneratedComposeFile: filepath.Join(isolationRoot, "compose.generated.json"),
		PortAllocation:       IsolationPortRuntimeAuto,
	}
	generated, services, volumes, networks, ports, _, err := transformIsolationCompose([]byte(canonical), spec)
	if err != nil {
		t.Fatalf("转换真实 Compose 失败: %v", err)
	}
	if len(services) != 1 || len(volumes) != 1 || len(networks) != 1 || len(ports) != 2 {
		t.Fatalf("隔离资源清单不完整: services=%#v volumes=%#v networks=%#v ports=%#v", services, volumes, networks, ports)
	}
	if err := os.MkdirAll(isolationRoot, 0o700); err != nil {
		t.Fatalf("创建 isolation root 失败: %v", err)
	}
	if err := os.WriteFile(spec.GeneratedComposeFile, append(generated, '\n'), 0o600); err != nil {
		t.Fatalf("写入隔离 Compose 失败: %v", err)
	}
	plan := Plan{
		DestinationHost: "docker-integration",
		Project: Project{
			Name:        prefix,
			ComposeFile: spec.GeneratedComposeFile,
			ProjectName: isolationProject,
		},
		Isolation: spec,
	}
	state := isolationState{
		SchemaVersion: isolationSchemaVersion,
		ID:            id,
		Purpose:       IsolationPurposeRestore,
		ExecutionID:   "docker-integration",
		Destination:   plan.DestinationHost,
		ProjectName:   isolationProject,
		Root:          isolationRoot,
		ComposeFile:   spec.GeneratedComposeFile,
		Services:      services,
		Volumes:       volumes,
		Networks:      networks,
		Ports:         ports,
	}
	statePayload, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("编码隔离状态失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(isolationRoot, "state.json"), append(statePayload, '\n'), 0o600); err != nil {
		t.Fatalf("写入隔离状态失败: %v", err)
	}
	defer func() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_, _ = CleanupIsolation(cleanupContext, runner, plan.DestinationHost, id)
	}()
	isolationArgv := []string{"docker", "compose", "-f", spec.GeneratedComposeFile, "-p", isolationProject}
	if _, err := runner.Run(ctx, append(isolationArgv, "up", "-d", "--no-build", "--pull", "never")...); err != nil {
		t.Fatalf("启动隔离 Compose 失败: %v", err)
	}
	state, err = inspectIsolationPorts(ctx, plan, runner, state)
	if err != nil {
		t.Fatalf("读取隔离端口失败: %v", err)
	}
	if len(state.Containers) != 1 || len(state.Ports) != 2 {
		t.Fatalf("隔离运行状态错误: %#v", state)
	}
	protocols := make(map[string]bool, len(state.Ports))
	for _, port := range state.Ports {
		protocols[port.Protocol] = true
		if port.AllocatedPort == "" || port.AllocatedPort == strconv.Itoa(productionPort) {
			t.Fatalf("隔离端口未自动重新分配: %#v", state.Ports)
		}
	}
	if !protocols["tcp"] || !protocols["udp"] {
		t.Fatalf("隔离端口缺少 TCP/UDP: %#v", state.Ports)
	}
	if after := dockerIntegrationBaseline(t, ctx, runner, productionContainer, productionVolume, productionNetwork); after != baseline {
		t.Fatalf("隔离副本启动后原项目发生变化:\n前=%s\n后=%s", baseline, after)
	}

	cleanupResult, err := CleanupIsolation(ctx, runner, plan.DestinationHost, id)
	if err != nil || cleanupResult.Status != "ok" {
		t.Fatalf("CleanupIsolation result=%#v err=%v", cleanupResult, err)
	}
	if after := dockerIntegrationBaseline(t, ctx, runner, productionContainer, productionVolume, productionNetwork); after != baseline {
		t.Fatalf("清理隔离副本后原项目发生变化:\n前=%s\n后=%s", baseline, after)
	}
	if _, err := os.Stat(isolationRoot); !os.IsNotExist(err) {
		t.Fatalf("隔离根目录未清理: %v", err)
	}
}

func TestIsolationDockerIntegration_Verify不发布端口且不改变生产基线(t *testing.T) {
	if testing.Short() || os.Getenv("ARK_DOCKER_INTEGRATION") != "1" {
		t.Skip("设置 ARK_DOCKER_INTEGRATION=1 后运行真实 Docker 隔离测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	runner := sshexec.NewLocal()
	if _, err := runner.Run(ctx, "docker", "info"); err != nil {
		t.Skipf("Docker 不可用: %v", err)
	}
	if _, err := runner.Run(ctx, "docker", "image", "inspect", "redis:7-alpine"); err != nil {
		t.Skip("本机没有 redis:7-alpine，测试不会自动拉取镜像")
	}

	id := dockerIntegrationID(t)
	shortID := id[:12]
	prefix := "ark-verify-it-" + shortID
	productionProject := prefix + "-prod"
	productionContainer := prefix + "-container"
	productionVolume := prefix + "-data"
	productionNetwork := prefix + "-net"
	isolationProject := productionProject + "-verify-" + shortID
	isolationVolume := isolationResourceName(productionVolume, IsolationPurposeVerify, shortID)
	isolationNetwork := isolationResourceName(productionNetwork, IsolationPurposeVerify, shortID)
	isolationRoot := filepath.Join(isolationBase, id)
	productionPort := dockerIntegrationFreePort(t)
	temporaryDirectory := t.TempDir()
	productionCompose := filepath.Join(temporaryDirectory, "compose.yaml")
	composeContent := fmt.Sprintf(`services:
  app:
    image: redis:7-alpine
    container_name: %s
    command: ["redis-server", "--save", ""]
    ports:
      - "127.0.0.1:%d:6379"
    volumes:
      - data:/data
    networks:
      - app
volumes:
  data:
    name: %s
    driver: local
networks:
  app:
    name: %s
    external: true
`, productionContainer, productionPort, productionVolume, productionNetwork)
	if err := os.WriteFile(productionCompose, []byte(composeContent), 0o600); err != nil {
		t.Fatalf("写入生产 Compose 夹具失败: %v", err)
	}

	productionArgv := []string{"docker", "compose", "-f", productionCompose, "-p", productionProject}
	defer func() {
		_, _ = runner.Run(context.Background(), append(productionArgv, "down", "-v", "--remove-orphans")...)
		_, _ = runner.Run(context.Background(), "docker", "network", "rm", isolationNetwork)
		_, _ = runner.Run(context.Background(), "docker", "network", "rm", productionNetwork)
		_, _ = runner.Run(context.Background(), "docker", "volume", "rm", isolationVolume)
		_ = os.RemoveAll(isolationRoot)
	}()
	if _, err := runner.Run(ctx, "docker", "network", "create", "--driver", "bridge", productionNetwork); err != nil {
		t.Fatalf("创建生产 external network 失败: %v", err)
	}
	if _, err := runner.Run(ctx, append(productionArgv, "up", "-d", "--no-build", "--pull", "never")...); err != nil {
		t.Fatalf("启动生产 Compose 夹具失败: %v", err)
	}
	if err := waitDockerContainerRunning(ctx, runner, productionContainer); err != nil {
		t.Fatal(err)
	}
	baseline := dockerIntegrationBaseline(t, ctx, runner, productionContainer, productionVolume, productionNetwork)

	canonical, err := runner.Run(ctx, append(productionArgv, "config", "--format", "json", "--no-env-resolution")...)
	if err != nil {
		t.Fatalf("读取生产 Compose canonical JSON 失败: %v", err)
	}
	spec := &IsolationSpec{
		SchemaVersion: isolationSchemaVersion, ID: id, ShortID: shortID, Purpose: IsolationPurposeVerify,
		ProjectName: isolationProject, Root: isolationRoot, FilesRoot: filepath.Join(isolationRoot, "files"),
		GeneratedComposeFile: filepath.Join(isolationRoot, "compose.generated.json"),
		PortAllocation:       IsolationPortDisabled,
	}
	generated, services, volumes, networks, ports, hostIPs, err := transformIsolationCompose([]byte(canonical), spec)
	if err != nil {
		t.Fatalf("转换 verify Compose 失败: %v", err)
	}
	if len(services) != 1 || len(volumes) != 1 || len(networks) != 1 || len(ports) != 1 || len(hostIPs) != 0 {
		t.Fatalf("verify 隔离资源清单错误: services=%#v volumes=%#v networks=%#v ports=%#v hostIPs=%#v",
			services, volumes, networks, ports, hostIPs)
	}
	if networks[0] != isolationNetwork {
		t.Fatalf("external network 未使用派生名称: networks=%#v want=%q", networks, isolationNetwork)
	}
	if strings.Contains(string(generated), `"ports"`) || ports[0].AllocatedPort != IsolationPortDisabled {
		t.Fatalf("verify Compose 仍发布端口: generated=%s ports=%#v", generated, ports)
	}
	if err := os.MkdirAll(isolationRoot, 0o700); err != nil {
		t.Fatalf("创建 verify isolation root 失败: %v", err)
	}
	if err := os.WriteFile(spec.GeneratedComposeFile, append(generated, '\n'), 0o600); err != nil {
		t.Fatalf("写入 verify Compose 失败: %v", err)
	}
	plan := Plan{
		DestinationHost: "docker-integration",
		Project:         Project{Name: prefix, ComposeFile: spec.GeneratedComposeFile, ProjectName: isolationProject},
		Isolation:       spec,
	}
	state := isolationState{
		SchemaVersion: isolationSchemaVersion, ID: id, Purpose: IsolationPurposeVerify,
		ExecutionID: "docker-integration", Destination: plan.DestinationHost,
		ProjectName: isolationProject, Root: isolationRoot, ComposeFile: spec.GeneratedComposeFile,
		Services: services, Volumes: volumes, Networks: networks, Ports: ports,
	}
	statePayload, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("编码 verify 隔离状态失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(isolationRoot, "state.json"), append(statePayload, '\n'), 0o600); err != nil {
		t.Fatalf("写入 verify 隔离状态失败: %v", err)
	}
	defer func() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_, _ = CleanupIsolation(cleanupContext, runner, plan.DestinationHost, id)
	}()
	isolationArgv := []string{"docker", "compose", "-f", spec.GeneratedComposeFile, "-p", isolationProject}
	if _, err := runner.Run(ctx, append(isolationArgv, "up", "-d", "--no-build", "--pull", "never")...); err != nil {
		t.Fatalf("启动 verify Compose 失败: %v", err)
	}
	state, err = inspectIsolationPorts(ctx, plan, runner, state)
	if err != nil || len(state.Containers) != 1 {
		t.Fatalf("读取 verify 容器失败: state=%#v err=%v", state, err)
	}
	published, err := runner.Run(ctx, "docker", "port", state.Containers[0].ID)
	if err != nil || strings.TrimSpace(published) != "" {
		t.Fatalf("verify 容器存在宿主机 published ports: output=%q err=%v", published, err)
	}
	attachedOutput, err := runner.Run(ctx, "docker", "inspect", "--format", "{{json .NetworkSettings.Networks}}", state.Containers[0].ID)
	if err != nil {
		t.Fatalf("读取 verify 容器网络失败: %v", err)
	}
	var attachedNetworks map[string]json.RawMessage
	if err := json.Unmarshal([]byte(attachedOutput), &attachedNetworks); err != nil {
		t.Fatalf("解析 verify 容器网络失败: %v", err)
	}
	if _, connected := attachedNetworks[productionNetwork]; connected {
		t.Fatalf("verify 容器错误连接生产 external network: %#v", attachedNetworks)
	}
	if _, connected := attachedNetworks[isolationNetwork]; !connected {
		t.Fatalf("verify 容器未连接派生 network: %#v", attachedNetworks)
	}
	if after := dockerIntegrationBaseline(t, ctx, runner, productionContainer, productionVolume, productionNetwork); after != baseline {
		t.Fatalf("verify 副本启动后生产项目发生变化:\n前=%s\n后=%s", baseline, after)
	}

	cleanupResult, err := CleanupIsolation(ctx, runner, plan.DestinationHost, id)
	if err != nil || cleanupResult.Status != "ok" {
		t.Fatalf("CleanupIsolation result=%#v err=%v", cleanupResult, err)
	}
	if after := dockerIntegrationBaseline(t, ctx, runner, productionContainer, productionVolume, productionNetwork); after != baseline {
		t.Fatalf("清理 verify 副本后生产项目发生变化:\n前=%s\n后=%s", baseline, after)
	}
	if _, err := os.Stat(isolationRoot); !os.IsNotExist(err) {
		t.Fatalf("verify 隔离根目录未清理: %v", err)
	}
}

func dockerIntegrationID(t *testing.T) string {
	t.Helper()
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		t.Fatalf("生成集成测试 isolation ID 失败: %v", err)
	}
	return hex.EncodeToString(buffer)
}

func dockerIntegrationFreePort(t *testing.T) int {
	t.Helper()
	for attempt := 0; attempt < 10; attempt++ {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("分配集成测试 TCP 端口失败: %v", err)
		}
		port := listener.Addr().(*net.TCPAddr).Port
		packet, packetErr := net.ListenPacket("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if closeErr := listener.Close(); closeErr != nil {
			t.Fatalf("释放集成测试 TCP 端口失败: %v", closeErr)
		}
		if packetErr != nil {
			continue
		}
		if closeErr := packet.Close(); closeErr != nil {
			t.Fatalf("释放集成测试 UDP 端口失败: %v", closeErr)
		}
		return port
	}
	t.Fatal("无法找到同时空闲的 TCP/UDP 测试端口")
	return 0
}

func waitDockerContainerRunning(ctx context.Context, runner sshexec.Runner, container string) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		out, err := runner.Run(ctx, "docker", "container", "inspect", "--format", "{{.State.Status}}", container)
		if err == nil && strings.TrimSpace(out) == "running" {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("等待容器 %q 运行失败: %w", container, ctx.Err())
		case <-ticker.C:
		}
	}
}

func dockerIntegrationBaseline(
	t *testing.T,
	ctx context.Context,
	runner sshexec.Runner,
	container string,
	volume string,
	network string,
) string {
	t.Helper()
	var values []string
	queries := [][]string{
		{"docker", "container", "inspect", "--format", "{{.Id}}|{{.State.Status}}|{{json .NetworkSettings.Ports}}", container},
		{"docker", "volume", "inspect", "--format", "{{.Name}}|{{json .Labels}}", volume},
		{"docker", "network", "inspect", "--format", "{{.Id}}|{{json .Labels}}", network},
	}
	for _, argv := range queries {
		out, err := runner.Run(ctx, argv...)
		if err != nil {
			t.Fatalf("读取 Docker 基线失败: %v", err)
		}
		values = append(values, strings.TrimSpace(out))
	}
	return strings.Join(values, "\n")
}
