package proxy

import (
	"net/http"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/Felix2yu/qbhive/internal/config"
	"github.com/Felix2yu/qbhive/internal/models"
)

func reqURL(t *testing.T, raw string) *http.Request {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("bad url %q: %v", raw, err)
	}
	return &http.Request{URL: u, Method: "GET"}
}

func newCfg(t *testing.T, p models.ProxyConfig) *config.Manager {
	t.Helper()
	cfg := config.New(filepath.Join(t.TempDir(), "config.json"))
	cfg.Set(models.AppConfig{Proxy: p})
	return cfg
}

func TestParseScutilProxy(t *testing.T) {
	out := `<dictionary> {
  HTTPEnable : 1
  HTTPPort : 6152
  HTTPProxy : 127.0.0.1
  HTTPSEnable : 1
  HTTPSPort : 6152
  HTTPSProxy : 127.0.0.1
  SOCKSEnable : 1
  SOCKSPort : 6152
  SOCKSProxy : 127.0.0.1
}`
	kv := parseScutilProxy(out)
	if kv["HTTPProxy"] != "127.0.0.1" || kv["HTTPPort"] != "6152" || kv["HTTPEnable"] != "1" {
		t.Fatalf("解析错误: %+v", kv)
	}
}

func TestSelectSystemProxy(t *testing.T) {
	cases := []struct {
		name        string
		kv          map[string]string
		wantConnect string
		wantSocks   string
	}{
		{"HTTP+SOCKS 混合端口（Clash/Surge）",
			map[string]string{"HTTPEnable": "1", "HTTPProxy": "127.0.0.1", "HTTPPort": "6152", "SOCKSEnable": "1", "SOCKSProxy": "127.0.0.1", "SOCKSPort": "6152"},
			"http://127.0.0.1:6152", "socks5://127.0.0.1:6152"},
		{"仅 HTTPS 安全代理",
			map[string]string{"HTTPSEnable": "1", "HTTPSProxy": "proxy.example.com", "HTTPSPort": "8443"},
			"https://proxy.example.com:8443", ""},
		{"仅 SOCKS5",
			map[string]string{"SOCKSEnable": "1", "SOCKSProxy": "127.0.0.1", "SOCKSPort": "7890"},
			"", "socks5://127.0.0.1:7890"},
		{"HTTP 关闭则不选",
			map[string]string{"HTTPEnable": "0", "HTTPProxy": "127.0.0.1", "HTTPPort": "6152"},
			"", ""},
		{"PAC -only（无显式代理）",
			map[string]string{"ProxyAutoConfigEnable": "1", "ProxyAutoConfigURLString": "http://x/pac"},
			"", ""},
		{"HTTP 无端口（少见）",
			map[string]string{"HTTPEnable": "1", "HTTPProxy": "10.0.0.1"},
			"http://10.0.0.1", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			conn, socks := selectSystemProxy(c.kv)
			if conn != c.wantConnect || socks != c.wantSocks {
				t.Errorf("selectSystemProxy = (%q,%q), want (%q,%q)", conn, socks, c.wantConnect, c.wantSocks)
			}
		})
	}
}

// HTTP CONNECT 代理优先级高于 SOCKS
func TestSystemURLPrefersConnect(t *testing.T) {
	defer swapDetect(t, func() (string, string) { return "http://127.0.0.1:6152", "socks5://127.0.0.1:6152" })()
	sc := &systemCache{}
	if got := systemURL(sc); got != "http://127.0.0.1:6152" {
		t.Errorf("systemURL = %q, want http connect", got)
	}
}

