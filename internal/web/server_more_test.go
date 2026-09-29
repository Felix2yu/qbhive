package web

// 本文件补充 internal/web 包的单元测试，目标是把语句覆盖率从 53% 提到 80%+：
//   - Start / Shutdown（含静态资源服务、端口占用错误分支）
//   - testQB / debugQBRaw / setTorrentLimit
//   - forceRSS / rssReset / rssStatus / testNotify / RandomID
//   - listTorrents 的 filter/sort/reverse/limit/缓存/失败分支
//   - saveConfig / New / login / torrentsStats 的剩余分支
//
// 依赖说明：setupTestServerFull 会构造真实的 limiter/rss/notifier/scheduler/filemgr，
// 因为 forceRSS/rssReset/testNotify 这类 handler 直接调用它们的方法，传 nil 会 panic。

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Felix2yu/qbhive/internal/config"
	"github.com/Felix2yu/qbhive/internal/filemgr"
	"github.com/Felix2yu/qbhive/internal/limiter"
	"github.com/Felix2yu/qbhive/internal/models"
	"github.com/Felix2yu/qbhive/internal/notifier"
	qb "github.com/Felix2yu/qbhive/internal/qbittorrent"
	"github.com/Felix2yu/qbhive/internal/rss"
	"github.com/Felix2yu/qbhive/internal/scheduler"
	"github.com/gin-gonic/gin"
)

// =============== qBittorrent mock（可控行为 + 请求记录） ===============

const defaultTorrentsJSON = `[{"hash":"aaa","name":"Movie.mkv","state":"downloading","progress":0.5,"size":1073741824}]`

// mockQB 是可控行为的 qBittorrent WebUI mock：
// 默认各接口都返回"正常"响应，测试按需覆盖 body/status，并能拿到各接口收到的参数。
type mockQB struct {
	srv *httptest.Server

	mu          sync.Mutex
	torrentsReq []url.Values // /api/v2/torrents/info 收到的 query
	torrentsHdr []http.Header // 同上，收到的请求头（断言 Authorization / Cookie）
	limitReq    []url.Values // /api/v2/torrents/setUploadLimit 收到的 form
	loginReq    []url.Values // /api/v2/auth/login 收到的 form

	// 可控行为
	torrentsBody   string
	torrentsStatus int
	activeBody     string // filter=active 时返回（stats 用）
	stoppedBody    string // filter=stopped 时返回（stats 用）
	stalledBody    string // filter=stalled 时返回（stats 用，5.x 独立 filter）
	erroredBody    string // filter=errored 时返回（stats 用，5.x 独立 filter）
	transferBody   string
	transferStatus int
	limitStatus    int
	versionStatus  int

	// GET /api/v2/torrents/uploadLimit 的响应（getTorrentLimit 用）
	uploadLimitBody   string
	uploadLimitStatus int
}

func newMockQB(t *testing.T) *mockQB {
	t.Helper()
	m := &mockQB{
		torrentsBody: defaultTorrentsJSON,
		activeBody:   defaultTorrentsJSON,
		stoppedBody:  defaultTorrentsJSON,
		stalledBody:  `[]`,
		erroredBody:  `[]`,
		// qB 5.x 的 /transfer/info 只返回 dl_info_speed / dl_rate_limit 这类 key
		transferBody: `{"dl_info_speed":1000,"up_info_speed":2000,"dl_rate_limit":5000,"up_rate_limit":6000}`,
	}
	m.srv = httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockQB) URL() string { return m.srv.URL }

func (m *mockQB) handle(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	w.Header().Set("Content-Type", "application/x-bittorrent")

	m.mu.Lock()
	defer m.mu.Unlock()

	switch r.URL.Path {
	case "/api/v2/auth/login":
		m.loginReq = append(m.loginReq, r.Form.Clone())
		http.SetCookie(w, &http.Cookie{Name: "SID", Value: "testsid", Path: "/"})
		w.Write([]byte("Ok."))
	case "/api/v2/app/version":
		if m.versionStatus != 0 {
			w.WriteHeader(m.versionStatus)
		}
		w.Write([]byte("v5.0.0-test"))
	case "/api/v2/torrents/info":
		m.torrentsReq = append(m.torrentsReq, r.Form.Clone())
		m.torrentsHdr = append(m.torrentsHdr, r.Header.Clone())
		body := m.torrentsBody
		switch r.Form.Get("filter") {
		case "active":
			body = m.activeBody
		case "stopped":
			body = m.stoppedBody
		case "stalled":
			body = m.stalledBody
		case "errored":
			body = m.erroredBody
		}
		if m.torrentsStatus != 0 {
			w.WriteHeader(m.torrentsStatus)
		}
		w.Write([]byte(body))
	case "/api/v2/transfer/info":
		if m.transferStatus != 0 {
			w.WriteHeader(m.transferStatus)
		}
		w.Write([]byte(m.transferBody))
	case "/api/v2/torrents/setUploadLimit":
		m.limitReq = append(m.limitReq, r.Form.Clone())
		if m.limitStatus != 0 {
			w.WriteHeader(m.limitStatus)
			return
		}
		w.Write([]byte(""))
	case "/api/v2/torrents/uploadLimit":
		if m.uploadLimitStatus != 0 {
			w.WriteHeader(m.uploadLimitStatus)
		}
		body := m.uploadLimitBody
		if body == "" {
			body = `{"H1":2048}`
		}
		w.Write([]byte(body))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// torrentsRequests 返回 /api/v2/torrents/info 收到的所有 query（加锁拷贝）
func (m *mockQB) torrentsRequests() []url.Values {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]url.Values, len(m.torrentsReq))
	copy(out, m.torrentsReq)
	return out
}

func (m *mockQB) torrentsHeaders() []http.Header {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]http.Header, len(m.torrentsHdr))
	copy(out, m.torrentsHdr)
	return out
}

func (m *mockQB) limitRequests() []url.Values {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]url.Values, len(m.limitReq))
	copy(out, m.limitReq)
	return out
}

func (m *mockQB) loginRequests() []url.Values {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]url.Values, len(m.loginReq))
	copy(out, m.loginReq)
	return out
}

// =============== 完整依赖的 Server 构造 ===============

// setupTestServerFull 构造带**真实** limiter/rss/notifier/filemgr/scheduler 依赖的 Server。
// dir 是状态目录（config.json / rss_seen.json / finished.json 都放这里），
// 通过 QBHIVE_CONFIG 环境变量让 rss/scheduler 的状态文件也落到 dir，避免污染仓库 data/ 目录。
// 不启动任何后台 ticker（测试配置里 RSS/Limiter/FileManager 均为 disabled）。
func setupTestServerFull(t *testing.T, dir, qbURL, authToken string) *Server {
	t.Helper()
	cfgPath := filepath.Join(dir, "config.json")
	t.Setenv("QBHIVE_CONFIG", cfgPath)

	cfg := config.New(cfgPath)
	cfg.Set(models.AppConfig{
		Server:      models.ServerConfig{Listen: "127.0.0.1:0"},
		Qbittorrent: models.QBConfig{URL: qbURL, Username: "u", Password: "p"},
		RSS:         models.RSSConfig{Enabled: false, Interval: 15},
		Limiter:     models.LimiterConfig{Enabled: false, Interval: 10},
		FileManager: models.FileManagerConfig{Enabled: false, ScanInterval: 60},
		Notifier:    models.NotifierConfig{Enabled: false},
	})
	return newFullServer(t, cfg, authToken)
}

