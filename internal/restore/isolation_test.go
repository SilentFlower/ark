package restore

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

func testIsolationSpec(t *testing.T) *IsolationSpec {
	t.Helper()
	cfg, manifest := testRestoreInputs()
	plan, err := BuildPlan(cfg, manifest, "manifest-snapshot", "source-01", "destination-01")
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	isolated, err := WithIsolation(plan)
	if err != nil {
		t.Fatalf("WithIsolation 失败: %v", err)
	}
	return isolated.Isolation
}

func TestTransformIsolationCompose_结构化隔离资源路径和端口(t *testing.T) {
	spec := testIsolationSpec(t)
	canonical := `{
		"name":"app-prod",
		"services":{"api":{
			"container_name":"app-api",
			"labels":{"existing":"value"},
			"env_file":[{"path":"/srv/app/.env","required":true}],
			"volumes":[
				{"type":"volume","source":"uploads","target":"/data"},
				{"type":"bind","source":"/srv/app/.env","target":"/run/app.env"}
			],
			"ports":[
				{"target":8080,"published":"8080","host_ip":"127.0.0.1","protocol":"tcp","app_protocol":"http","mode":"ingress"},
				{"target":8080,"published":"8080","host_ip":"127.0.0.1","protocol":"udp"},
				{"target":8081,"published":"8081","protocol":"tcp"}
			]
		}},
		"volumes":{"uploads":{"name":"uploads"}},
		"networks":{"default":{"name":"app-prod_default"}},
		"configs":{"app":{"file":"/srv/app/.env"}}
	}`

	generated, services, volumes, networks, ports, hostIPs, err := transformIsolationCompose([]byte(canonical), spec)
	if err != nil {
		t.Fatalf("transformIsolationCompose 失败: %v", err)
	}
	if len(services) != 1 || services[0] != "api" || len(volumes) != 1 || len(networks) != 1 {
		t.Fatalf("资源清单错误: services=%#v volumes=%#v networks=%#v", services, volumes, networks)
	}
	var httpPort *IsolationPort
	for index := range ports {
		if ports[index].HostIP == "127.0.0.1" && ports[index].Target == 8080 && ports[index].Protocol == "tcp" {
			httpPort = &ports[index]
			break
		}
	}
	if len(ports) != 3 || httpPort == nil || httpPort.OriginalPublished != "8080" ||
		httpPort.AllocatedPort != "auto" || httpPort.AppProtocol != "http" {
		t.Fatalf("端口元数据错误: %#v", ports)
	}
	if len(hostIPs) != 1 || hostIPs[0] != "127.0.0.1" {
		t.Fatalf("具体 host IP 清单错误: %#v", hostIPs)
	}

	var document map[string]any
	if err := json.Unmarshal(generated, &document); err != nil {
		t.Fatalf("生成 JSON 无效: %v", err)
	}
	if document["name"] != spec.ProjectName {
		t.Fatalf("project name = %#v", document["name"])
	}
	api := document["services"].(map[string]any)["api"].(map[string]any)
	if _, exists := api["container_name"]; exists {
		t.Fatal("隔离配置不应保留 container_name")
	}
	labels := api["labels"].(map[string]any)
	if labels[isolationLabel] != spec.ID || labels["existing"] != "value" {
		t.Fatalf("service labels = %#v", labels)
	}
	mounts := api["volumes"].([]any)
	if mounts[1].(map[string]any)["source"] != spec.SourceEnvFile {
		t.Fatalf("bind source = %#v", mounts[1])
	}
	envFiles := api["env_file"].([]any)
	if envFiles[0].(map[string]any)["path"] != spec.SourceEnvFile {
		t.Fatalf("service env_file 未隔离: %#v", envFiles)
	}
	for _, rawPort := range api["ports"].([]any) {
		port := rawPort.(map[string]any)
		if _, exists := port["published"]; exists {
			t.Fatalf("生成端口仍包含固定 published: %#v", port)
		}
	}
	volume := document["volumes"].(map[string]any)["uploads"].(map[string]any)
	if volume["name"] != volumes[0] || volume["labels"].(map[string]any)[isolationLabel] != spec.ID {
		t.Fatalf("volume 未隔离: %#v", volume)
	}
	config := document["configs"].(map[string]any)["app"].(map[string]any)
	if config["file"] != spec.SourceEnvFile {
		t.Fatalf("config path = %#v", config)
	}
}

