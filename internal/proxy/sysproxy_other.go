//go:build !darwin

package proxy

// detectSystemProxy 在非 macOS 平台不读取系统代理（无统一 API）。
// 返回空串，调用方据此回退 HTTP_PROXY / HTTPS_PROXY 环境变量。
func detectSystemProxy() (connect, socks string) { return "", "" }