// newFullServer 用给定的 config.Manager 组装完整 Server，并**串行预热一次 qB 登录**。
// 预热的意义：torrentsStats 会并发调用 3 次 qB 接口，若此时 client 还没登录，
// 三个 goroutine 会并发写 client.cookie 字段 → -race 直接报数据竞争。
func newFullServer(t *testing.T, cfg *config.Manager, authToken string) *Server {
	t.Helper()
	qbURL := cfg.Get().Qbittorrent.URL
	baseURL := qbURL
	if baseURL == "" {
		baseURL = "http://127.0.0.1:1"
	}
	qbCli := qb.New(baseURL, "u", "p", "")
	if qbURL != "" {
		if err := qbCli.TestConnection(); err != nil {
			t.Logf("预热 qB 登录失败（忽略，后续用例会覆盖失败分支）: %v", err)
		}
	}
	notif := notifier.New(cfg.Get().Notifier)
	lim := limiter.New(cfg, qbCli)
	rssE := rss.New(cfg, qbCli)
	fm := filemgr.New(cfg, qbCli)
	sch := scheduler.New(cfg, qbCli, notif, lim, fm, rssE)

	srv := New(cfg, qbCli, lim, rssE, notif, sch, fm)
	srv.authToken = authToken // 直接设，绕过 env 依赖（与 setupTestServer 保持一致）
	return srv
}

// =============== 通用小工具 ===============