func TestTransformIsolationCompose_ExternalNetwork转换为私有Bridge(t *testing.T) {
	spec := testIsolationSpec(t)
	tests := []struct {
		name       string
		network    string
		sourceName string
	}{
		{
			name:       "布尔声明和显式名称",
			network:    `{"name":"api_shared","external":true}`,
			sourceName: "api_shared",
		},
		{
			name:       "Compose canonical 空默认字段",
			network:    `{"name":"api_shared","ipam":{},"external":true}`,
			sourceName: "api_shared",
		},
		{
			name:       "对象声明中的名称",
			network:    `{"external":{"name":"api_shared"}}`,
			sourceName: "api_shared",
		},
		{
			name:       "缺少名称时按项目回退",
			network:    `{"external":true}`,
			sourceName: effectiveProjectResourceName(spec.SourceProject, "shared"),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			canonical := `{"services":{"api":{"networks":{"shared":{"aliases":["api.internal"],"priority":100}}}},` +
				`"networks":{"shared":` + tc.network + `}}`
			generated, _, _, networks, _, _, err := transformIsolationCompose([]byte(canonical), spec)
			if err != nil {
				t.Fatalf("transformIsolationCompose 失败: %v", err)
			}
			wantName := isolationResourceName(tc.sourceName, spec.Purpose, spec.ShortID)
			if !reflect.DeepEqual(networks, []string{wantName}) {
				t.Fatalf("network 清单=%#v，期望 %#v", networks, []string{wantName})
			}

			var document map[string]any
			if err := json.Unmarshal(generated, &document); err != nil {
				t.Fatalf("生成 JSON 无效: %v", err)
			}
			network := document["networks"].(map[string]any)["shared"].(map[string]any)
			if network["name"] != wantName || network["driver"] != "bridge" {
				t.Fatalf("external network 未转换为私有 bridge: %#v", network)
			}
			if _, exists := network["external"]; exists {
				t.Fatalf("隔离 network 仍含 external: %#v", network)
			}
			if network["labels"].(map[string]any)[isolationLabel] != spec.ID {
				t.Fatalf("隔离 network 缺少归属标签: %#v", network)
			}
			serviceNetwork := document["services"].(map[string]any)["api"].(map[string]any)["networks"].(map[string]any)["shared"].(map[string]any)
			if !reflect.DeepEqual(serviceNetwork["aliases"], []any{"api.internal"}) || serviceNetwork["priority"] != float64(100) {
				t.Fatalf("service network attachment 发生变化: %#v", serviceNetwork)
			}
		})
	}
}

func TestTransformIsolationCompose_普通NamedVolume允许默认LocalDriver(t *testing.T) {
	spec := testIsolationSpec(t)
	tests := []struct {
		name       string
		volume     string
		wantDriver string
	}{
		{
			name:   "缺省 driver",
			volume: `{"name":"app_data"}`,
		},
		{
			name:       "Compose canonical 默认 local driver",
			volume:     `{"name":"app_data","driver":"local"}`,
			wantDriver: "local",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			canonical := `{"services":{"api":{"volumes":[{"type":"volume","source":"data","target":"/data"}]}},` +
				`"volumes":{"data":` + tc.volume + `}}`
			generated, _, volumes, _, _, _, err := transformIsolationCompose([]byte(canonical), spec)
			if err != nil {
				t.Fatalf("transformIsolationCompose 失败: %v", err)
			}
			wantName := isolationResourceName("app_data", spec.Purpose, spec.ShortID)
			if !reflect.DeepEqual(volumes, []string{wantName}) {
				t.Fatalf("volume 清单=%#v，期望 %#v", volumes, []string{wantName})
			}

			var document map[string]any
			if err := json.Unmarshal(generated, &document); err != nil {
				t.Fatalf("生成 JSON 无效: %v", err)
			}
			volume := document["volumes"].(map[string]any)["data"].(map[string]any)
			if volume["name"] != wantName || volume["labels"].(map[string]any)[isolationLabel] != spec.ID {
				t.Fatalf("named volume 未隔离: %#v", volume)
			}
			if strings.TrimSpace(stringValue(volume["driver"])) != tc.wantDriver {
				t.Fatalf("volume driver=%#v，期望 %q", volume["driver"], tc.wantDriver)
			}
		})
	}
}

