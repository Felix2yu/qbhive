//go:build darwin

package proxy

import (
	"context"
	"os/exec"
	"time"
)

// detectSystemProxy 读取 macOS「系统设置→网络→代理」并转成 Go transport 可用的代理 URL。
// 仅配置 PAC（ProxyAutoConfigEnable）而无显式代理时返回空串，调用方回退环境变量。
func detectSystemProxy() (connect, socks string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	bin, err := exec.LookPath("scutil")
	if err != nil {
		// launchd 精简 PATH 时兜底到绝对路径
		bin = "/usr/sbin/scutil"
	}
	out, err := exec.CommandContext(ctx, bin, "--proxy").Output()
	if err != nil {
		return "", ""
	}
	return selectSystemProxy(parseScutilProxy(string(out)))
}
