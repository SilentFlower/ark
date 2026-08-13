package restore

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestCleanupIsolation_校验归属后按顺序删除(t *testing.T) {
	isolationID := strings.Repeat("a", 64)
	root := isolationBase + "/" + isolationID
	state := isolationState{
		SchemaVersion: isolationSchemaVersion,
		ID:            isolationID,
		Purpose:       IsolationPurposeRestore,
		Destination:   "destination-01",
		ProjectName:   "app-restore-aaaaaaaaaaaa",
		Root:          root,
		ComposeFile:   root + "/compose.generated.json",
		Services:      []string{"api"},
		Volumes:       []string{"app-data-restore-aaaaaaaaaaaa"},
		Networks:      []string{"app-net-restore-aaaaaaaaaaaa"},
	}
	stateJSON, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("编码状态失败: %v", err)
	}
	var destructive []string
	runner := &runnerFuncs{run: func(_ context.Context, argv ...string) (string, error) {
		joined := strings.Join(argv, " ")
		switch {
		case len(argv) == 3 && argv[0] == "test" && argv[1] == "-e":
			return "", nil
		case len(argv) == 4 && reflect.DeepEqual(argv[:3], []string{"test", "!", "-L"}):
			return "", nil
		case len(argv) == 4 && reflect.DeepEqual(argv[:3], []string{"readlink", "-f", "--"}):
			return argv[3] + "\n", nil
		case reflect.DeepEqual(argv, []string{"cat", "--", root + "/state.json"}):
			return string(stateJSON), nil
		case strings.HasPrefix(joined, "docker ps -a "):
			return "cid-api\n", nil
		case reflect.DeepEqual(argv, []string{"docker", "container", "inspect", "--format", "{{json .Config.Labels}}", "cid-api"}):
			return `{"com.docker.compose.project":"app-restore-aaaaaaaaaaaa","com.docker.compose.service":"api","io.ark.restore.isolation":"` + isolationID + `"}`, nil
		case strings.HasPrefix(joined, "docker network ls "):
			return "app-net-restore-aaaaaaaaaaaa\n", nil
		case reflect.DeepEqual(argv, []string{"docker", "network", "inspect", "--format", "{{json .Labels}}", "app-net-restore-aaaaaaaaaaaa"}):
			return `{"com.docker.compose.project":"app-restore-aaaaaaaaaaaa","io.ark.restore.isolation":"` + isolationID + `"}`, nil
		case strings.HasPrefix(joined, "docker volume ls "):
			return "app-data-restore-aaaaaaaaaaaa\n", nil
		case reflect.DeepEqual(argv, []string{"docker", "volume", "inspect", "--format", "{{json .Labels}}", "app-data-restore-aaaaaaaaaaaa"}):
			return `{"com.docker.compose.project":"app-restore-aaaaaaaaaaaa","io.ark.restore.isolation":"` + isolationID + `"}`, nil
		case argv[0] == "docker" && (argv[1] == "rm" || argv[2] == "rm"), argv[0] == "rm":
			destructive = append(destructive, joined)
			return "", nil
		default:
			t.Fatalf("未配置命令响应: %#v", argv)
			return "", nil
		}
	}}

	result, err := CleanupIsolation(context.Background(), runner, "destination-01", isolationID)
	if err != nil || result.Status != "ok" {
		t.Fatalf("cleanup result=%#v err=%v", result, err)
	}
	want := []string{
		"docker rm -f cid-api",
		"docker network rm app-net-restore-aaaaaaaaaaaa",
		"docker volume rm app-data-restore-aaaaaaaaaaaa",
		"rm -rf -- " + root,
	}
	if !reflect.DeepEqual(destructive, want) {
		t.Fatalf("删除顺序 = %#v，期望 %#v", destructive, want)
	}
}

func TestCleanupIsolation_资源标签漂移时零删除(t *testing.T) {
	isolationID := strings.Repeat("b", 64)
	root := isolationBase + "/" + isolationID
	state := isolationState{
		SchemaVersion: isolationSchemaVersion,
		ID:            isolationID,
		Purpose:       IsolationPurposeRestore,
		Destination:   "destination-01",
		ProjectName:   "app-restore-bbbbbbbbbbbb",
		Root:          root,
		ComposeFile:   root + "/compose.generated.json",
		Services:      []string{"api"},
	}
	stateJSON, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("编码状态失败: %v", err)
	}
	deleted := false
	runner := &runnerFuncs{run: func(_ context.Context, argv ...string) (string, error) {
		switch {
		case len(argv) == 3 && argv[0] == "test" && argv[1] == "-e":
			return "", nil
		case len(argv) == 4 && argv[0] == "test":
			return "", nil
		case len(argv) == 4 && argv[0] == "readlink":
			return argv[3] + "\n", nil
		case reflect.DeepEqual(argv, []string{"cat", "--", root + "/state.json"}):
			return string(stateJSON), nil
		case argv[0] == "docker" && argv[1] == "ps":
			return "cid-api\n", nil
		case argv[0] == "docker" && argv[1] == "container" && argv[2] == "inspect":
			return `{"com.docker.compose.project":"app-restore-bbbbbbbbbbbb","com.docker.compose.service":"api","io.ark.restore.isolation":"wrong"}`, nil
		case argv[0] == "docker" || argv[0] == "rm":
			deleted = true
			return "", nil
		default:
			t.Fatalf("未配置命令响应: %#v", argv)
			return "", nil
		}
	}}

	result, err := CleanupIsolation(context.Background(), runner, "destination-01", isolationID)
	if err == nil || result.Status != "fail" || deleted {
		t.Fatalf("cleanup result=%#v err=%v deleted=%v", result, err, deleted)
	}
}