func TestTransformIsolationCompose_拒绝无法证明隔离的配置(t *testing.T) {
	spec := testIsolationSpec(t)
	tests := []struct {
		name      string
		canonical string
		want      string
	}{
		{
			name:      "external volume",
			canonical: `{"services":{"api":{}},"volumes":{"data":{"external":true}}}`,
			want:      "external",
		},
		{
			name:      "external config",
			canonical: `{"services":{"api":{}},"configs":{"app":{"external":true}}}`,
			want:      "external",
		},
		{
			name:      "external secret",
			canonical: `{"services":{"api":{}},"secrets":{"token":{"external":true}}}`,
			want:      "external",
		},
		{
			name:      "external network 额外运行时参数",
			canonical: `{"services":{"api":{}},"networks":{"shared":{"name":"api_shared","external":true,"ipam":{"driver":"default"}}}}`,
			want:      "运行时参数",
		},
		{
			name:      "host network",
			canonical: `{"services":{"api":{"network_mode":"host"}}}`,
			want:      "无法隔离",
		},
		{
			name:      "macvlan network",
			canonical: `{"services":{"api":{}},"networks":{"prod":{"driver":"macvlan"}}}`,
			want:      "macvlan",
		},
		{
			name:      "network driver options",
			canonical: `{"services":{"api":{}},"networks":{"prod":{"driver":"bridge","driver_opts":{"parent":"eth0"}}}}`,
			want:      "driver_opts",
		},
		{
			name:      "未备份 bind",
			canonical: `{"services":{"api":{"volumes":[{"type":"bind","source":"/etc/passwd","target":"/data"}]}}}`,
			want:      "未包含",
		},
		{
			name:      "匿名 volume",
			canonical: `{"services":{"api":{"volumes":[{"type":"volume","target":"/data"}]}}}`,
			want:      "匿名 volume",
		},
		{
			name:      "privileged",
			canonical: `{"services":{"api":{"privileged":true}}}`,
			want:      "privileged",
		},
		{
			name:      "host user namespace",
			canonical: `{"services":{"api":{"userns_mode":"host"}}}`,
			want:      "无法隔离",
		},
		{
			name:      "volume driver options",
			canonical: `{"services":{"api":{}},"volumes":{"data":{"driver":"local","driver_opts":{"type":"none"}}}}`,
			want:      "driver/driver_opts",
		},
		{
			name:      "非 local volume driver",
			canonical: `{"services":{"api":{}},"volumes":{"data":{"driver":"nfs"}}}`,
			want:      "driver/driver_opts",
		},
		{
			name:      "Docker API socket",
			canonical: `{"services":{"api":{"use_api_socket":true}}}`,
			want:      "use_api_socket",
		},
		{
			name:      "重复端口",
			canonical: `{"services":{"api":{"ports":[{"target":8080,"protocol":"tcp"},{"target":8080,"protocol":"tcp"}]}}}`,
			want:      "重复",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, _, _, _, err := transformIsolationCompose([]byte(tc.canonical), spec)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误 = %v，期望包含 %q", err, tc.want)
			}
		})
	}
}

func TestTransformIsolationCompose_策略拒绝提供受控结果摘要(t *testing.T) {
	spec := testIsolationSpec(t)
	tests := []struct {
		name        string
		canonical   string
		wantSummary string
	}{
		{
			name:        "external volume 使用具体摘要",
			canonical:   `{"services":{"api":{}},"volumes":{"data":{"external":true}}}`,
			wantSummary: "隔离 Compose external volume 无法隔离",
		},
		{
			name:        "external network 参数使用具体摘要",
			canonical:   `{"services":{"api":{}},"networks":{"shared":{"external":true,"ipam":{"driver":"default"}}}}`,
			wantSummary: "隔离 Compose external network 包含不支持的运行时参数",
		},
		{
			name:        "非 local volume driver 使用具体摘要",
			canonical:   `{"services":{"api":{}},"volumes":{"data":{"driver":"nfs"}}}`,
			wantSummary: "隔离 Compose volume driver/driver_opts 无法隔离",
		},
		{
			name:        "其它转换拒绝使用通用阶段摘要",
			canonical:   `{"services":{"api":{"network_mode":"host"}}}`,
			wantSummary: "隔离 Compose 配置不符合安全策略",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, _, _, _, transformErr := transformIsolationCompose([]byte(tc.canonical), spec)
			if transformErr == nil {
				t.Fatal("期望 Compose 转换失败")
			}
			result, err := failResult(Result{}, withResultSummary(transformErr, "隔离 Compose 配置不符合安全策略"))
			if err == nil || result.Error != tc.wantSummary {
				t.Fatalf("result=%#v err=%v", result, err)
			}
		})
	}
}

