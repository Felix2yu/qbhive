package proxy

import (
	"net"
	"strings"
)

// parseScutilProxy 解析 scutil --proxy 的 “Key : Value” 文本为映射。
// 纯函数、跨平台，便于单测。
func parseScutilProxy(out string) map[string]string {
	m := make(map[string]string)
	for _, line := range strings.Split(out, "\n") {
		i := strings.Index(line, ":")
		if i < 0 {
			continue
		}
		k := strings.TrimSpace(line[:i])
		v := strings.TrimSpace(line[i+1:])
		if k != "" {
			m[k] = v
		}
	}
	return m
}

// selectSystemProxy 按 macOS 系统代理映射挑出 Go transport 可用的代理 URL。
// 返回 (connect, socks)：
//   - connect：HTTP(S) CONNECT 代理，优先 Web Proxy(http)，其次 Secure Web Proxy(https)。
//     一个 http CONNECT 代理即可同时服务 http 与 https 目标，故 https 目标也走它。
//   - socks：SOCKS5 代理，仅当无 HTTP(S) 代理时由调用方兜底。
func selectSystemProxy(kv map[string]string) (connect, socks string) {
	if kv["HTTPEnable"] == "1" && kv["HTTPProxy"] != "" {
		connect = "http://" + joinHostPort(kv["HTTPProxy"], kv["HTTPPort"])
	} else if kv["HTTPSEnable"] == "1" && kv["HTTPSProxy"] != "" {
		connect = "https://" + joinHostPort(kv["HTTPSProxy"], kv["HTTPSPort"])
	}
	if kv["SOCKSEnable"] == "1" && kv["SOCKSProxy"] != "" {
		socks = "socks5://" + joinHostPort(kv["SOCKSProxy"], kv["SOCKSPort"])
	}
	return connect, socks
}

func joinHostPort(host, port string) string {
	if port == "" {
		return host
	}
	return net.JoinHostPort(host, port)
}