func TestCleanupIsolation_目录不存在时幂等成功(t *testing.T) {
	isolationID := strings.Repeat("c", 64)
	runner := &runnerFuncs{run: func(_ context.Context, argv ...string) (string, error) {
		if len(argv) == 3 && argv[0] == "test" && (argv[1] == "-e" || argv[1] == "-L") {
			return "", commandExitError(t, 1)
		}
		if len(argv) > 2 && argv[0] == "docker" &&
			(argv[1] == "ps" || argv[1] == "network" || argv[1] == "volume") {
			return "", nil
		}
		return "", errors.New("不应执行其它命令")
	}}
	result, err := CleanupIsolation(context.Background(), runner, "destination-01", isolationID)
	if err != nil || result.Status != "ok" || len(result.Removed) != 0 {
		t.Fatalf("cleanup result=%#v err=%v", result, err)
	}
}

func TestCleanupIsolation_状态目录丢失但存在孤立资源时失败(t *testing.T) {
	isolationID := strings.Repeat("d", 64)
	runner := &runnerFuncs{run: func(_ context.Context, argv ...string) (string, error) {
		switch {
		case len(argv) == 3 && argv[0] == "test" && (argv[1] == "-e" || argv[1] == "-L"):
			return "", commandExitError(t, 1)
		case len(argv) > 2 && argv[0] == "docker" && argv[1] == "ps":
			return "container-id\n", nil
		case len(argv) > 2 && argv[0] == "docker" && (argv[1] == "network" || argv[1] == "volume"):
			return "", nil
		default:
			t.Fatalf("未配置命令响应: %#v", argv)
			return "", nil
		}
	}}
	result, err := CleanupIsolation(context.Background(), runner, "destination-01", isolationID)
	if err == nil || result.Status != "fail" || !strings.Contains(err.Error(), "container:container-id") {
		t.Fatalf("cleanup result=%#v err=%v", result, err)
	}
}

func TestValidIsolationID_只接受完整小写摘要(t *testing.T) {
	if !ValidIsolationID(strings.Repeat("a", 64)) {
		t.Fatal("合法 isolation ID 被拒绝")
	}
	for _, value := range []string{strings.Repeat("a", 63), strings.Repeat("A", 64), strings.Repeat("g", 64)} {
		if ValidIsolationID(value) {
			t.Fatalf("非法 isolation ID 被接受: %q", value)
		}
	}
}

func TestCleanupIsolation_状态记录Volume标签漂移时零删除(t *testing.T) {
	isolationID := strings.Repeat("d", 64)
	root := isolationBase + "/" + isolationID
	volumeName := "app-data-restore-dddddddddddd"
	state := isolationState{
		SchemaVersion: isolationSchemaVersion,
		ID:            isolationID,
		Purpose:       IsolationPurposeRestore,
		Destination:   "destination-01",
		ProjectName:   "app-restore-dddddddddddd",
		Root:          root,
		ComposeFile:   root + "/compose.generated.json",
		Volumes:       []string{volumeName},
	}
	stateJSON, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("编码状态失败: %v", err)
	}
	deleted := false
	runner := &runnerFuncs{run: func(_ context.Context, argv ...string) (string, error) {
		joined := strings.Join(argv, " ")
		switch {
		case len(argv) == 3 && argv[0] == "test" && argv[1] == "-e":
			return "", nil
		case len(argv) == 4 && argv[0] == "test":
			return "", nil
		case len(argv) == 4 && argv[0] == "readlink":
			return argv[3], nil
		case reflect.DeepEqual(argv, []string{"cat", "--", root + "/state.json"}):
			return string(stateJSON), nil
		case strings.HasPrefix(joined, "docker ps -a "):
			return "", nil
		case strings.Contains(joined, "docker network ls"):
			return "", nil
		case strings.Contains(joined, "docker volume ls --filter label="):
			return "", nil
		case strings.Contains(joined, "docker volume ls --filter name=^"):
			return volumeName + "\n", nil
		case reflect.DeepEqual(argv, []string{"docker", "volume", "inspect", "--format", "{{json .Labels}}", volumeName}):
			return `{"com.docker.compose.project":"wrong","io.ark.restore.isolation":"wrong"}`, nil
		case argv[0] == "docker" || argv[0] == "rm":
			deleted = true
			return "", nil
		default:
			t.Fatalf("未配置命令响应: %#v", argv)
			return "", nil
		}
	}}
	result, err := CleanupIsolation(context.Background(), runner, "destination-01", isolationID)
	if err == nil || result.Status != "fail" || deleted {
		t.Fatalf("cleanup result=%#v err=%v deleted=%v", result, err, deleted)
	}
}