func TestTransformIsolationCompose_接受Compose已解析到FilesRoot的相对路径(t *testing.T) {
	spec := testIsolationSpec(t)
	resolved := spec.SourceEnvFile
	canonical := `{"services":{"api":{"volumes":[{"type":"bind","source":"` + resolved + `","target":"/data/config.txt"}]}},` +
		`"configs":{"app":{"file":"` + resolved + `"}}}`
	generated, _, _, _, _, _, err := transformIsolationCompose([]byte(canonical), spec)
	if err != nil {
		t.Fatalf("已位于 files root 的相对路径解析结果被拒绝: %v", err)
	}
	if strings.Count(string(generated), resolved) != 2 {
		t.Fatalf("路径不应再次映射: %s", generated)
	}
}

func TestWithIsolationOptions_Verify按InstanceKey稳定派生且复用转换(t *testing.T) {
	cfg, manifest := testRestoreInputs()
	plan, err := BuildPlan(cfg, manifest, "manifest-snapshot", "source-01", "destination-01")
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	first, err := WithIsolationOptions(plan, IsolationOptions{
		Purpose: IsolationPurposeVerify, InstanceKey: "verification-2026w33", PortAllocation: IsolationPortDisabled,
	})
	if err != nil {
		t.Fatalf("verify isolation 失败: %v", err)
	}
	again, err := WithIsolationOptions(plan, IsolationOptions{
		Purpose: IsolationPurposeVerify, InstanceKey: "verification-2026w33", PortAllocation: IsolationPortDisabled,
	})
	if err != nil {
		t.Fatalf("再次 verify isolation 失败: %v", err)
	}
	other, err := WithIsolationOptions(plan, IsolationOptions{
		Purpose: IsolationPurposeVerify, InstanceKey: "verification-2026w34", PortAllocation: IsolationPortDisabled,
	})
	if err != nil {
		t.Fatalf("另一 verify isolation 失败: %v", err)
	}
	if first.Isolation.ID != again.Isolation.ID || first.Isolation.ID == other.Isolation.ID ||
		!strings.Contains(first.Project.ProjectName, "-verify-") {
		t.Fatalf("verify identity 不稳定或未隔离: first=%#v other=%#v", first.Isolation, other.Isolation)
	}
	if first.Isolation.PortAllocation != IsolationPortDisabled || len(first.Isolation.Ports) == 0 {
		t.Fatalf("verify 端口策略未写入 Plan: %#v", first.Isolation)
	}
	for _, port := range first.Isolation.Ports {
		if port.AllocatedPort != IsolationPortDisabled {
			t.Fatalf("verify 端口未禁用: %#v", first.Isolation.Ports)
		}
	}
	if err := validateIsolationPlan(first); err != nil {
		t.Fatalf("verify isolation Plan 无法由统一 Executor 接受: %v", err)
	}
}

func TestTransformIsolationCompose_Verify删除全部PublishedPorts(t *testing.T) {
	spec := testIsolationSpec(t)
	spec.Purpose = IsolationPurposeVerify
	spec.PortAllocation = IsolationPortDisabled
	canonical := `{"services":{"api":{"ports":[{"target":8080,"published":"8080","host_ip":"127.0.0.1","protocol":"tcp"}]}}}`
	generated, _, _, _, ports, hostIPs, err := transformIsolationCompose([]byte(canonical), spec)
	if err != nil {
		t.Fatalf("转换 verify Compose 失败: %v", err)
	}
	if len(ports) != 1 || ports[0].AllocatedPort != IsolationPortDisabled || len(hostIPs) != 0 {
		t.Fatalf("verify 端口摘要错误: ports=%#v hostIPs=%#v", ports, hostIPs)
	}
	var document map[string]any
	if err := json.Unmarshal(generated, &document); err != nil {
		t.Fatalf("解析 verify Compose 失败: %v", err)
	}
	api := document["services"].(map[string]any)["api"].(map[string]any)
	if _, exists := api["ports"]; exists {
		t.Fatalf("verify Compose 仍发布宿主机端口: %#v", api)
	}
}

