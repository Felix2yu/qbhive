package web

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"

	"github.com/Felix2yu/qbhive/internal/config"
	"github.com/Felix2yu/qbhive/internal/limiter"
	"github.com/Felix2yu/qbhive/internal/models"
	"github.com/Felix2yu/qbhive/internal/notifier"
	qb "github.com/Felix2yu/qbhive/internal/qbittorrent"
	"github.com/Felix2yu/qbhive/internal/rss"

	"github.com/gin-contrib/static"
	"github.com/gin-gonic/gin"
)

type Server struct {
	cfg       *config.Manager
	qbClient  *qb.Client
	limiter   *limiter.Limiter
	rssEngine *rss.Engine
	notifier  *notifier.Notifier
	httpSrv   *http.Server
}

func New(cfg *config.Manager, qbClient *qb.Client, lim *limiter.Limiter, rssEngine *rss.Engine, not *notifier.Notifier) *Server {
	return &Server{cfg: cfg, qbClient: qbClient, limiter: lim, rssEngine: rssEngine, notifier: not}
}

func (s *Server) Start(webRoot string) error {
	r := gin.Default()

	api := r.Group("/api")
	{
		api.GET("/ping", func(c *gin.Context) {
			c.JSON(200, models.APIResponse{Success: true, Message: "pong"})
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
		return s.httpSrv.Shutdown(nil)
	}
	return nil
}

func (s *Server) getConfig(c *gin.Context) {
	cfg := s.cfg.Get()
	if cfg.Qbittorrent.Password != "" {
		cfg.Qbittorrent.Password = "********"
	}
	if cfg.Qbittorrent.APIKey != "" {
		cfg.Qbittorrent.APIKey = "********"
	}
	c.JSON(200, models.APIResponse{Success: true, Data: cfg})
}

func (s *Server) saveConfig(c *gin.Context) {
	var in models.AppConfig
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(400, models.APIResponse{Success: false, Message: err.Error()})
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
	s.qbClient = qb.New(in.Qbittorrent.URL, in.Qbittorrent.Username, in.Qbittorrent.Password, in.Qbittorrent.APIKey)
	s.notifier.Reload(in.Notifier)
	c.JSON(200, models.APIResponse{Success: true})
}

func (s *Server) testQB(c *gin.Context) {
	var in models.QBConfig
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(400, models.APIResponse{Success: false, Message: err.Error()})
		return
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

func RandomID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

var _ = fmt.Errorf