func TestResolve(t *testing.T) {
	cases := []struct {
		name     string
		proxy    models.ProxyConfig
		detect   func() (string, string)
		target   string
		wantNil  bool
		wantHost string
	}{
		{"回环直连（qB）", models.ProxyConfig{Mode: "manual", URL: "http://1.2.3.4:8"}, nil, "http://127.0.0.1:8989/api", true, ""},
		{"localhost 直连", models.ProxyConfig{Mode: "manual", URL: "http://1.2.3.4:8"}, nil, "http://localhost:11434/v1", true, ""},
		{"::1 直连", models.ProxyConfig{Mode: "manual", URL: "http://1.2.3.4:8"}, nil, "http://[::1]:8080", true, ""},
		{"off 忽略系统代理", models.ProxyConfig{Mode: "off"}, func() (string, string) { return "http://1.2.3.4:8", "" }, "https://telegram.org", true, ""},
		{"manual http", models.ProxyConfig{Mode: "manual", URL: "http://1.2.3.4:8080"}, nil, "https://api.telegram.org/x", false, "1.2.3.4:8080"},
		{"manual socks5", models.ProxyConfig{Mode: "manual", URL: "socks5://1.2.3.4:7890"}, nil, "https://example.com", false, "1.2.3.4:7890"},
		{"manual 非法 scheme → 直连", models.ProxyConfig{Mode: "manual", URL: "ftp://1.2.3.4:21"}, nil, "https://example.com", true, ""},
		{"manual 空 URL → 直连", models.ProxyConfig{Mode: "manual"}, func() (string, string) { return "http://9.9.9.9:8", "" }, "https://example.com", true, ""},
		{"system 跟随探测到的代理", models.ProxyConfig{Mode: "system"}, func() (string, string) { return "http://127.0.0.1:6152", "" }, "https://example.com", false, "127.0.0.1:6152"},
		{"system 兼容：填 URL 未选模式视为手动", models.ProxyConfig{URL: "http://5.6.7.8:9"}, func() (string, string) { return "http://127.0.0.1:6152", "" }, "https://example.com", false, "5.6.7.8:9"},
		{"system 无系统代理且无 env → 直连", models.ProxyConfig{Mode: "system"}, func() (string, string) { return "", "" }, "https://example.com", true, ""},
		{"system socks 兜底", models.ProxyConfig{Mode: "system"}, func() (string, string) { return "", "socks5://127.0.0.1:7890" }, "https://example.com", false, "127.0.0.1:7890"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := c.detect
			if d == nil {
				d = func() (string, string) { return "", "" }
			}
			defer swapDetect(t, d)()
			cfg := newCfg(t, c.proxy)
			sc := &systemCache{}
			got, err := resolve(cfg, sc, reqURL(t, c.target))
			if err != nil {
				t.Fatalf("resolve error: %v", err)
			}
			if c.wantNil {
				if got != nil {
					t.Fatalf("want nil proxy, got %v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("want proxy host %q, got nil", c.wantHost)
			}
			if got.Host != c.wantHost {
				t.Errorf("proxy host = %q, want %q", got.Host, c.wantHost)
			}
		})
	}
}

// resolve 在 system 模式下命中 localhost 目标时，绝不因系统代理而泄漏本地请求
func TestResolveLoopbackAlwaysDirect(t *testing.T) {
	defer swapDetect(t, func() (string, string) { return "http://127.0.0.1:6152", "socks5://127.0.0.1:6152" })()
	cfg := newCfg(t, models.ProxyConfig{Mode: "system"})
	sc := &systemCache{}
	for _, target := range []string{"http://127.0.0.1:8989/", "http://localhost:11434/v1", "http://[::1]:8080/"} {
		if got, _ := resolve(cfg, sc, reqURL(t, target)); got != nil {
			t.Errorf("本地目标 %s 不应走代理，got %v", target, got)
		}
	}
}

// Install 替换全局 DefaultTransport 后，真实 client 请求 qB(127.0.0.1) 不应被发往代理。
// 用 manual 指向一个必然连不上的地址来证明「本地直连绕过了代理」。
func TestInstallKeepsLocalDirect(t *testing.T) {
	defer restoreDefaultTransport(t)()
	cfg := newCfg(t, models.ProxyConfig{Mode: "manual", URL: "http://192.0.2.1:1"}) // TEST-NET-1，不可路由
	Install(cfg)
	tr := http.DefaultTransport.(*http.Transport)
	req := reqURL(t, "http://127.0.0.1:8989/api/v2/app/version")
	got, err := tr.Proxy(req)
	if err != nil || got != nil {
		t.Fatalf("本地 qB 请求应绕过代理，got=%v err=%v", got, err)
	}
}

func swapDetect(t *testing.T, fn func() (string, string)) func() {
	t.Helper()
	prev := detectSystemProxyFn
	detectSystemProxyFn = fn
	return func() { detectSystemProxyFn = prev }
}

func restoreDefaultTransport(t *testing.T) func() {
	t.Helper()
	prev := http.DefaultTransport
	return func() { http.DefaultTransport = prev }
}