func TestRewriteComposePreReadPaths_绝对路径改到隔离恢复树(t *testing.T) {
	spec := testIsolationSpec(t)
	// 用父目录 files target 模拟包含 include/extends/label 文件的常见项目快照。
	spec.PathMappings = []IsolationPathMapping{{Source: "/srv/app", Destination: spec.FilesRoot + "/srv/app"}}
	document := map[string]any{
		"include": []any{
			"/srv/app/include.yaml",
			map[string]any{
				"path":              []any{"parts/api.yaml"},
				"env_file":          "/srv/app/include.env",
				"project_directory": "/srv/app",
			},
		},
		"services": map[string]any{
			"api": map[string]any{
				"extends":    map[string]any{"file": "/srv/app/base.yaml", "service": "base"},
				"label_file": []any{"/srv/app/labels.env"},
			},
		},
	}
	references, composeFiles, changed, err := rewriteComposePreReadPaths(
		document, spec, spec.FilesRoot+"/srv/app",
	)
	if err != nil || !changed {
		t.Fatalf("rewrite failed: changed=%v err=%v", changed, err)
	}
	for _, reference := range references {
		if !strings.HasPrefix(reference, spec.FilesRoot+"/srv/app/") && reference != spec.FilesRoot+"/srv/app" {
			t.Fatalf("引用未隔离: %#v", references)
		}
	}
	baseDirectory := spec.FilesRoot + "/srv/app"
	wantComposeFiles := []composeSourceReference{
		{Path: baseDirectory + "/base.yaml", BaseDirectory: baseDirectory},
		{Path: baseDirectory + "/include.yaml", BaseDirectory: baseDirectory},
		{Path: baseDirectory + "/parts/api.yaml", BaseDirectory: baseDirectory},
	}
	if !reflect.DeepEqual(composeFiles, wantComposeFiles) {
		t.Fatalf("递归 Compose 文件 = %#v，期望 %#v", composeFiles, wantComposeFiles)
	}
	text, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("编码改写结果失败: %v", err)
	}
	if strings.Contains(string(text), `"/srv/app/`) || !strings.Contains(string(text), spec.FilesRoot) {
		t.Fatalf("前置读取路径未完全改写: %s", text)
	}
}

