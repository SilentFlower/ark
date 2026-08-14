// Command ark-hub 是运行在 hub 本机上的常驻管理服务。
//
// 入口只负责把命令执行结果转换为进程退出码；HTTP、鉴权和 systemd 安装逻辑
// 全部位于 internal/hub，避免 cmd 层持有业务状态。
package main

import (
	"os"

	"github.com/silentflower/ark/internal/hub"
)

func main() {
	os.Exit(hub.Execute())
}
