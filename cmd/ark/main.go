// Command ark 是部署在每台机器上的备份 agent。
//
// 它被设计成 oneshot 进程：由 systemd timer 触发，跑完即退出。
// 不常驻的好处是没有「守护进程自己悄悄挂了、三个月没备份」这种故障模式。
package main

import (
	"os"

	"github.com/silentflower/ark/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