func TestPrepareIsolation_校验路径后写入生成配置和状态(t *testing.T) {
	cfg, manifest := testRestoreInputs()
	plan, err := BuildPlan(cfg, manifest, "manifest-snapshot", "source-01", "destination-01")
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	plan, err = WithIsolation(plan)
	if err != nil {
		t.Fatalf("WithIsolation 失败: %v", err)
	}
	executionID := executionIdentity(plan, nil)
	canonical := `{"services":{"api":{"image":"busybox","ports":[{"target":8080,"published":"8080","protocol":"tcp","app_protocol":"http","mode":"ingress"},{"target":5353,"published":"5353","host_ip":"127.0.0.1","protocol":"udp"}],` +
		`"volumes":[{"type":"bind","source":"/srv/app/.env","target":"/run/app.env"}]}},` +
		`"volumes":{"uploads":{"name":"uploads"}},"networks":{"default":{"name":"app-prod_default"}}}`
	writes := make(map[string]string)
	var commands [][]string
	runner := &runnerFuncs{
		run: func(_ context.Context, argv ...string) (string, error) {
			commands = append(commands, append([]string(nil), argv...))
			switch {
			case len(argv) == 3 && argv[0] == "test" && argv[1] == "-e":
				if argv[2] == isolationStatePath(plan) {
					return "", commandExitError(t, 1)
				}
				return "", nil
			case len(argv) == 3 && argv[0] == "test" && argv[1] == "-L":
				return "", commandExitError(t, 1)
			case len(argv) == 4 && argv[0] == "test" && argv[1] == "!" && argv[2] == "-L":
				return "", nil
			case len(argv) == 4 && argv[0] == "readlink" && argv[1] == "-f":
				return argv[3] + "\n", nil
			case reflect.DeepEqual(argv, []string{"find", plan.Isolation.FilesRoot, "-type", "l", "-print"}):
				return "", nil
			case reflect.DeepEqual(argv, []string{"cat", "--", plan.Isolation.SourceComposeFile}):
				return "services:\n  api:\n    image: busybox\n", nil
			case reflect.DeepEqual(argv, []string{"ip", "-j", "address"}):
				return `[{"addr_info":[{"local":"127.0.0.1"}]}]`, nil
			case reflect.DeepEqual(argv, append(composeBaseArgv(plan.Project), "config", "--format", "json")):
				return `{"services":{"api":{}}}`, nil
			case len(argv) > 0 && (argv[0] == "install" || argv[0] == "chmod" || argv[0] == "rm" || argv[0] == "mv"):
				return "", nil
			default:
				t.Fatalf("未配置命令响应: %#v", argv)
				return "", nil
			}
		},
		stream: func(_ context.Context, argv ...string) (io.ReadCloser, func() error, error) {
			want := append(composeBaseArgv(Project{
				Name:        plan.Isolation.SourceProject.Name,
				ComposeFile: plan.Isolation.SourceComposeFile,
				EnvFile:     plan.Isolation.SourceEnvFile,
				ProjectName: plan.Isolation.SourceProject.ProjectName,
			}), "config", "--format", "json", "--no-env-resolution")
			if !reflect.DeepEqual(argv, want) {
				t.Fatalf("未配置 Stream: %#v", argv)
			}
			return io.NopCloser(strings.NewReader(canonical)), func() error { return nil }, nil
		},
		feed: func(_ context.Context, input io.Reader, argv ...string) error {
			content, err := io.ReadAll(input)
			if err != nil {
				return err
			}
			if len(argv) != 2 || argv[0] != "tee" {
				t.Fatalf("未配置 Feed: %#v", argv)
			}
			writes[argv[1]] = string(content)
			return nil
		},
	}

	state, err := prepareIsolation(context.Background(), plan, runner, executionID)
	if err != nil {
		t.Fatalf("prepareIsolation 失败: %v", err)
	}
	if len(state.Services) != 1 || state.Services[0] != "api" || len(state.Ports) != 2 ||
		state.Ports[0].AllocatedPort != "auto" {
		t.Fatalf("state = %#v", state)
	}
	generated := writes[plan.Isolation.GeneratedComposeFile+".tmp"]
	if generated == "" || strings.Contains(generated, `"published"`) || !strings.Contains(generated, plan.Isolation.ID) {
		t.Fatalf("生成 Compose 内容错误: %s", generated)
	}
	stateContent := writes[isolationStatePath(plan)+".tmp"]
	if !strings.Contains(stateContent, executionID) || !strings.Contains(stateContent, `"purpose":"restore"`) {
		t.Fatalf("状态内容错误: %s", stateContent)
	}
	if len(commands) == 0 {
		t.Fatal("未执行目标机检查")
	}
}

func TestPrepareIsolation_Compose失败不泄漏插值内容(t *testing.T) {
	spec := testIsolationSpec(t)
	plan := Plan{Isolation: spec}
	runner := &runnerFuncs{run: func(_ context.Context, argv ...string) (string, error) {
		switch {
		case len(argv) == 3 && argv[0] == "test" && argv[1] == "-e":
			if argv[2] == isolationStatePath(plan) {
				return "", commandExitError(t, 1)
			}
			return "", nil
		case len(argv) == 3 && argv[0] == "test" && argv[1] == "-L":
			return "", commandExitError(t, 1)
		case len(argv) == 4 && argv[0] == "test":
			return "", nil
		case len(argv) == 4 && argv[0] == "readlink":
			return argv[3], nil
		case len(argv) > 0 && argv[0] == "find":
			return "", nil
		case reflect.DeepEqual(argv, []string{"cat", "--", spec.SourceComposeFile}):
			return "services:\n  api:\n    image: busybox\n", nil
		default:
			t.Fatalf("未配置命令响应: %#v", argv)
			return "", nil
		}
	}, stream: func(context.Context, ...string) (io.ReadCloser, func() error, error) {
		return nil, nil, errors.New("SECRET_TOKEN=do-not-print")
	}}
	_, err := prepareIsolation(context.Background(), plan, runner, "execution")
	if err == nil || strings.Contains(err.Error(), "SECRET_TOKEN") {
		t.Fatalf("错误未脱敏: %v", err)
	}
}