type apiResp struct {
	Success bool            `json:"success"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// decodeAPI 把 handler 响应解析成通用结构，响应不是 JSON 时直接 Fatal
func decodeAPI(t *testing.T, w *httptest.ResponseRecorder) apiResp {
	t.Helper()
	var out apiResp
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v, body=%s", err, w.Body.String())
	}
	return out
}

// bindData 把响应的 data 字段反序列化到 out
func (r apiResp) bindData(t *testing.T, out interface{}) {
	t.Helper()
	if len(r.Data) == 0 {
		t.Fatalf("响应缺少 data 字段: message=%q", r.Message)
	}
	if err := json.Unmarshal(r.Data, out); err != nil {
		t.Fatalf("解析 data 失败: %v, raw=%s", err, string(r.Data))
	}
}

// serveJSON 以 JSON 形式调用 gin 路由并返回 recorder
func serveJSON(t *testing.T, r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, path, reader)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// freeLocalAddr 取一个当前空闲的 127.0.0.1 端口
func freeLocalAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("获取空闲端口失败: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("释放探测端口失败: %v", err)
	}
	return addr
}

// waitListening 轮询直到 addr 上有监听（Start 在 goroutine 里跑，需要等待就绪）
func waitListening(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("服务未在 %s 开始监听", addr)
}

// httpGet 发起 GET 并返回状态码、body、响应头
func httpGet(t *testing.T, rawURL string) (int, string, http.Header) {
	t.Helper()
	resp, err := http.Get(rawURL)
	if err != nil {
		t.Fatalf("GET %s 失败: %v", rawURL, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取 %s 响应失败: %v", rawURL, err)
	}
	return resp.StatusCode, string(b), resp.Header
}

// numVal 把 JSON 反序列化后的数值断言为 float64
func numVal(t *testing.T, v interface{}) float64 {
	t.Helper()
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("期望数值字段，实际 %T=%v", v, v)
	}
	return f
}

// =============== New / Start / Shutdown ===============

// TestNew_EnvTokenEnablesAuth 覆盖 New() 里读取 QBHIVE_TOKEN 的分支。
// New 只负责装配字段（不触碰依赖），所以这里依赖可以直接传 nil。
func TestNew_EnvTokenEnablesAuth(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")

	t.Setenv("QBHIVE_TOKEN", "env-secret")
	srv := New(config.New(cfgPath), nil, nil, nil, nil, nil, nil)
	if srv.authToken != "env-secret" {
		t.Errorf("QBHIVE_TOKEN 已设置，authToken 应为 env-secret，实际 %q", srv.authToken)
	}
	if srv.tcache == nil {
		t.Error("New 必须初始化 tcache（nil map 写会 panic）")
	}

	t.Setenv("QBHIVE_TOKEN", "")
	srv2 := New(config.New(cfgPath), nil, nil, nil, nil, nil, nil)
	if srv2.authToken != "" {
		t.Errorf("QBHIVE_TOKEN 为空时应不鉴权，实际 %q", srv2.authToken)
	}
}

// TestStartShutdown_ServesStaticAndAPI 覆盖 Start（含静态资源）与 Shutdown 全流程
func TestStartShutdown_ServesStaticAndAPI(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := setupTestServerFull(t, t.TempDir(), "", "")

	webRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(webRoot, "index.html"), []byte("<html>QBHive Home</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(webRoot, "app.js"), []byte("console.log('qbhive')"), 0o644); err != nil {
		t.Fatal(err)
	}

	addr := freeLocalAddr(t)
	cfg := s.cfg.Get()
	cfg.Server.Listen = addr
	s.cfg.Set(cfg)

	errCh := make(chan error, 1)
	go func() { errCh <- s.Start(webRoot) }()
	waitListening(t, addr)

	// 首页（static.Serve + index）
	if code, body, _ := httpGet(t, "http://"+addr+"/"); code != 200 || !strings.Contains(body, "QBHive Home") {
		t.Errorf("首页应返回 index.html，got code=%d body=%q", code, body)
	}
	// 普通静态文件
	if code, body, _ := httpGet(t, "http://"+addr+"/app.js"); code != 200 || !strings.Contains(body, "qbhive") {
		t.Errorf("静态文件应可访问，got code=%d body=%q", code, body)
	}
	// API 路由
	if code, body, _ := httpGet(t, "http://"+addr+"/api/ping"); code != 200 || !strings.Contains(body, `"success":true`) || !strings.Contains(body, "pong") {
		t.Errorf("/api/ping 应返回 pong，got code=%d body=%q", code, body)
	}
	// 鉴权状态接口（本例未设置 token → auth_required=false）
	if code, body, _ := httpGet(t, "http://"+addr+"/api/auth/status"); code != 200 || !strings.Contains(body, `"auth_required":false`) {
		t.Errorf("/api/auth/status 应返回 auth_required=false，got code=%d body=%q", code, body)
	}
	// 不存在的路径
	if code, _, _ := httpGet(t, "http://"+addr+"/no-such-file"); code != 404 {
		t.Errorf("不存在的路径应 404，got %d", code)
	}

	if err := s.Shutdown(); err != nil {
		t.Fatalf("Shutdown 失败: %v", err)
	}
	if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
		t.Errorf("Start 应返回 http.ErrServerClosed，实际 %v", err)
	}
	// Shutdown 后端口必须释放
	if conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Errorf("Shutdown 后 %s 仍在监听", addr)
	}
}

// TestStart_PortOccupiedReturnsError 覆盖 Start 的监听失败分支与 webRoot=="" 分支
func TestStart_PortOccupiedReturnsError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := setupTestServerFull(t, t.TempDir(), "", "")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	cfg := s.cfg.Get()
	cfg.Server.Listen = ln.Addr().String()
	s.cfg.Set(cfg)

	// webRoot="" → 不挂静态资源；端口被占用 → ListenAndServe 立即报错
	err = s.Start("")
	if err == nil {
		t.Fatal("端口被占用时 Start 应返回错误")
	}
	if errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("期望 bind 错误，实际 %v", err)
	}
	// httpSrv 已赋值但从未服务，Shutdown 应返回 nil
	if err := s.Shutdown(); err != nil {
		t.Fatalf("Shutdown 应成功，实际 %v", err)
	}
}

// TestShutdown_WithoutStartReturnsNil 覆盖 Shutdown 里 httpSrv == nil 的分支
func TestShutdown_WithoutStartReturnsNil(t *testing.T) {
	s := setupTestServerFull(t, t.TempDir(), "", "")
	if err := s.Shutdown(); err != nil {
		t.Errorf("未 Start 时 Shutdown 应返回 nil，实际 %v", err)
	}
}

// =============== login 剩余分支 ===============

// TestLogin_InvalidJSONReturns400 覆盖 login 的 bind 失败分支
func TestLogin_InvalidJSONReturns400(t *testing.T) {
	s := setupTestServer(t, "", "secret")
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/login", s.login)

	w := serveJSON(t, r, "POST", "/api/login", "not-json")
	if w.Code != 400 {
		t.Errorf("非法 JSON 应返回 400，got %d body=%s", w.Code, w.Body.String())
	}
	resp := decodeAPI(t, w)
	if resp.Success {
		t.Error("非法 JSON 不应 success")
	}
}

// =============== saveConfig 剩余分支 ===============

func configBodyWithURL(url, apprise string) string {
	return `{"qbittorrent":{"url":"` + url + `","username":"u","password":"p"},` +
		`"rss":{"enabled":false,"interval":15,"feeds":[]},` +
		`"limiter":{"enabled":false,"interval":10,"rules":[]},` +
		`"fileManager":{"enabled":false,"scanInterval":60},` +
		`"notifier":{"enabled":true,"appriseUrls":[` + apprise + `]}}`
}

// TestSaveConfig_InvalidJSONReturns400 覆盖 saveConfig 的 bind 失败分支
func TestSaveConfig_InvalidJSONReturns400(t *testing.T) {
	s := setupTestServerFull(t, t.TempDir(), "", "")
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/config", s.saveConfig)

	w := serveJSON(t, r, "POST", "/api/config", "not-json")
	if w.Code != 400 {
		t.Errorf("非法 JSON 应返回 400，got %d", w.Code)
	}
	resp := decodeAPI(t, w)
	if resp.Success || resp.Message == "" {
		t.Errorf("应返回 success=false + 错误消息，got %+v", resp)
	}
}

// TestSaveConfig_PersistFailureReturns500 覆盖 cfg.Save() 失败的 500 分支
func TestSaveConfig_PersistFailureReturns500(t *testing.T) {
	dir := t.TempDir()
	// 让配置文件路径指向一个**已存在的目录**：Save() 里 os.Rename(tmp, 目录) 必然失败
	cfgPath := filepath.Join(dir, "config-as-dir")
	if err := os.Mkdir(cfgPath, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QBHIVE_CONFIG", filepath.Join(dir, "config.json"))
	cfg := config.New(cfgPath) // Load 失败 → 走默认配置；内部 Save 也失败（被忽略）
	s := newFullServer(t, cfg, "")

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/config", s.saveConfig)

	w := serveJSON(t, r, "POST", "/api/config", configBodyWithURL("http://new.example:8080", `"tgram://x/y"`))
	if w.Code != 500 {
		t.Fatalf("持久化失败应返回 500，got %d body=%s", w.Code, w.Body.String())
	}
	resp := decodeAPI(t, w)
	if resp.Success || resp.Message == "" {
		t.Errorf("应返回 success=false + 错误消息，got %+v", resp)
	}
}

// TestSaveConfig_HotReloadWithScheduler 覆盖 s.scheduler != nil → ReloadAll 分支
func TestSaveConfig_HotReloadWithScheduler(t *testing.T) {
	mock := newMockQB(t)
	s := setupTestServerFull(t, t.TempDir(), mock.URL(), "")
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/config", s.saveConfig)

	w := serveJSON(t, r, "POST", "/api/config", configBodyWithURL(mock.URL(), `"tgram://x/y"`))
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if resp := decodeAPI(t, w); !resp.Success {
		t.Fatalf("expected success, got %+v", resp)
	}
	// ReloadAll 会把新 URL 热更新进 client（SetCredentials），验证配置确实生效
	got := s.cfg.Get()
	if got.Qbittorrent.URL != mock.URL() {
		t.Errorf("配置未更新: %q", got.Qbittorrent.URL)
	}
	// notifier.Reload 也被调用：Enabled + URL 应生效
	if !s.notifier.Enabled() {
		t.Error("notifier 应在热更新后处于启用状态")
	}
}

// TestSaveConfig_AppriseURLsExtendByIndex 覆盖"新增 URL 超出原数组长度"的扩展分支
func TestSaveConfig_AppriseURLsExtendByIndex(t *testing.T) {
	s := setupTestServerFull(t, t.TempDir(), "", "")
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/config", s.saveConfig)

	s.cfg.Set(models.AppConfig{
		Notifier: models.NotifierConfig{Enabled: true, AppriseURLs: []string{"ntfys://old@ntfy.sh/a"}},
	})
	_ = s.cfg.Save()

	// 前端传 2 项：第 0 项是新值，第 1 项是新增 → base 需要从 1 扩到 2
	body := configBodyWithURL("http://x", `"ntfys://new@ntfy.sh/a","slack://TOK/CHAN"`)
	w := serveJSON(t, r, "POST", "/api/config", body)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	got := s.cfg.Get().Notifier.AppriseURLs
	want := []string{"ntfys://new@ntfy.sh/a", "slack://TOK/CHAN"}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("AppriseURLs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestSaveConfig_AppriseURLsAllBlankClears 覆盖"输入全为空行 → 过滤后为空"分支：
// AppriseURLs 已不做掩码，全空输入就是清空，不再保留原值
func TestSaveConfig_AppriseURLsAllBlankClears(t *testing.T) {
	s := setupTestServerFull(t, t.TempDir(), "", "")
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/config", s.saveConfig)

	s.cfg.Set(models.AppConfig{Notifier: models.NotifierConfig{Enabled: true, AppriseURLs: []string{"ntfys://key@ntfy.sh/qbhive"}}})
	_ = s.cfg.Save()

	// 两个空字符串（用户把两行都删了）→ 没有"新输入" → 清空
	body := configBodyWithURL("http://x", `"",""`)
	w := serveJSON(t, r, "POST", "/api/config", body)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	got := s.cfg.Get().Notifier.AppriseURLs
	if len(got) != 0 {
		t.Errorf("全空输入应清空 AppriseURLs，got %v", got)
	}
}

// TestSaveConfig_AppriseURLsTrimsAndDropsBlanks 覆盖逐项 trim + 过滤空项的分支
func TestSaveConfig_AppriseURLsTrimsAndDropsBlanks(t *testing.T) {
	s := setupTestServerFull(t, t.TempDir(), "", "")
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/config", s.saveConfig)

	body := configBodyWithURL("http://x", `"  ntfys://a@b/c  ","   ","slack://T/C"`)
	w := serveJSON(t, r, "POST", "/api/config", body)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	got := s.cfg.Get().Notifier.AppriseURLs
	want := []string{"ntfys://a@b/c", "slack://T/C"}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("AppriseURLs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// =============== testQB ===============

// TestTestQB_SuccessAndMaskFallback 覆盖 testQB 成功分支 + 掩码回退分支
func TestTestQB_SuccessAndMaskFallback(t *testing.T) {
	mock := newMockQB(t)
	s := setupTestServerFull(t, t.TempDir(), mock.URL(), "")
	// 当前已保存的真实凭据
	s.cfg.Set(models.AppConfig{
		Qbittorrent: models.QBConfig{URL: mock.URL(), Username: "saved-u", Password: "real-pass", APIKey: ""},
	})
	_ = s.cfg.Save()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/test-qb", s.testQB)

	// 前端默认带着掩码密码提交 → 应回退成已保存的真实密码
	body := `{"url":"` + mock.URL() + `","username":"form-u","password":"********"}`
	w := serveJSON(t, r, "POST", "/api/test-qb", body)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	resp := decodeAPI(t, w)
	if !resp.Success {
		t.Fatalf("连接测试应成功，got %+v", resp)
	}
	logins := mock.loginRequests()
	if len(logins) == 0 {
		t.Fatal("mock 未收到登录请求")
	}
	// 最后一次登录才是 testQB 发起的（setup 里预热登录排在前面）
	last := logins[len(logins)-1]
	if got := last.Get("password"); got != "real-pass" {
		t.Errorf("掩码密码应回退为已保存凭据，got %q", got)
	}
	if got := last.Get("username"); got != "form-u" {
		t.Errorf("username 应取表单值，got %q", got)
	}
}

// TestTestQB_ConnectionFailure 覆盖 testQB 的失败分支（HTTP 非 200）
func TestTestQB_ConnectionFailure(t *testing.T) {
	mock := newMockQB(t)
	mock.versionStatus = http.StatusInternalServerError
	s := setupTestServerFull(t, t.TempDir(), mock.URL(), "")

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/test-qb", s.testQB)

	body := `{"url":"` + mock.URL() + `","username":"u","password":"p"}`
	w := serveJSON(t, r, "POST", "/api/test-qb", body)
	if w.Code != 200 {
		t.Fatalf("testQB 失败时也返回 200，got %d", w.Code)
	}
	resp := decodeAPI(t, w)
	if resp.Success {
		t.Fatal("qB 返回 500 时应 success=false")
	}
	if !strings.Contains(resp.Message, "500") {
		t.Errorf("错误消息应包含状态码 500，got %q", resp.Message)
	}
}

// TestTestQB_InvalidJSON 覆盖 testQB 的 bind 失败分支
func TestTestQB_InvalidJSON(t *testing.T) {
	s := setupTestServerFull(t, t.TempDir(), "", "")
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/test-qb", s.testQB)

	w := serveJSON(t, r, "POST", "/api/test-qb", "not-json")
	if w.Code != 400 {
		t.Errorf("非法 JSON 应返回 400，got %d", w.Code)
	}
}

// =============== debugQBRaw ===============

// TestDebugQBRaw_NoURLConfigured 覆盖 qbURL 为空的 500 分支
func TestDebugQBRaw_NoURLConfigured(t *testing.T) {
	s := setupTestServerFull(t, t.TempDir(), "", "")
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/debug/qb-raw", s.debugQBRaw)

	w := serveJSON(t, r, "GET", "/api/debug/qb-raw", "")
	if w.Code != 500 {
		t.Errorf("未配置 qB URL 应返回 500，got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "no qbittorrent url configured") {
		t.Errorf("unexpected body: %s", w.Body.String())
	}
}

// TestDebugQBRaw_UsesAPIKeyHeader 覆盖"配置了 APIKey → 路径 A 带 X-Webapi-Key 头"分支
func TestDebugQBRaw_UsesAPIKeyHeader(t *testing.T) {
	mock := newMockQB(t)
	s := setupTestServerFull(t, t.TempDir(), mock.URL(), "")
	cfg := s.cfg.Get()
	cfg.Qbittorrent.APIKey = "k-123"
	s.cfg.Set(cfg)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/debug/qb-raw", s.debugQBRaw)

	w := serveJSON(t, r, "GET", "/api/debug/qb-raw?filter=all", "")
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{"=== 路径 A", "Status: 200", "=== 路径 B", "listLen=1", "err=<nil>"} {
		if !strings.Contains(body, want) {
			t.Errorf("响应缺少 %q: %s", want, body)
		}
	}
	hdrs := mock.torrentsHeaders()
	if len(hdrs) == 0 {
		t.Fatal("mock 未收到 torrents/info 请求")
	}
	if got := hdrs[0].Get("X-Webapi-Key"); got != "k-123" {
		t.Errorf("路径 A 应带 X-Webapi-Key: k-123，got %q", got)
	}
}

// TestDebugQBRaw_UsesCookieHeader 覆盖"无 APIKey → 路径 A 带 Cookie 头"分支
func TestDebugQBRaw_UsesCookieHeader(t *testing.T) {
	mock := newMockQB(t)
	s := setupTestServerFull(t, t.TempDir(), mock.URL(), "") // setup 里已预热登录

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/debug/qb-raw", s.debugQBRaw)

	w := serveJSON(t, r, "GET", "/api/debug/qb-raw?filter=all&sort=name&reverse=true", "")
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "listLen=1") {
		t.Errorf("路径 B 应拉到 1 条: %s", w.Body.String())
	}
	hdrs := mock.torrentsHeaders()
	if len(hdrs) == 0 {
		t.Fatal("mock 未收到 torrents/info 请求")
	}
	if got := hdrs[0].Get("Cookie"); !strings.Contains(got, "SID=testsid") {
		t.Errorf("路径 A 应带 SID cookie，got %q", got)
	}
}

// TestDebugQBRaw_QBUnreachable 覆盖路径 A 网络失败（errA != nil）与路径 B 调用失败分支
func TestDebugQBRaw_QBUnreachable(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // 立即关闭 → 连接必被拒绝

	s := setupTestServerFull(t, t.TempDir(), deadURL, "")
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/debug/qb-raw", s.debugQBRaw)

	w := serveJSON(t, r, "GET", "/api/debug/qb-raw?filter=all", "")
	if w.Code != 200 {
		t.Fatalf("debugQBRaw 始终返回 200，got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Status: 0") || !strings.Contains(body, "Len: 0") {
		t.Errorf("路径 A 失败时应 Status: 0 / Len: 0: %s", body)
	}
	if strings.Contains(body, "err=<nil>") {
		t.Errorf("路径 B 应报错: %s", body)
	}
}

// =============== listTorrents 剩余分支 ===============

// TestListTorrents_QueryMapping 表驱动覆盖 filter/sort/reverse 参数归一化与透传
func TestListTorrents_QueryMapping(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name        string
		query       string // 拼在 /api/torrents 后的 query
		wantFilter  string // 期望转发给 qB 的 filter 参数（"" = 不带该参数）
		wantSort    string
		wantReverse string
	}{
		{"空 filter 默认 active", "?", "active", "added_on", "true"},
		{"downloading 透传", "?filter=downloading", "downloading", "added_on", "true"},
		{"seeding 透传", "?filter=seeding", "seeding", "added_on", "true"},
		{"completed 透传", "?filter=completed", "completed", "added_on", "true"},
		{"stalledUP 映射 stalled_uploading", "?filter=stalledUP", "stalled_uploading", "added_on", "true"},
		{"stalledDL 映射 stalled_downloading", "?filter=stalledDL", "stalled_downloading", "added_on", "true"},
		{"stoppedUP 映射 stopped", "?filter=stoppedUP", "stopped", "added_on", "true"},
		{"stoppedDL 映射 stopped", "?filter=stoppedDL", "stopped", "added_on", "true"},
		{"all 不带 filter 参数", "?filter=all", "", "added_on", "true"},
		{"未知 filter 原样透传", "?filter=customState", "customState", "added_on", "true"},
		{"空 sort 默认 added_on", "?filter=all&sort=", "", "added_on", "true"},
		{"自定义 sort 透传", "?filter=all&sort=name", "", "name", "true"},
		{"reverse=1 归一化 true", "?filter=all&reverse=1", "", "added_on", "true"},
		{"reverse=yes 归一化 true", "?filter=all&reverse=yes", "", "added_on", "true"},
		{"reverse=on 归一化 true", "?filter=all&reverse=on", "", "added_on", "true"},
		{"reverse=0 归一化 false", "?filter=all&reverse=0", "", "added_on", "false"},
		{"reverse=no 归一化 false", "?filter=all&reverse=no", "", "added_on", "false"},
		{"reverse=off 归一化 false", "?filter=all&reverse=off", "", "added_on", "false"},
		{"reverse=false 原样保留", "?filter=all&reverse=false", "", "added_on", "false"},
		{"reverse 非法值原样透传", "?filter=all&reverse=maybe", "", "added_on", "maybe"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := newMockQB(t)
			// stoppedUP / stoppedDL 走 qB filter=stopped，mock 需要返回匹配 state 的数据，
			// 否则会被 listTorrents 的 state 二次过滤清空
			switch {
			case strings.Contains(tc.query, "filter=stoppedUP"):
				mock.stoppedBody = `[{"hash":"aaa","state":"stoppedUP"}]`
			case strings.Contains(tc.query, "filter=stoppedDL"):
				mock.stoppedBody = `[{"hash":"aaa","state":"stoppedDL"}]`
			}
			s := setupTestServerFull(t, t.TempDir(), mock.URL(), "")
			r := gin.New()
			r.GET("/api/torrents", s.listTorrents)

			w := serveJSON(t, r, "GET", "/api/torrents"+tc.query, "")
			if w.Code != 200 {
				t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
			}
			resp := decodeAPI(t, w)
			if !resp.Success {
				t.Fatalf("expected success, got %+v", resp)
			}
			var list []models.QBTorrent
			resp.bindData(t, &list)
			if len(list) != 1 || list[0].Hash != "aaa" {
				t.Errorf("期望 1 条 hash=aaa，got %+v", list)
			}

			reqs := mock.torrentsRequests()
			if len(reqs) != 1 {
				t.Fatalf("qB 应被调用 1 次，实际 %d 次", len(reqs))
			}
			if got := reqs[0].Get("filter"); got != tc.wantFilter {
				t.Errorf("qB filter = %q, want %q", got, tc.wantFilter)
			}
			if got := reqs[0].Get("sort"); got != tc.wantSort {
				t.Errorf("qB sort = %q, want %q", got, tc.wantSort)
			}
			if got := reqs[0].Get("reverse"); got != tc.wantReverse {
				t.Errorf("qB reverse = %q, want %q", got, tc.wantReverse)
			}
		})
	}
}

// TestListTorrents_StateSecondaryFilter 覆盖 stoppedUP/stoppedDL 的 state 二次过滤
func TestListTorrents_StateSecondaryFilter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name        string
		filter      string
		stoppedBody string
		wantHashes  []string
	}{
		{
			name:        "stoppedUP 只保留 stoppedUP",
			filter:      "stoppedUP",
			stoppedBody: `[{"hash":"u1","state":"stoppedUP"},{"hash":"d1","state":"stoppedDL"},{"hash":"u2","state":"stoppedUP"}]`,
			wantHashes:  []string{"u1", "u2"},
		},
		{
			name:        "stoppedDL 只保留 stoppedDL",
			filter:      "stoppedDL",
			stoppedBody: `[{"hash":"u1","state":"stoppedUP"},{"hash":"d1","state":"stoppedDL"}]`,
			wantHashes:  []string{"d1"},
		},
		{
			name:        "stoppedUP 无匹配时返回空数组",
			filter:      "stoppedUP",
			stoppedBody: `[{"hash":"d1","state":"stoppedDL"}]`,
			wantHashes:  []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := newMockQB(t)
			mock.stoppedBody = tc.stoppedBody
			s := setupTestServerFull(t, t.TempDir(), mock.URL(), "")
			r := gin.New()
			r.GET("/api/torrents", s.listTorrents)

			w := serveJSON(t, r, "GET", "/api/torrents?filter="+tc.filter, "")
			if w.Code != 200 {
				t.Fatalf("expected 200, got %d", w.Code)
			}
			var list []models.QBTorrent
			decodeAPI(t, w).bindData(t, &list)

			got := make([]string, 0, len(list))
			for _, item := range list {
				got = append(got, item.Hash)
			}
			if len(got) != len(tc.wantHashes) {
				t.Fatalf("hashes = %v, want %v", got, tc.wantHashes)
			}
			for i := range tc.wantHashes {
				if got[i] != tc.wantHashes[i] {
					t.Errorf("hashes[%d] = %q, want %q", i, got[i], tc.wantHashes[i])
				}
			}
			reqs := mock.torrentsRequests()
			if len(reqs) != 1 || reqs[0].Get("filter") != "stopped" {
				t.Errorf("qB 应收到 filter=stopped，got %v", reqs)
			}
		})
	}
}

// TestListTorrents_LimitHandling 表驱动覆盖 limit 的各个分支（默认/截断/全量告警/钳制/非法值）
func TestListTorrents_LimitHandling(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// 4.x 的 paused state 已不存在，这里用 5.x 真实 state（stoppedUP/stoppedDL 等）
	three := `[{"hash":"h1","state":"downloading"},{"hash":"h2","state":"uploading"},{"hash":"h3","state":"stoppedUP"}]`
	cases := []struct {
		name        string
		limit       string
		wantCount   int
		wantWarning string // "" 表示必须没有 X-Warning
	}{
		{"空 limit 用默认 500", "", 3, ""},
		{"正常 limit 截断", "2", 2, ""},
		{"limit=0 全量并告警", "0", 3, "full list requested"},
		{"limit 超过上限被钳制", "5000", 3, "limit capped at 2000"},
		{"负数 limit 用默认值", "-5", 3, ""},
		{"非数字 limit 用默认值", "abc", 3, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := newMockQB(t)
			mock.torrentsBody = three
			s := setupTestServerFull(t, t.TempDir(), mock.URL(), "")
			r := gin.New()
			r.GET("/api/torrents", s.listTorrents)

			w := serveJSON(t, r, "GET", "/api/torrents?filter=all&limit="+tc.limit, "")
			if w.Code != 200 {
				t.Fatalf("expected 200, got %d", w.Code)
			}
			var list []models.QBTorrent
			decodeAPI(t, w).bindData(t, &list)
			if len(list) != tc.wantCount {
				t.Errorf("返回 %d 条，want %d", len(list), tc.wantCount)
			}
			warn := w.Header().Get("X-Warning")
			if tc.wantWarning == "" {
				if warn != "" {
					t.Errorf("不应有 X-Warning，got %q", warn)
				}
			} else if !strings.Contains(warn, tc.wantWarning) {
				t.Errorf("X-Warning = %q, 应包含 %q", warn, tc.wantWarning)
			}
		})
	}
}

// TestListTorrents_CacheHitTruncates 覆盖缓存命中分支（含命中后的 limit 截断）
func TestListTorrents_CacheHitTruncates(t *testing.T) {
	mock := newMockQB(t)
	mock.torrentsBody = `[{"hash":"h1"},{"hash":"h2"},{"hash":"h3"}]`
	s := setupTestServerFull(t, t.TempDir(), mock.URL(), "")
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/torrents", s.listTorrents)

	// 第一次：cache miss → 拉 qB 并写缓存
	w := serveJSON(t, r, "GET", "/api/torrents?filter=all&limit=500", "")
	var list []models.QBTorrent
	decodeAPI(t, w).bindData(t, &list)
	if len(list) != 3 {
		t.Fatalf("首次应返回 3 条，got %d", len(list))
	}

	// 第二次：同 filter/sort/reverse → 命中缓存，且按 limit=2 截断
	w = serveJSON(t, r, "GET", "/api/torrents?filter=all&limit=2", "")
	list = nil
	decodeAPI(t, w).bindData(t, &list)
	if len(list) != 2 {
		t.Errorf("缓存命中后应按 limit 截断为 2 条，got %d", len(list))
	}
	if got := len(mock.torrentsRequests()); got != 1 {
		t.Errorf("第二次应命中缓存，qB 只应被调用 1 次，实际 %d 次", got)
	}
}

// TestListTorrents_QBFailureReturns502 覆盖 qB 调用失败 → 502 分支
func TestListTorrents_QBFailureReturns502(t *testing.T) {
	mock := newMockQB(t)
	mock.torrentsStatus = http.StatusInternalServerError
	mock.torrentsBody = "boom" // 非 JSON → 解析失败
	mock.activeBody = "boom"
	s := setupTestServerFull(t, t.TempDir(), mock.URL(), "")
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/torrents", s.listTorrents)

	w := serveJSON(t, r, "GET", "/api/torrents?filter=active", "")
	if w.Code != 502 {
		t.Fatalf("qB 失败应返回 502，got %d body=%s", w.Code, w.Body.String())
	}
	resp := decodeAPI(t, w)
	if resp.Success || resp.Message == "" {
		t.Errorf("应返回 success=false + 错误消息，got %+v", resp)
	}
}

// =============== torrentsStats 剩余分支 ===============

// TestTorrentsStats_CountsAndCache 覆盖各 state 计数分支 + 3 秒缓存命中分支。
// qB 5.x 语义：active filter 只含有实际传输速度的任务（下载中/做种中），
// stalled 与 errored 各走独立 filter，stopped 独立一路，共 4 路 torrents/info。
func TestTorrentsStats_CountsAndCache(t *testing.T) {
	mock := newMockQB(t)
	// active 列表：只应统计 downloading 系与 uploading 系（其余状态无速度，不进该列表）
	mock.activeBody = `[
		{"state":"downloading"},{"state":"forcedDL"},{"state":"metaDL"},{"state":"forcedMetaDL"},
		{"state":"uploading"},{"state":"forcedUP"},
		{"state":"checkingUP"},{"state":"queuedUP"},{"state":"stalledDL"},{"state":"checkingDL"}
	]`
	mock.stoppedBody = `[{"state":"stoppedUP"},{"state":"stoppedUP"},{"state":"stoppedDL"}]`
	mock.stalledBody = `[{"state":"stalledDL"},{"state":"stalledUP"},{"state":"stalledUP"}]`
	mock.erroredBody = `[{"state":"error"},{"state":"missingFiles"}]`
	s := setupTestServerFull(t, t.TempDir(), mock.URL(), "")
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/torrents/stats", s.torrentsStats)

	w := serveJSON(t, r, "GET", "/api/torrents/stats", "")
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	var data map[string]interface{}
	decodeAPI(t, w).bindData(t, &data)

	want := map[string]float64{
		"dlSpeed":          1000,
		"upSpeed":          2000,
		"dlSpeedLimit":     5000,
		"upSpeedLimit":     6000,
		"activeCount":      10,
		"stoppedUP":        2,
		"stoppedDL":        1,
		"downloadingCount": 4,
		"seedingCount":     2,
		"stalledCount":     3,
		"erroredCount":     2,
	}
	for key, expect := range want {
		got, ok := data[key]
		if !ok {
			t.Errorf("缺少字段 %s", key)
			continue
		}
		if numVal(t, got) != expect {
			t.Errorf("%s = %v, want %v", key, got, expect)
		}
	}

	// 第二次请求应命中 3 秒缓存 → qB 不再被调用
	w = serveJSON(t, r, "GET", "/api/torrents/stats", "")
	if w.Code != 200 {
		t.Fatalf("缓存命中时也应 200，got %d", w.Code)
	}
	if got := len(mock.torrentsRequests()); got != 4 {
		t.Errorf("第二次应命中缓存，qB info 只应被调用 4 次（active+stopped+stalled+errored），实际 %d 次", got)
	}
}

// TestTorrentsStats_TransferInfoFailure502 覆盖 transfer/info 失败 → 502 分支
func TestTorrentsStats_TransferInfoFailure502(t *testing.T) {
	mock := newMockQB(t)
	mock.transferStatus = http.StatusInternalServerError
	mock.transferBody = "boom"
	s := setupTestServerFull(t, t.TempDir(), mock.URL(), "")
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/torrents/stats", s.torrentsStats)

	w := serveJSON(t, r, "GET", "/api/torrents/stats", "")
	if w.Code != 502 {
		t.Fatalf("transfer/info 失败应返回 502，got %d body=%s", w.Code, w.Body.String())
	}
	resp := decodeAPI(t, w)
	if resp.Success || resp.Message == "" {
		t.Errorf("应返回 success=false + 错误消息，got %+v", resp)
	}
}

// TestTorrentsStats_ListFailureCountsZero 覆盖"列表失败只计数归零"的分支
func TestTorrentsStats_ListFailureCountsZero(t *testing.T) {
	mock := newMockQB(t)
	mock.activeBody = "boom"  // 非 JSON → GetTorrents 失败（错误被忽略）
	mock.stoppedBody = "boom" // 非 JSON → GetTorrents 失败（错误被忽略）
	mock.stalledBody = "boom"
	mock.erroredBody = "boom"
	s := setupTestServerFull(t, t.TempDir(), mock.URL(), "")
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/torrents/stats", s.torrentsStats)

	w := serveJSON(t, r, "GET", "/api/torrents/stats", "")
	if w.Code != 200 {
		t.Fatalf("列表失败仍应 200，got %d body=%s", w.Code, w.Body.String())
	}
	var data map[string]interface{}
	decodeAPI(t, w).bindData(t, &data)
	for _, key := range []string{"activeCount", "stoppedUP", "stoppedDL", "downloadingCount", "seedingCount", "stalledCount", "erroredCount"} {
		if numVal(t, data[key]) != 0 {
			t.Errorf("%s 应为 0，got %v", key, data[key])
		}
	}
	// 速度字段依然来自 transfer/info（正常）
	if numVal(t, data["dlSpeed"]) != 1000 {
		t.Errorf("dlSpeed = %v, want 1000", data["dlSpeed"])
	}
}

// =============== setTorrentLimit ===============

// TestSetTorrentLimit_AppliesAndConverts 表驱动覆盖 0 → -1（不限速）与 N → N*1024 的换算
func TestSetTorrentLimit_AppliesAndConverts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name      string
		body      string
		wantLimit string // qB 收到的 limit（字节/秒）
	}{
		{"uploadLimit=0 → -1（取消限速）", `{"uploadLimit":0}`, "-1"},
		{"uploadLimit=100 → 102400", `{"uploadLimit":100}`, "102400"},
		{"uploadLimit=1 → 1024", `{"uploadLimit":1}`, "1024"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := newMockQB(t)
			s := setupTestServerFull(t, t.TempDir(), mock.URL(), "")
			r := gin.New()
			r.POST("/api/torrents/:hash/limit", s.setTorrentLimit)

			w := serveJSON(t, r, "POST", "/api/torrents/ABC123DEF/limit", tc.body)
			if w.Code != 200 {
				t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
			}
			if resp := decodeAPI(t, w); !resp.Success {
				t.Fatalf("expected success, got %+v", resp)
			}
			reqs := mock.limitRequests()
			if len(reqs) != 1 {
				t.Fatalf("qB 应收到 1 次 setUploadLimit，实际 %d", len(reqs))
			}
			if got := reqs[0].Get("hashes"); got != "ABC123DEF" {
				t.Errorf("hashes = %q, want ABC123DEF", got)
			}
			if got := reqs[0].Get("limit"); got != tc.wantLimit {
				t.Errorf("limit = %q, want %q", got, tc.wantLimit)
			}
		})
	}
}

// TestSetTorrentLimit_FailureBranches 覆盖 qB 失败 → 500 与 bind 失败 → 400
func TestSetTorrentLimit_FailureBranches(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("qB 返回 500 → 接口 500", func(t *testing.T) {
		mock := newMockQB(t)
		mock.limitStatus = http.StatusInternalServerError
		s := setupTestServerFull(t, t.TempDir(), mock.URL(), "")
		r := gin.New()
		r.POST("/api/torrents/:hash/limit", s.setTorrentLimit)

		w := serveJSON(t, r, "POST", "/api/torrents/H1/limit", `{"uploadLimit":10}`)
		if w.Code != 500 {
			t.Fatalf("qB 失败应返回 500，got %d body=%s", w.Code, w.Body.String())
		}
		resp := decodeAPI(t, w)
		if resp.Success || resp.Message == "" {
			t.Errorf("应返回 success=false + 错误消息，got %+v", resp)
		}
	})

	t.Run("非法 JSON → 400", func(t *testing.T) {
		mock := newMockQB(t)
		s := setupTestServerFull(t, t.TempDir(), mock.URL(), "")
		r := gin.New()
		r.POST("/api/torrents/:hash/limit", s.setTorrentLimit)

		w := serveJSON(t, r, "POST", "/api/torrents/H1/limit", `{"uploadLimit":"abc"}`)
		if w.Code != 400 {
			t.Fatalf("类型不匹配应返回 400，got %d body=%s", w.Code, w.Body.String())
		}
		if len(mock.limitRequests()) != 0 {
			t.Error("bind 失败时不应调用 qB")
		}
	})
}

// =============== getTorrentLimit ===============

// TestGetTorrentLimit_ConvertsBytesToKB 覆盖字节 → KB 换算与 -1（不限速）→ 0 分支
func TestGetTorrentLimit_ConvertsBytesToKB(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name string
		body string
		want float64 // 接口返回的 uploadLimit（KB）
	}{
		{"2048 B → 2 KB", `{"H1":2048}`, 2},
		{"-1（不限速）→ 0", `{"H1":-1}`, 0},
		{"0 → 0", `{"H1":0}`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := newMockQB(t)
			mock.uploadLimitBody = tc.body
			s := setupTestServerFull(t, t.TempDir(), mock.URL(), "")
			r := gin.New()
			r.GET("/api/torrents/:hash/limit", s.getTorrentLimit)

			w := serveJSON(t, r, "GET", "/api/torrents/H1/limit", "")
			if w.Code != 200 {
				t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
			}
			var data map[string]interface{}
			decodeAPI(t, w).bindData(t, &data)
			if got := numVal(t, data["uploadLimit"]); got != tc.want {
				t.Errorf("uploadLimit = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestGetTorrentLimit_FailureBranches 覆盖 qB 失败 → 500 与 hash 不在响应里 → 500
func TestGetTorrentLimit_FailureBranches(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("qB 连不上 → 接口 500", func(t *testing.T) {
		mock := newMockQB(t)
		s := setupTestServerFull(t, t.TempDir(), mock.URL(), "")
		r := gin.New()
		r.GET("/api/torrents/:hash/limit", s.getTorrentLimit)
		// 登录预热之后再关掉 qB：后续请求在传输层失败
		mock.srv.Close()

		w := serveJSON(t, r, "GET", "/api/torrents/H1/limit", "")
		if w.Code != 500 {
			t.Fatalf("qB 失败应返回 500，got %d body=%s", w.Code, w.Body.String())
		}
		resp := decodeAPI(t, w)
		if resp.Success || resp.Message == "" {
			t.Errorf("应返回 success=false + 错误消息，got %+v", resp)
		}
	})

	t.Run("响应里没有该 hash → 500", func(t *testing.T) {
		mock := newMockQB(t)
		mock.uploadLimitBody = `{"OTHER":2048}`
		s := setupTestServerFull(t, t.TempDir(), mock.URL(), "")
		r := gin.New()
		r.GET("/api/torrents/:hash/limit", s.getTorrentLimit)

		w := serveJSON(t, r, "GET", "/api/torrents/H1/limit", "")
		if w.Code != 500 {
			t.Fatalf("hash 缺失应返回 500，got %d body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("响应体非 JSON → 500", func(t *testing.T) {
		mock := newMockQB(t)
		mock.uploadLimitBody = "boom"
		s := setupTestServerFull(t, t.TempDir(), mock.URL(), "")
		r := gin.New()
		r.GET("/api/torrents/:hash/limit", s.getTorrentLimit)

		w := serveJSON(t, r, "GET", "/api/torrents/H1/limit", "")
		if w.Code != 500 {
			t.Fatalf("非 JSON 应返回 500，got %d body=%s", w.Code, w.Body.String())
		}
	})
}

// =============== RSS 系列接口 ===============

// rssFeedsForTest 返回两个禁用的 feed（ForceFetch 不会真的去外网拉取）
func rssFeedsForTest() []models.RSSFeed {
	return []models.RSSFeed{
		{ID: "f1", Name: "Feed1", URL: "http://127.0.0.1:1/feed1", Enabled: false},
		{ID: "f2", Name: "Feed2", URL: "http://127.0.0.1:1/feed2", Enabled: false},
	}
}

// TestRSSStatus_ReturnsFeeds 覆盖 rssStatus handler
func TestRSSStatus_ReturnsFeeds(t *testing.T) {
	s := setupTestServerFull(t, t.TempDir(), "", "")
	cfg := s.cfg.Get()
	cfg.RSS.Feeds = rssFeedsForTest()
	s.cfg.Set(cfg)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/rss/status", s.rssStatus)

	w := serveJSON(t, r, "GET", "/api/rss/status", "")
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	resp := decodeAPI(t, w)
	if !resp.Success {
		t.Fatalf("expected success, got %+v", resp)
	}
	var feeds []struct {
		FeedID    string `json:"feedId"`
		FeedName  string `json:"feedName"`
		SeenCount int    `json:"seenCount"`
	}
	resp.bindData(t, &feeds)
	if len(feeds) != 2 {
		t.Fatalf("应返回 2 个 feed，got %d", len(feeds))
	}
	if feeds[0].FeedID != "f1" || feeds[1].FeedID != "f2" {
		t.Errorf("feedId 顺序不对: %s, %s", feeds[0].FeedID, feeds[1].FeedID)
	}
}

// TestForceRSS_ReturnsTriggered 覆盖 forceRSS handler
func TestForceRSS_ReturnsTriggered(t *testing.T) {
	s := setupTestServerFull(t, t.TempDir(), "", "")
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/rss/force", s.forceRSS)

	w := serveJSON(t, r, "POST", "/api/rss/force", "")
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	resp := decodeAPI(t, w)
	if !resp.Success || resp.Message != "RSS fetch triggered" {
		t.Errorf("unexpected response: %+v", resp)
	}
}

// TestRSSReset_ClearsSeenState 覆盖 rssReset 的三个分支，并断言 seen 状态真的被清掉
func TestRSSReset_ClearsSeenState(t *testing.T) {
	dir := t.TempDir()
	// 先写一份 seen 状态，让 rss.New 预加载（f1 有 2 条，f2 有 1 条）
	seenPath := filepath.Join(dir, "rss_seen.json")
	if err := os.WriteFile(seenPath, []byte(`{"f1":["k1","k2"],"f2":["k3"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s := setupTestServerFull(t, dir, "", "")
	cfg := s.cfg.Get()
	cfg.RSS.Feeds = rssFeedsForTest()
	s.cfg.Set(cfg)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/rss/reset", s.rssReset)
	r.GET("/api/rss/status", s.rssStatus)

	readSeen := func() map[string][]string {
		t.Helper()
		b, err := os.ReadFile(seenPath)
		if err != nil {
			t.Fatalf("读取 seen 文件失败: %v", err)
		}
		out := map[string][]string{}
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("解析 seen 文件失败: %v, raw=%s", err, string(b))
		}
		return out
	}
	seenCounts := func() map[string]int {
		t.Helper()
		w := serveJSON(t, r, "GET", "/api/rss/status", "")
		var feeds []struct {
			FeedID    string `json:"feedId"`
			SeenCount int    `json:"seenCount"`
		}
		decodeAPI(t, w).bindData(t, &feeds)
		out := map[string]int{}
		for _, f := range feeds {
			out[f.FeedID] = f.SeenCount
		}
		return out
	}

	// 初始：f1=2, f2=1
	if got := seenCounts(); got["f1"] != 2 || got["f2"] != 1 {
		t.Fatalf("初始 seen 计数不对: %v", got)
	}

	// 1) 只重置 f1
	w := serveJSON(t, r, "POST", "/api/rss/reset", `{"feedId":"f1"}`)
	if w.Code != 200 {
		t.Fatalf("reset f1: expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if resp := decodeAPI(t, w); !resp.Success || resp.Message != "RSS seen reset, fetch triggered" {
		t.Errorf("unexpected response: %+v", resp)
	}
	if got := seenCounts(); got["f1"] != 0 || got["f2"] != 1 {
		t.Errorf("重置 f1 后计数应 f1=0 f2=1，got %v", got)
	}
	diskAfterReset := readSeen()
	if _, stillThere := diskAfterReset["f1"]; stillThere {
		t.Errorf("f1 应已从 seen 文件移除，got %v", diskAfterReset)
	}

	// 2) feedId 为空 → 重置全部
	w = serveJSON(t, r, "POST", "/api/rss/reset", `{"feedId":""}`)
	if w.Code != 200 {
		t.Fatalf("reset all: expected 200, got %d", w.Code)
	}
	if got := seenCounts(); got["f1"] != 0 || got["f2"] != 0 {
		t.Errorf("全部重置后计数应为 0，got %v", got)
	}
	if disk := readSeen(); len(disk) != 0 {
		t.Errorf("全部重置后 seen 文件应为空，got %v", disk)
	}

	// 3) 非法 body → 400
	w = serveJSON(t, r, "POST", "/api/rss/reset", "not-json")
	if w.Code != 400 {
		t.Errorf("非法 JSON 应返回 400，got %d", w.Code)
	}
}

// =============== testNotify ===============

// newNotifyMock 启动一个"通知上游" mock，返回它的 json:// URL 和读取最后一条请求体的函数
func newNotifyMock(t *testing.T, status int) (string, func() string) {
	t.Helper()
	var lastBody string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		lastBody = string(b)
		mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	get := func() string {
		mu.Lock()
		defer mu.Unlock()
		return lastBody
	}
	return "json://" + strings.TrimPrefix(srv.URL, "http://") + "/notify", get
}

// TestTestNotify_Branches 覆盖 testNotify 的 400 / success / 上游失败三个分支
func TestTestNotify_Branches(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("非法 JSON → 400", func(t *testing.T) {
		s := setupTestServerFull(t, t.TempDir(), "", "")
		r := gin.New()
		r.POST("/api/notify/test", s.testNotify)
		w := serveJSON(t, r, "POST", "/api/notify/test", "not-json")
		if w.Code != 400 {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})

	t.Run("未启用通知 → 400", func(t *testing.T) {
		s := setupTestServerFull(t, t.TempDir(), "", "")
		r := gin.New()
		r.POST("/api/notify/test", s.testNotify)
		w := serveJSON(t, r, "POST", "/api/notify/test", `{"enabled":false,"appriseUrls":["json://127.0.0.1:1/x"]}`)
		if w.Code != 400 {
			t.Errorf("expected 400, got %d", w.Code)
		}
		resp := decodeAPI(t, w)
		if !strings.Contains(resp.Message, "通知渠道未配置") {
			t.Errorf("message = %q", resp.Message)
		}
	})

	t.Run("通知渠道为空 → 400", func(t *testing.T) {
		s := setupTestServerFull(t, t.TempDir(), "", "")
		r := gin.New()
		r.POST("/api/notify/test", s.testNotify)
		w := serveJSON(t, r, "POST", "/api/notify/test", `{"enabled":true,"appriseUrls":[]}`)
		if w.Code != 400 {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})

	t.Run("上游返回 200 → 发送成功", func(t *testing.T) {
		notifyURL, getLast := newNotifyMock(t, http.StatusOK)
		s := setupTestServerFull(t, t.TempDir(), "", "")
		r := gin.New()
		r.POST("/api/notify/test", s.testNotify)

		body := `{"enabled":true,"appriseUrls":["` + notifyURL + `"]}`
		w := serveJSON(t, r, "POST", "/api/notify/test", body)
		if w.Code != 200 {
			t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
		}
		resp := decodeAPI(t, w)
		if !resp.Success {
			t.Fatalf("通知应发送成功，got %+v", resp)
		}
		if resp.Message != "测试通知已发送" {
			t.Errorf("message = %q", resp.Message)
		}
		sent := getLast()
		if !strings.Contains(sent, "QBHive 通知测试") {
			t.Errorf("上游应收到测试通知标题，got body=%q", sent)
		}
	})

	t.Run("上游返回 500 → success=false", func(t *testing.T) {
		notifyURL, _ := newNotifyMock(t, http.StatusInternalServerError)
		s := setupTestServerFull(t, t.TempDir(), "", "")
		r := gin.New()
		r.POST("/api/notify/test", s.testNotify)

		body := `{"enabled":true,"appriseUrls":["` + notifyURL + `"]}`
		w := serveJSON(t, r, "POST", "/api/notify/test", body)
		if w.Code != 200 {
			t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
		}
		resp := decodeAPI(t, w)
		if resp.Success {
			t.Fatal("上游 500 时应 success=false")
		}
		if resp.Message == "" {
			t.Error("应携带错误消息")
		}
	})
}

// =============== RandomID ===============

// TestRandomID 覆盖 RandomID 的正常路径
func TestRandomID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		id := RandomID()
		if len(id) != 16 {
			t.Fatalf("RandomID 长度应为 16，got %d (%q)", len(id), id)
		}
		if _, err := hex.DecodeString(id); err != nil {
			t.Fatalf("RandomID 应为 hex: %v (%q)", err, id)
		}
		if seen[id] {
			t.Fatalf("RandomID 碰撞: %q", id)
		}
		seen[id] = true
	}
}
