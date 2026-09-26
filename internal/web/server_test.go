package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Felix2yu/qbhive/internal/config"
	"github.com/Felix2yu/qbhive/internal/models"
	"github.com/Felix2yu/qbhive/internal/notifier"
	qb "github.com/Felix2yu/qbhive/internal/qbittorrent"
	"github.com/gin-gonic/gin"
)

// =============== pure functions (validateConfig + maskAppriseURL) ===============

func TestMaskAppriseURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"http://short", "http://...********"},
		{"http://long-secret-token", "http://lon...********"},
		{"tgram://bottoken/ChatID", "tgram://bot...********"},
		{"no-scheme-short", "no-...********"},
		{"abc", "...********"},
		{"1234567890abcd", "123...********"},
	}
	for _, c := range cases {
		got := maskAppriseURL(c.in)
		if got != c.want {
			t.Errorf("maskAppriseURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func emptyAppCfg() *models.AppConfig {
	return &models.AppConfig{
		Qbittorrent: models.QBConfig{URL: "http://127.0.0.1:8080"},
		RSS: models.RSSConfig{
			Enabled:  true, Interval: 15,
			Feeds: []models.RSSFeed{
				{ID: "f1", Name: "feed", URL: "https://example.com/feed.xml", Enabled: true},
			},
		},
		Limiter: models.LimiterConfig{
			Enabled: true, Interval: 10,
			Rules: []models.LimitRule{
				{ID: "r1", Name: "rule", Enabled: true, Match: ".*\\.mkv$"},
			},
		},
		FileManager: models.FileManagerConfig{Enabled: true, ScanInterval: 60},
		Notifier:    models.NotifierConfig{Enabled: true, AppriseURLs: []string{"tgram://abc/123"}},
	}
}

func TestValidateConfig_HappyPath(t *testing.T) {
	if msg := validateConfig(emptyAppCfg()); msg != "" {
		t.Errorf("expected empty on valid config, got: %s", msg)
	}
}

func TestValidateConfig_QBUrlMustBeHTTP(t *testing.T) {
	cfg := emptyAppCfg(); cfg.Qbittorrent.URL = "ftp://x"
	if !strings.Contains(validateConfig(cfg), "http:// 或 https://") {
		t.Error("expected QB URL error")
	}
	cfg.Qbittorrent.URL = "https://qb.example.com:8080"
	if validateConfig(cfg) != "" {
		t.Error("https URL should pass")
	}
	cfg.Qbittorrent.URL = ""
	if validateConfig(cfg) != "" {
		t.Error("empty QB URL should pass (unconfigured)")
	}
}

func TestValidateConfig_IntervalZeroChecks(t *testing.T) {
	cfg := emptyAppCfg()
	cfg.RSS.Interval = 0
	if !strings.Contains(validateConfig(cfg), "RSS 刷新间隔必须 ≥ 1") {
		t.Error("expected RSS interval error")
	}
	cfg.RSS.Enabled = false; cfg.RSS.Interval = 0
	if validateConfig(cfg) != "" {
		t.Error("disabled RSS should skip interval check")
	}

	cfg = emptyAppCfg(); cfg.Limiter.Interval = 0
	if !strings.Contains(validateConfig(cfg), "限速器刷新间隔必须 ≥ 1") {
		t.Error("expected limiter interval error")
	}

	cfg = emptyAppCfg(); cfg.FileManager.ScanInterval = 0
	if !strings.Contains(validateConfig(cfg), "文件管理扫描间隔必须 ≥ 1") {
		t.Error("expected filemanager interval error")
	}
}

func TestValidateConfig_AppriseURLMissingScheme(t *testing.T) {
	cfg := emptyAppCfg()
	cfg.Notifier.AppriseURLs = []string{"tgram://ok", "missing-scheme", ""}
	if !strings.Contains(validateConfig(cfg), "缺少协议前缀") {
		t.Error("expected apprise scheme error")
	}
}

// 覆盖 validateConfig 里 "空串 continue" 分支
func TestValidateConfig_AppriseURLEmptyIsSkipped(t *testing.T) {
	cfg := emptyAppCfg()
	cfg.Notifier.AppriseURLs = []string{"", "tgram://ok"}
	if validateConfig(cfg) != "" {
		t.Errorf("empty should be skipped, got: %s", validateConfig(cfg))
	}
}

func TestValidateConfig_RegexCompilation(t *testing.T) {
	cfg := emptyAppCfg()

	cfg.RSS.Feeds[0].URL = "ftp://feed"
	if !strings.Contains(validateConfig(cfg), "RSS 订阅源 URL 必须以 http") {
		t.Error("expected feed URL error")
	}

	cfg = emptyAppCfg()
	cfg.RSS.Feeds[0].Rules = []models.RSSRule{{ID: "r1", Name: "bad", Mode: "regex", Include: "["}}
	if !strings.Contains(validateConfig(cfg), "include 正则无效") {
		t.Error("expected include regex error")
	}

	cfg = emptyAppCfg()
	cfg.RSS.Feeds[0].Rules = []models.RSSRule{{ID: "r1", Name: "bad", Mode: "regex", Include: "ok", Exclude: "("}}
	if !strings.Contains(validateConfig(cfg), "exclude 正则无效") {
		t.Error("expected exclude regex error")
	}

	cfg = emptyAppCfg()
	cfg.Limiter.Rules = []models.LimitRule{{ID: "r1", Name: "bad", Match: "*"}}
	if !strings.Contains(validateConfig(cfg), "match 正则无效") {
		t.Error("expected limiter match regex error")
	}

	cfg = emptyAppCfg()
	cfg.RSS.Feeds[0].Rules = []models.RSSRule{{ID: "r1", Name: "kw", Mode: "keyword", Include: "[invalid"}}
	if validateConfig(cfg) != "" {
		t.Error("keyword mode should skip regex compile")
	}
}

// =============== httptest handler tests (B 层) ===============

// setupTestServer: 返回一个带真实 config.Manager + 可选 httptest qB mock 的 Server
func setupTestServer(t *testing.T, qbURL string, authToken string) *Server {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	cfg := config.New(cfgPath)
	cfg.Set(models.AppConfig{
		Qbittorrent: models.QBConfig{URL: qbURL, Username: "u", Password: "p"},
	})
	var qbCli *qb.Client
	if qbURL != "" {
		qbCli = qb.New(qbURL, "u", "p", "")
	} else {
		qbCli = qb.New("http://localhost", "", "", "")
	}
	srv := New(cfg, qbCli, nil, nil, notifier.New(models.NotifierConfig{Enabled: false}), nil)
	srv.authToken = authToken // 直接设，绕过 env 依赖
	// 不手动 make tcache：必须依赖 New() 的初始化，
	// 否则会掩盖 "nil map 写 panic -> listTorrents 500" 这类回归
	return srv
}


func TestAuthMiddleware_NoTokenAllowsAll(t *testing.T) {
	s := setupTestServer(t, "", "")
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(s.authMiddleware())
	r.GET("/api/ping", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/ping", nil)
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Errorf("no token set -> should allow, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestAuthMiddleware_WithToken_Bearer(t *testing.T) {
	s := setupTestServer(t, "", "secret123")
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(s.authMiddleware())
	r.GET("/api/config", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	// 无 token → 401
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/config", nil)
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Errorf("expected 401, got %d", w.Code)
	}

	// 正确 Bearer token → 200
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/api/config", nil)
	req.Header.Set("Authorization", "Bearer secret123")
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Errorf("expected 200 with correct token, got %d", w.Code)
	}

	// 错误 token → 401
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/api/config", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Errorf("expected 401 with wrong token, got %d", w.Code)
	}
}

func TestAuthMiddleware_WithToken_Cookie(t *testing.T) {
	s := setupTestServer(t, "", "secret123")
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(s.authMiddleware())
	r.GET("/api/config", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/config", nil)
	req.AddCookie(&http.Cookie{Name: "qbhive_token", Value: "secret123"})
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Errorf("expected 200 with cookie token, got %d", w.Code)
	}
}

func TestAuthMiddleware_WithToken_QueryParam(t *testing.T) {
	s := setupTestServer(t, "", "secret123")
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(s.authMiddleware())
	r.GET("/api/config", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/config?token=secret123", nil)
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Errorf("expected 200 with query token, got %d", w.Code)
	}
}

func TestAuthMiddleware_ExemptRoutes(t *testing.T) {
	s := setupTestServer(t, "", "secret123")
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(s.authMiddleware())
	r.GET("/api/ping", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
	r.POST("/api/login", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
	r.POST("/api/logout", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
	r.GET("/api/auth/status", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	for _, path := range []string{"/api/ping", "/api/login", "/api/logout", "/api/auth/status"} {
		w := httptest.NewRecorder()
		var req *http.Request
		if path == "/api/login" || path == "/api/logout" {
			req, _ = http.NewRequest("POST", path, nil)
		} else {
			req, _ = http.NewRequest("GET", path, nil)
		}
		r.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Errorf("path %s should be exempt from auth, got %d", path, w.Code)
		}
	}
}

func TestGetConfig_MasksSecrets(t *testing.T) {
	s := setupTestServer(t, "", "")
	s.cfg.Set(models.AppConfig{
		Qbittorrent: models.QBConfig{URL: "http://x:8080", Password: "realpass", APIKey: "realkey"},
		Notifier:    models.NotifierConfig{AppriseURLs: []string{"tgram://bottoken/secretchat"}},
	})
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/config", s.getConfig)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/config", nil)
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("getConfig failed: %d %s", w.Code, w.Body.String())
	}
	var resp models.APIResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	b, _ := json.Marshal(resp.Data)
	if strings.Contains(string(b), "realpass") || strings.Contains(string(b), "realkey") {
		t.Error("password/apiKey should be masked in response")
	}
	if strings.Contains(string(b), "bottoken") {
		t.Error("apprise URL secret should be masked")
	}
}

func TestSaveConfig_ValidConfigPersists(t *testing.T) {
	s := setupTestServer(t, "", "")
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/config", s.saveConfig)

	body := `{"qbittorrent":{"url":"http://new.example.com:8080","username":"u","password":"p"},"rss":{"enabled":true,"interval":15,"feeds":[]},"limiter":{"enabled":true,"interval":10,"rules":[]},"fileManager":{"enabled":false},"notifier":{"enabled":false}}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("saveConfig expected 200, got %d %s", w.Code, w.Body.String())
	}

	// 验证 cfg 实际更新了
	got := s.cfg.Get()
	if got.Qbittorrent.URL != "http://new.example.com:8080" {
		t.Errorf("cfg URL not updated: %q", got.Qbittorrent.URL)
	}
}

func TestSaveConfig_InvalidRejectsWith400(t *testing.T) {
	s := setupTestServer(t, "", "")
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/config", s.saveConfig)

	// 非法 URL
	body := `{"qbittorrent":{"url":"ftp://bad"},"rss":{"enabled":true,"interval":0,"feeds":[]},"limiter":{"enabled":true,"interval":10,"rules":[]},"fileManager":{"enabled":false},"notifier":{"enabled":false}}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expected 400, got %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "http:// 或 https://") {
		t.Error("error msg should mention http://")
	}
}

func TestSaveConfig_PreservesMaskedPassword(t *testing.T) {
	s := setupTestServer(t, "", "")
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/config", s.saveConfig)

	// 先存一个真实密码
	s.cfg.Set(models.AppConfig{Qbittorrent: models.QBConfig{URL: "http://x", Password: "original-secret", APIKey: "original-key"}})
	_ = s.cfg.Save()

	// 提交掩码形式（前端从 getConfig 拿到的）
	body := `{"qbittorrent":{"url":"http://x","username":"u","password":"********","apiKey":"********"},"rss":{"enabled":true,"interval":15,"feeds":[]},"limiter":{"enabled":true,"interval":10,"rules":[]},"fileManager":{"enabled":false},"notifier":{"enabled":false}}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	got := s.cfg.Get()
	if got.Qbittorrent.Password != "original-secret" || got.Qbittorrent.APIKey != "original-key" {
		t.Errorf("masked password should preserve original, got pwd=%q key=%q", got.Qbittorrent.Password, got.Qbittorrent.APIKey)
	}
}

// listTorrents + torrentsStats 需要 httptest mock qB
func TestListTorrents_HappyPath(t *testing.T) {
	mockQB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-bittorrent")
		if r.URL.Path == "/api/v2/auth/login" {
			w.Write([]byte("Ok."))
			return
		}
		w.Write([]byte(`[{"hash":"aaa","name":"Movie.mkv","state":"downloading","progress":0.5,"size":1073741824}]`))
	}))
	defer mockQB.Close()

	s := setupTestServer(t, mockQB.URL, "")
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/torrents", s.listTorrents)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/torrents?filter=active", nil)
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("listTorrents got %d: %s", w.Code, w.Body.String())
	}
	var resp models.APIResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	list, ok := resp.Data.([]interface{})
	if !ok || len(list) != 1 {
		t.Errorf("expected 1 torrent, got %v", resp.Data)
	}
}

func TestTorrentsStats_HappyPath(t *testing.T) {
	mockQB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-bittorrent")
		if r.URL.Path == "/api/v2/auth/login" {
			w.Write([]byte("Ok."))
			return
		}
		if r.URL.Path == "/api/v2/transfer/info" {
			w.Write([]byte(`{"dl_info_speed":1024,"up_info_speed":2048}`))
		} else {
			w.Write([]byte(`[{"state":"active"},{"state":"active"},{"state":"stoppedUP"}]`))
		}
	}))
	defer mockQB.Close()

	s := setupTestServer(t, mockQB.URL, "")
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/torrents/stats", s.torrentsStats)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/torrents/stats", nil)
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("torrentsStats got %d: %s", w.Code, w.Body.String())
	}
}

func TestLoginLogout_NoTokenSkips(t *testing.T) {
	s := setupTestServer(t, "", "")
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/login", s.login)
	r.POST("/api/logout", s.logout)

	// login 无 auth 时返回 disabled
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/login", strings.NewReader(`{"token":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "auth disabled") {
		t.Errorf("expected disabled message, got %s", w.Body.String())
	}
}

func TestLoginLogout_WithToken(t *testing.T) {
	s := setupTestServer(t, "", "mysecret")
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/login", s.login)
	r.POST("/api/logout", s.logout)

	// 错误 token
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/login", strings.NewReader(`{"token":"wrong"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Errorf("wrong token expected 401, got %d", w.Code)
	}

	// 正确 token
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("POST", "/api/login", strings.NewReader(`{"token":"mysecret"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Errorf("correct token expected 200, got %d %s", w.Code, w.Body.String())
	}

	// logout
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("POST", "/api/logout", nil)
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Errorf("logout expected 200, got %d", w.Code)
	}
}
