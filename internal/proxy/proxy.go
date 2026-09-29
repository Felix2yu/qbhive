// Package proxy 为 qbhive 的全部出网请求统一装配代理。
//
// qB 客户端、RSS 抓取、AI 客户端以及 apprise-go 通知库都基于 http.DefaultTransport，
// 因此 Install 用克隆 + 覆盖 Proxy 后的 transport 替换全局 http.DefaultTransport，
// 一处生效、覆盖所有出网点。Proxy 函数每次请求实时读取配置，POST /api/config
// 保存后无需重启即生效。localhost / 回环地址（qB、Ollama 等）恒定直连。
package proxy

import (
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Felix2yu/qbhive/internal/config"
	"github.com/Felix2yu/qbhive/internal/models"
)

// systemRefreshInterval macOS 系统代理缓存刷新间隔：足够低开销，又能跟上
// Clash/Surge 开关变化。
const systemRefreshInterval = 60 * time.Second

// detectSystemProxyFn 是平台探测入口的变量别名，单测据此注入桩函数。
var detectSystemProxyFn = detectSystemProxy

// systemCache 缓存系统代理解析结果（connect=HTTP(S) CONNECT 代理，socks=SOCKS5 代理）。
type systemCache struct {
	mu      sync.Mutex
	at      time.Time
	connect string
	socks   string
}

func (c *systemCache) get() (connect, socks string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.at.IsZero() || time.Since(c.at) > systemRefreshInterval {
		conn, so := detectSystemProxyFn()
		c.connect, c.socks, c.at = conn, so, time.Now()
	}
	return c.connect, c.socks
}

// Install 替换全局 http.DefaultTransport，使所有默认 client 走本包代理路由。
// cfg 为 nil 时仅装配 localhost 绕过与系统代理/环境变量回退。
func Install(cfg *config.Manager) {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok || base == nil {
		return
	}
	tr := base.Clone()
	sc := &systemCache{}
	tr.Proxy = func(req *http.Request) (*url.URL, error) {
		return resolve(cfg, sc, req)
	}
	http.DefaultTransport = tr
}

// resolve 是 transport 的 Proxy 回调：按当前配置决定该请求用哪个代理。
func resolve(cfg *config.Manager, sc *systemCache, req *http.Request) (*url.URL, error) {
	// 回环/localhost 永不走代理（qB、Ollama 等本地服务）
	if host := req.URL.Hostname(); host == "" || isLoopback(host) {
		return nil, nil
	}

	var p models.ProxyConfig
	if cfg != nil {
		p = cfg.Get().Proxy
	}
	mode := strings.ToLower(strings.TrimSpace(p.Mode))
	manual := strings.TrimSpace(p.URL)

	switch mode {
	case "off":
		return nil, nil
	case "manual":
		if manual == "" {
			return nil, nil
		}
		return parseProxy(manual)
	default: // "" / "system"：跟随系统代理，失败回退环境变量
		// 兼容旧配置：填了 URL 但未选模式，视为手动
		if manual != "" {
			return parseProxy(manual)
		}
		if u := systemURL(sc); u != "" {
			return parseProxy(u)
		}
		return http.ProxyFromEnvironment(req)
	}
}

// systemURL 首选 CONNECT(HTTPS) 代理，其次 SOCKS5；都没有则空串。
func systemURL(sc *systemCache) string {
	connect, socks := sc.get()
	if connect != "" {
		return connect
	}
	return socks
}

func parseProxy(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, nil
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks5", "socks5h":
		return u, nil
	default:
		return nil, nil
	}
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// Snapshot 返回当前代理配置的一句话描述，供启动日志使用。
func Snapshot(cfg *config.Manager) string {
	var p models.ProxyConfig
	if cfg != nil {
		p = cfg.Get().Proxy
	}
	mode := strings.ToLower(strings.TrimSpace(p.Mode))
	manual := strings.TrimSpace(p.URL)
	switch mode {
	case "off":
		return "已关闭（全部直连）"
	case "manual":
		if manual == "" {
			return "手动模式但未填地址（全部直连）"
		}
		return "手动代理 " + manual
	default:
		if manual != "" {
			return "手动代理 " + manual + "（兼容：填地址未选模式）"
		}
		if u := systemURL(&systemCache{}); u != "" {
			return "跟随系统代理 " + u
		}
		return "跟随系统：未检测到系统代理，回退环境变量/直连"
	}
}