func TestValidateIsolationHostIPs_具体地址必须存在(t *testing.T) {
	runner := &runnerFuncs{run: func(_ context.Context, argv ...string) (string, error) {
		if strings.Join(argv, " ") != "ip -j address" {
			t.Fatalf("未预期命令: %#v", argv)
		}
		return `[{"addr_info":[{"local":"10.0.0.8"},{"local":"2001:db8::8"}]}]`, nil
	}}
	if err := validateIsolationHostIPs(context.Background(), runner, []string{"10.0.0.8", "2001:db8::8"}); err != nil {
		t.Fatalf("已有地址校验失败: %v", err)
	}
	if err := validateIsolationHostIPs(context.Background(), runner, []string{"10.0.0.9"}); err == nil ||
		!strings.Contains(err.Error(), "不存在") {
		t.Fatalf("缺失地址错误 = %v", err)
	}
}

func TestInspectIsolationPorts_校验容器归属并持久化实际端口(t *testing.T) {
	cfg, manifest := testRestoreInputs()
	plan, err := BuildPlan(cfg, manifest, "manifest-snapshot", "source-01", "destination-01")
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	plan, err = WithIsolation(plan)
	if err != nil {
		t.Fatalf("WithIsolation 失败: %v", err)
	}
	state := newIsolationState(plan, executionIdentity(plan, nil))
	state.Services = []string{"api"}
	state.Ports = []IsolationPort{
		{Service: "api", OriginalPublished: "8080", AllocatedPort: "auto", Target: 8080, Protocol: "tcp"},
		{Service: "api", HostIP: "127.0.0.1", OriginalPublished: "5353", AllocatedPort: "auto", Target: 5353, Protocol: "udp"},
	}
	writes := make(map[string]string)
	runner := &runnerFuncs{
		run: func(_ context.Context, argv ...string) (string, error) {
			switch {
			case reflect.DeepEqual(argv, append(composeArgv(plan), "ps", "--all", "--format", "json")):
				return `[{"ID":"container-full-id","Service":"api","State":"running","Health":"healthy"}]`, nil
			case reflect.DeepEqual(argv, []string{"docker", "container", "inspect", "--format", "{{json .Config.Labels}}", "container-full-id"}):
				return `{"com.docker.compose.project":"` + plan.Isolation.ProjectName + `",` +
					`"com.docker.compose.service":"api","io.ark.restore.isolation":"` + plan.Isolation.ID + `"}`, nil
			case reflect.DeepEqual(argv, []string{"docker", "container", "inspect", "--format", "{{json .NetworkSettings.Ports}}", "container-full-id"}):
				return `{"8080/tcp":[{"HostIp":"0.0.0.0","HostPort":"32768"},{"HostIp":"::","HostPort":"32768"}],` +
					`"5353/udp":[{"HostIp":"127.0.0.1","HostPort":"32769"}]}`, nil
			case len(argv) > 0 && (argv[0] == "install" || argv[0] == "chmod" || argv[0] == "rm" || argv[0] == "mv"):
				return "", nil
			default:
				t.Fatalf("未配置命令响应: %#v", argv)
				return "", nil
			}
		},
		feed: func(_ context.Context, input io.Reader, argv ...string) error {
			content, err := io.ReadAll(input)
			if err != nil {
				return err
			}
			writes[argv[1]] = string(content)
			return nil
		},
	}
	updated, err := inspectIsolationPorts(context.Background(), plan, runner, state)
	if err != nil {
		t.Fatalf("inspectIsolationPorts 失败: %v", err)
	}
	if len(updated.Containers) != 1 || updated.Containers[0].ID != "container-full-id" ||
		updated.Ports[0].AllocatedPort != "32768" || updated.Ports[0].HostIP != "" ||
		updated.Ports[1].AllocatedPort != "32769" || updated.Ports[1].HostIP != "127.0.0.1" {
		t.Fatalf("端口或容器状态错误: %#v", updated)
	}
	stateContent := writes[isolationStatePath(plan)+".tmp"]
	if strings.Contains(stateContent, `"allocated_port":"auto"`) ||
		!strings.Contains(stateContent, `"allocated_port":"32768"`) {
		t.Fatalf("实际端口未持久化: %s", stateContent)
	}
}
