package web

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

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

	// torrents 缓存：key = "filter|sort|reverse"，value = cachedResult
	tcacheMu sync.Mutex
	tcache   map[string]*cacheEntry

	// stats 缓存（概览用，刷新频率更高）
	scacheMu sync.Mutex
	scache   *cacheEntry
}

type cacheEntry struct {
	data      interface{}
	expiresAt time.Time
}

const (
	torrentCacheTTL = 5 * time.Second  // 任务列表缓存 5s（qb info 接口慢，重复请求挡掉）
	statsCacheTTL   = 3 * time.Second  // stats 缓存 3s（概览 8s 刷一次，3s 足够）
	defaultLimit    = 500              // 硬上限：默认返回 top 500，避免全量
	maxLimit        = 2000             // 允许的最大 limit（全量要显式 limit=0）
)

func New(cfg *config.Manager, qbClient *qb.Client, lim *limiter.Limiter, rssEngine *rss.Engine, not *notifier.Notifier) *Server {
	return &Server{
		cfg:       cfg,
		qbClient:  qbClient,
		limiter:   lim,
		rssEngine: rssEngine,
		notifier:  not,
		tcache:    make(map[string]*cacheEntry),
	}
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
		api.GET("/torrents/stats", s.torrentsStats) // 轻量统计，概览页用
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
	// 配置变更时清空缓存（qb client 可能换了，旧缓存没用）
	s.tcacheMu.Lock()
	s.tcache = make(map[string]*cacheEntry)
	s.tcacheMu.Unlock()
	s.scacheMu.Lock()
	s.scache = nil
	s.scacheMu.Unlock()
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

// 预定义的窄 filter：比 raw qBittorrent filter 更贴近用户想看的场景
var narrowFilters = map[string]string{
	"active":      "active",       // 正在工作的（下载+做种，有速度）
	"downloading": "downloading",  // 下载中
	"seeding":     "seeding",      // 做种中（有速度）
	"pausedDL":    "pausedDL",     // 暂停下载（未完成）
	"pausedUP":    "pausedUP",     // 暂停做种（已完成，用户最常看的「历史任务」）
	"stalledUP":   "stalledUP",    // 做种停滞（已完成但没速度）
	"completed":   "completed",    // 所有已完成的（pausedUP + stalledUP + uploading）
	"all":         "all",
}

// listTorrents 是前端任务列表的主入口。核心优化点：
//   1. 默认 filter=active（活跃任务，几十条），避开历史数千条
//   2. 默认 limit=500（后端硬上限），limit=0 才允许全量（会加 warning header）
//   3. 透传 sort/reverse 给 qBittorrent 原生排序，省 CPU
//   4. 5 秒缓存同 filter+sort+reverse 的请求，自动刷新加防抖挡重复
func (s *Server) listTorrents(c *gin.Context) {
	filter := c.Query("filter")
	if filter == "" {
		filter = "active"
	}
	// 把前端友好的别名转成 qBittorrent 原生 filter
	if raw, ok := narrowFilters[filter]; ok {
		filter = raw
	}

	sortField := c.Query("sort")
	if sortField == "" {
		sortField = "added_time" // 默认按添加时间倒序，新任务排前
	}
	reverse := c.Query("reverse")
	if reverse == "" {
		reverse = "true"
	}

	// limit 处理：默认 500，最大 2000，0 表示全量（加 warning）
	var limit int
	limitStr := c.Query("limit")
	if limitStr == "" {
		limit = defaultLimit
	} else if n, e := strconv.Atoi(limitStr); e == nil {
		switch {
		case n == 0:
			// 显式要全量
			c.Header("X-Warning", "full list requested, may be slow with thousands of torrents")
		case n > 0 && n <= maxLimit:
			limit = n
		case n > maxLimit:
			limit = maxLimit
			c.Header("X-Warning", fmt.Sprintf("limit capped at %d", maxLimit))
		default:
			limit = defaultLimit
		}
	} else {
		limit = defaultLimit
	}

	// 缓存 key（不含 limit：limit 只做上层截断，缓存存完整 filter 结果）
	cacheKey := filter + "|" + sortField + "|" + reverse

	// 查缓存
	s.tcacheMu.Lock()
	if ent, ok := s.tcache[cacheKey]; ok && time.Now().Before(ent.expiresAt) {
		cached := ent.data.([]models.QBTorrent)
		s.tcacheMu.Unlock()
		if limit > 0 && limit < len(cached) {
			cached = cached[:limit]
		}
		c.JSON(200, models.APIResponse{Success: true, Data: cached})
		return
	}
	s.tcacheMu.Unlock()

	// 缓存 miss → 拉 qBittorrent
	list, err := s.qbClient.GetTorrents(filter, sortField, reverse)
	if err != nil {
		c.JSON(502, models.APIResponse{Success: false, Message: err.Error()})
		return
	}

	// 写缓存（完整 filter 结果，不带 limit）
	s.tcacheMu.Lock()
	s.tcache[cacheKey] = &cacheEntry{
		data:      list,
		expiresAt: time.Now().Add(torrentCacheTTL),
	}
	s.tcacheMu.Unlock()

	// 后端再补一次进度降序（如果 qBittorrent sort 不生效的话）
	sort.Slice(list, func(i, j int) bool { return list[i].Progress > list[j].Progress })

	// limit 截断
	if limit > 0 && limit < len(list) {
		list = list[:limit]
	}

	c.JSON(200, models.APIResponse{Success: true, Data: list})
}

// torrentsStats 是概览页用的轻量统计，不走 info 大接口。
// 拉一次 transfer info（全局速度） + active/pausedUP 的数量，3 秒缓存。
func (s *Server) torrentsStats(c *gin.Context) {
	// 查缓存
	s.scacheMu.Lock()
	if ent := s.scache; ent != nil && time.Now().Before(ent.expiresAt) {
		out := ent.data.(map[string]interface{})
		s.scacheMu.Unlock()
		c.JSON(200, models.APIResponse{Success: true, Data: out})
		return
	}
	s.scacheMu.Unlock()

	// 拉速度（轻量，单次）
	ti, err := s.qbClient.GetTransferInfo()
	if err != nil {
		c.JSON(502, models.APIResponse{Success: false, Message: err.Error()})
		return
	}

	// 拉三个窄 filter 的数量（用 listTorrents 内部的缓存逻辑，这里直接调 qb client）
	activeList, _ := s.qbClient.GetTorrents("active", "", "")
	pausedUpList, _ := s.qbClient.GetTorrents("pausedUP", "", "") // 最常看的历史
	pausedDlList, _ := s.qbClient.GetTorrents("pausedDL", "", "")  // 暂停下载的未完成任务

	out := map[string]interface{}{
		"dlSpeed":     ti.DlSpeed,
		"upSpeed":     ti.UpSpeed,
		"activeCount": len(activeList),
		"pausedUp":    len(pausedUpList),
		"pausedDL":    len(pausedDlList),
	}

	// 写缓存
	s.scacheMu.Lock()
	s.scache = &cacheEntry{
		data:      out,
		expiresAt: time.Now().Add(statsCacheTTL),
	}
	s.scacheMu.Unlock()

	c.JSON(200, models.APIResponse{Success: true, Data: out})
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
	// 限速变更后清任务缓存（用户可能立刻要刷新）
	s.tcacheMu.Lock()
	s.tcache = make(map[string]*cacheEntry)
	s.tcacheMu.Unlock()
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
