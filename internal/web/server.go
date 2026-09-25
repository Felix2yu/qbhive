package web

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"context"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/Felix2yu/qbhive/internal/config"
	"github.com/Felix2yu/qbhive/internal/limiter"
	"github.com/Felix2yu/qbhive/internal/models"
	"github.com/Felix2yu/qbhive/internal/notifier"
	qb "github.com/Felix2yu/qbhive/internal/qbittorrent"
	"github.com/Felix2yu/qbhive/internal/rss"
	"github.com/Felix2yu/qbhive/internal/scheduler"

	"github.com/gin-contrib/static"
	"github.com/gin-gonic/gin"
)

type Server struct {
	cfg       *config.Manager
	qbClient  *qb.Client
	limiter   *limiter.Limiter
	rssEngine *rss.Engine
	notifier  *notifier.Notifier
	scheduler *scheduler.Scheduler
	httpSrv   *http.Server

	// 鉴权 token（从环境变量 QBHIVE_TOKEN 读取；空则不鉴权，向后兼容）
	authToken string
}

func New(cfg *config.Manager, qbClient *qb.Client, lim *limiter.Limiter, rssEngine *rss.Engine, not *notifier.Notifier, sch *scheduler.Scheduler) *Server {
	token := strings.TrimSpace(os.Getenv("QBHIVE_TOKEN"))
	if token != "" {
		fmt.Println("[qbhive] Web auth enabled (QBHIVE_TOKEN set); UI will require login")
	}
	return &Server{
		cfg:       cfg,
		qbClient:  qbClient,
		limiter:   lim,
		rssEngine: rssEngine,
		notifier:  not,
		scheduler: sch,
		authToken: token,
	}
}

// authMiddleware 根据 QBHIVE_TOKEN 校验请求。
// 支持三种方式：Authorization: Bearer <token>、Cookie: qbhive_token=<token>、?token=<token>
func (s *Server) authMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.authToken == "" {
			c.Next()
			return
		}
		// /api/ping /api/login /api/logout /api/auth/status 放行
		p := c.Request.URL.Path
		if p == "/api/ping" || p == "/api/login" || p == "/api/logout" || p == "/api/auth/status" {
			c.Next()
			return
		}
		var provided string
		if h := c.GetHeader("Authorization"); h != "" {
			provided = strings.TrimPrefix(h, "Bearer ")
		}
		if provided == "" {
			if cookie, err := c.Cookie("qbhive_token"); err == nil {
				provided = cookie
			}
		}
		if provided == "" {
			provided = c.Query("token")
		}
		if provided != s.authToken {
			c.AbortWithStatusJSON(http.StatusUnauthorized, models.APIResponse{
				Success: false, Message: "unauthorized", Data: map[string]bool{"auth": true},
			})
			return
		}
		c.Next()
	}
}

func (s *Server) Start(webRoot string) error {
	r := gin.Default()
	r.Use(s.authMiddleware())

	api := r.Group("/api")
	{
		api.GET("/ping", func(c *gin.Context) {
			c.JSON(200, models.APIResponse{Success: true, Message: "pong"})
		})

		api.POST("/login", s.login)
		api.POST("/logout", s.logout)
		api.GET("/auth/status", func(c *gin.Context) {
			c.JSON(200, models.APIResponse{
				Success: true,
				Data:    map[string]bool{"auth_required": s.authToken != ""},
			})
		})

		api.GET("/config", s.getConfig)
		api.POST("/config", s.saveConfig)
		api.POST("/test-qb", s.testQB)

		api.GET("/torrents", s.listTorrents)
		api.POST("/torrents/:hash/limit", s.setTorrentLimit)

		api.POST("/rss/force", s.forceRSS)

		api.POST("/notify/test", s.testNotify)
	}

	if webRoot != "" {
		r.Use(static.Serve("/", static.LocalFile(webRoot, true)))
	}

	s.httpSrv = &http.Server{
		Addr:    s.cfg.Get().Server.Listen,
		Handler: r,
	}
	return s.httpSrv.ListenAndServe()
}

func (s *Server) Shutdown() error {
	if s.httpSrv != nil {
		return s.httpSrv.Shutdown(context.Background())
	}
	return nil
}

func (s *Server) login(c *gin.Context) {
	if s.authToken == "" {
		c.JSON(200, models.APIResponse{Success: true, Message: "auth disabled"})
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, models.APIResponse{Success: false, Message: err.Error()})
		return
	}
	if body.Token != s.authToken {
		c.JSON(401, models.APIResponse{Success: false, Message: "invalid token"})
		return
	}
	c.SetCookie("qbhive_token", s.authToken, 7*24*60*60, "/", "", false, false)
	c.JSON(200, models.APIResponse{Success: true})
}

func (s *Server) logout(c *gin.Context) {
	c.SetCookie("qbhive_token", "", -1, "/", "", false, false)
	c.JSON(200, models.APIResponse{Success: true})
}

func (s *Server) getConfig(c *gin.Context) {
	cfg := s.cfg.Get()
	if cfg.Qbittorrent.Password != "" {
		cfg.Qbittorrent.Password = "********"
	}
	if cfg.Qbittorrent.APIKey != "" {
		cfg.Qbittorrent.APIKey = "********"
	}
	// Apprise URLs 通常含 Bot Token / Webhook Secret，同样掩码
	if len(cfg.Notifier.AppriseURLs) > 0 {
		masked := make([]string, len(cfg.Notifier.AppriseURLs))
		for i, u := range cfg.Notifier.AppriseURLs {
			masked[i] = maskAppriseURL(u)
		}
		cfg.Notifier.AppriseURLs = masked
	}
	c.JSON(200, models.APIResponse{Success: true, Data: cfg})
}

func validateConfig(in *models.AppConfig) string {
	// URL 格式（qB 连接目标）
	if strings.TrimSpace(in.Qbittorrent.URL) != "" {
		if !(strings.HasPrefix(in.Qbittorrent.URL, "http://") || strings.HasPrefix(in.Qbittorrent.URL, "https://")) {
			return "qBittorrent URL 必须以 http:// 或 https:// 开头"
		}
	}
	// 间隔字段 >= 1
	if in.RSS.Interval < 1 && in.RSS.Enabled {
		return "RSS 刷新间隔必须 ≥ 1 分钟"
	}
	if in.Limiter.Interval < 1 && in.Limiter.Enabled {
		return "限速器刷新间隔必须 ≥ 1 秒"
	}
	if in.FileManager.ScanInterval < 1 && in.FileManager.Enabled {
		return "文件管理扫描间隔必须 ≥ 1 秒"
	}
	// Apprise URL 协议前缀
	for _, u := range in.Notifier.AppriseURLs {
		if strings.TrimSpace(u) == "" { continue }
		if !strings.Contains(u, "://") {
			return fmt.Sprintf("Apprise URL 缺少协议前缀: %s", u)
		}
	}
	// RSS 规则正则编译（仅 regex 模式校验）
	for _, feed := range in.RSS.Feeds {
		if strings.TrimSpace(feed.URL) != "" && !strings.HasPrefix(feed.URL, "http") {
			return fmt.Sprintf("RSS 订阅源 URL 必须以 http 开头: %s", feed.Name)
		}
		for _, rule := range feed.Rules {
			if rule.Mode == "regex" {
				if strings.TrimSpace(rule.Include) != "" {
					if _, err := regexp.Compile(rule.Include); err != nil {
						return fmt.Sprintf("订阅源 %s / 规则 %s 的 include 正则无效: %v", feed.Name, rule.Name, err)
					}
				}
				if strings.TrimSpace(rule.Exclude) != "" {
					if _, err := regexp.Compile(rule.Exclude); err != nil {
						return fmt.Sprintf("订阅源 %s / 规则 %s 的 exclude 正则无效: %v", feed.Name, rule.Name, err)
					}
				}
			}
		}
	}
	// 限速规则正则编译
	for _, r := range in.Limiter.Rules {
		if strings.TrimSpace(r.Match) != "" {
			if _, err := regexp.Compile(r.Match); err != nil {
				return fmt.Sprintf("限速规则 %s 的 match 正则无效: %v", r.Name, err)
			}
		}
	}
	return ""
}

func maskAppriseURL(u string) string {
	if u == "" { return u }
	if idx := strings.Index(u, "://"); idx > 0 {
		prefix := u[:idx+3]
		rest := u[idx+3:]
		if len(rest) > 6 { return prefix + rest[:3] + "...********" }
		return prefix + "...********"
	}
	if len(u) > 6 { return u[:3] + "...********" }
	return "...********"
}

func (s *Server) saveConfig(c *gin.Context) {
	var in models.AppConfig
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(400, models.APIResponse{Success: false, Message: err.Error()})
		return
	}
	// P1: 后端校验——在任何持久化/热生效之前拦截非法输入
	if msg := validateConfig(&in); msg != "" {
		c.JSON(400, models.APIResponse{Success: false, Message: msg})
		return
	}
	cur := s.cfg.Get()
	if strings.TrimSpace(in.Qbittorrent.Password) == "" || in.Qbittorrent.Password == "********" {
		in.Qbittorrent.Password = cur.Qbittorrent.Password
	}
	if strings.TrimSpace(in.Qbittorrent.APIKey) == "" || in.Qbittorrent.APIKey == "********" {
		in.Qbittorrent.APIKey = cur.Qbittorrent.APIKey
	}
	s.cfg.Set(in)
	if err := s.cfg.Save(); err != nil {
		c.JSON(500, models.APIResponse{Success: false, Message: err.Error()})
		return
	}
	// P0-3 热更新所有后台模块
	if s.scheduler != nil {
		s.scheduler.ReloadAll(in.Qbittorrent)
	}
	s.notifier.Reload(in.Notifier)
	c.JSON(200, models.APIResponse{Success: true})
}

func (s *Server) testQB(c *gin.Context) {
	var in models.QBConfig
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(400, models.APIResponse{Success: false, Message: err.Error()})
		return
	}
	// 掩码回退：前端保持默认掩码值/空值时，沿用当前已保存的真实凭据
	cur := s.cfg.Get()
	if strings.TrimSpace(in.Password) == "" || in.Password == "********" {
		in.Password = cur.Qbittorrent.Password
	}
	if strings.TrimSpace(in.APIKey) == "" || in.APIKey == "********" {
		in.APIKey = cur.Qbittorrent.APIKey
	}
	cli := qb.New(in.URL, in.Username, in.Password, in.APIKey)
	if err := cli.TestConnection(); err != nil {
		c.JSON(200, models.APIResponse{Success: false, Message: err.Error()})
		return
	}
	c.JSON(200, models.APIResponse{Success: true})
}

func (s *Server) listTorrents(c *gin.Context) {
	list, err := s.qbClient.GetTorrents()
	if err != nil {
		c.JSON(502, models.APIResponse{Success: false, Message: err.Error()})
		return
	}
	c.JSON(200, models.APIResponse{Success: true, Data: list})
}

type limitPayload struct {
	UploadLimit int `json:"uploadLimit"`
}

func (s *Server) setTorrentLimit(c *gin.Context) {
	hash := c.Param("hash")
	var p limitPayload
	if err := c.ShouldBindJSON(&p); err != nil {
		c.JSON(400, models.APIResponse{Success: false, Message: err.Error()})
		return
	}
	var limit int64
	if p.UploadLimit == 0 {
		limit = -1
	} else {
		limit = int64(p.UploadLimit) * 1024
	}
	if err := s.qbClient.SetUploadLimit(hash, limit); err != nil {
		c.JSON(500, models.APIResponse{Success: false, Message: err.Error()})
		return
	}
	c.JSON(200, models.APIResponse{Success: true})
}

func (s *Server) forceRSS(c *gin.Context) {
	s.rssEngine.ForceFetch()
	c.JSON(200, models.APIResponse{Success: true, Message: "RSS fetch triggered"})
}

func (s *Server) testNotify(c *gin.Context) {
	var cfg models.NotifierConfig
	if err := c.ShouldBindJSON(&cfg); err != nil {
		c.JSON(400, models.APIResponse{Success: false, Message: err.Error()})
		return
	}
	if !cfg.Enabled || len(cfg.AppriseURLs) == 0 {
		c.JSON(400, models.APIResponse{Success: false, Message: "通知渠道未配置"})
		return
	}
	tmp := notifier.New(cfg)
	if err := tmp.Notify("QBHive 通知测试", "这是一条来自 QBHive 的测试通知 🎉\n如果你看到了，说明通知已配置成功。"); err != nil {
		c.JSON(200, models.APIResponse{Success: false, Message: err.Error()})
		return
	}
	c.JSON(200, models.APIResponse{Success: true, Message: "测试通知已发送"})
}

// RandomID 已由前端 genID() 取代（前端直接生成），保留后端版本供可能的未来扩展。
func RandomID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// 极端情况：crypto/rand 无法读取时 fallback 为零值哈希前缀，保证返回有效长度
		return "0000000000000000"
	}
	return hex.EncodeToString(b)
}
