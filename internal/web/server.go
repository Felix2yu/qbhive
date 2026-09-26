package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
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
	torrentCacheTTL = 5 * time.Second
	statsCacheTTL   = 3 * time.Second
	defaultLimit    = 500
	maxLimit        = 2000
)

// qBittorrent v5.0+ 的 filter 参数值：
// all, downloading, seeding, completed, stopped, active, inactive, running,
// stalled, stalled_uploading, stalled_downloading, errored
//
// 前端用 torrent 的 state 值（如 stoppedUP、stoppedDL、stalledUP）作为 filter，
// 这里把 state 值映射到 qB 的 filter 参数；拉回后再按 state 二次过滤，
// 因为 stoppedUP / stoppedDL 共用 "stopped" filter，stalled 类同理。
var stateToFilter = map[string]string{
	"active":      "active",
	"downloading": "downloading",
	"seeding":     "seeding",
	"stoppedUP":   "stopped",
	"stoppedDL":   "stopped",
	"stalledUP":   "stalled_uploading",
	"stalledDL":   "stalled_downloading",
	"completed":   "completed",
	"all":         "all",
}

// 需要按 state 二次过滤的前端 filter（因为一个 filter 参数可能返回多个 state 值）
var needsStateFilter = map[string]bool{
	"stoppedUP": true,
	"stoppedDL": true,
}

// stateFilterSets 定义每个前端 filter 对应的 state 集合
var stateFilterSets = map[string]map[string]bool{
	"stoppedUP": {"stoppedUP": true},
	"stoppedDL": {"stoppedDL": true},
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
		// tcache 必须显式初始化：Go 的 nil map 读安全但写会 panic，
		// listTorrents 每次缓存 miss 后写缓存，未初始化会导致 500 空响应
		tcache: make(map[string]*cacheEntry),
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
		api.GET("/debug/qb-raw", s.debugQBRaw)
		api.GET("/torrents/stats", s.torrentsStats)
		api.POST("/torrents/:hash/limit", s.setTorrentLimit)

		api.GET("/rss/status", s.rssStatus)
		api.POST("/rss/reset", s.rssReset)
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

// isMaskedAppriseURL 判断一个 Apprise URL 是否是掩码后的显示值
// （所有 maskAppriseURL 的输出都包含 "...********"）
func isMaskedAppriseURL(u string) bool {
	return strings.Contains(u, "...********")
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
	// AppriseURLs：前端拿到的是掩码值，保存时跳过掩码项、跳过空字符串，其他按 index 替换
	// 关键：如果前端过滤后整个数组变空（用户什么都没改就保存），要保留原值
	curURLs := cur.Notifier.AppriseURLs
	inURLs := in.Notifier.AppriseURLs

	if len(inURLs) == 0 {
		// 空数组 → 保留原值（前端可能把所有掩码项都过滤掉了）
		in.Notifier.AppriseURLs = curURLs
	} else {
		cleaned := make([]string, 0, len(inURLs))
		// 先收集所有非掩码、非空的"用户新输入"项及其原始 index
		type pair struct {
			idx int
			val string
		}
		var newInputs []pair
		for i, u := range inURLs {
			trimmed := strings.TrimSpace(u)
			if trimmed == "" {
				continue // 空字符串表示用户删了这一行
			}
			if isMaskedAppriseURL(trimmed) {
				continue // 掩码值跳过，不要放进去
			}
			newInputs = append(newInputs, pair{i, trimmed})
		}
		if len(newInputs) == 0 {
			// 用户没改任何 URL → 保留原值
			in.Notifier.AppriseURLs = curURLs
		} else {
			// 用 cur 做基础，按 index 替换用户新输入的项
			base := make([]string, len(curURLs))
			copy(base, curURLs)
			// 如果前端传了更多项（新增 URL），扩展 base
			maxIdx := 0
			for _, p := range newInputs {
				if p.idx > maxIdx {
					maxIdx = p.idx
				}
			}
			if maxIdx >= len(base) {
				extended := make([]string, maxIdx+1)
				copy(extended, base)
				base = extended
			}
			for _, p := range newInputs {
				base[p.idx] = p.val
			}
			// 去掉尾部空字符串（用户删了末尾条目）
			cleaned = base
			in.Notifier.AppriseURLs = cleaned
		}
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

// debugQBRaw 对比两种路径：原始 HTTP vs client.GetTorrents()
func (s *Server) debugQBRaw(c *gin.Context) {
	filter := c.Query("filter")
	sort := c.Query("sort")
	reverse := c.Query("reverse")

	// === 路径 A: 直接 HTTP（绕过 client 层） ===
	qbURL := s.cfg.Get().Qbittorrent.URL
	if qbURL == "" {
		c.String(500, "no qbittorrent url configured")
		return
	}
	v := url.Values{}
	if filter != "" { v.Set("filter", filter) }
	if sort != "" { v.Set("sort", sort) }
	if reverse != "" { v.Set("reverse", reverse) }
	path := "/api/v2/torrents/info"
	if len(v) > 0 { path += "?" + v.Encode() }
	reqURL := qbURL + path
	req, _ := http.NewRequest("GET", reqURL, nil)
	if key := s.cfg.Get().Qbittorrent.APIKey; key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	} else if ck := s.qbClient.GetCookie(); ck != "" {
		req.Header.Set("Cookie", ck)
	}
	respA, errA := http.DefaultClient.Do(req)
	var bodyA []byte
	var statusA int
	if errA == nil {
		statusA = respA.StatusCode
		bodyA, _ = io.ReadAll(respA.Body)
		respA.Body.Close()
	}

	// === 路径 B: 走 client.GetTorrents() ===
	listB, errB := s.qbClient.GetTorrents(filter, sort, reverse)

	c.Header("Content-Type", "text/plain; charset=utf-8")
	c.String(200,
		"=== 路径 A: 直接 HTTP ===\nURL: %s\nStatus: %d\nLen: %d\nBody[:300]: %q\n\n"+
		"=== 路径 B: client.GetTorrents(%q, %q, %q) ===\nlistLen=%d err=%v\n",
		reqURL, statusA, len(bodyA), string(bodyA[:min(len(bodyA), 300)]),
		filter, sort, reverse, len(listB), errB,
	)
}

func (s *Server) listTorrents(c *gin.Context) {
	filter := c.Query("filter")
	if filter == "" {
		filter = "active"
	}
	// 前端 filter 值（qBittorrent 5.0+ 的 torrent state 值）→ qB API filter 参数
	qbFilter, _ := stateToFilter[filter]
	if qbFilter == "" {
		qbFilter = filter // 未知值原样透传，让 qB 自己决定
	}

	sortField := c.Query("sort")
	if sortField == "" {
		sortField = "added_on" // 默认按添加时间倒序，新任务排前
	}
	reverse := c.Query("reverse")
	if reverse == "" {
		reverse = "true"
	}
	// qBittorrent 的 reverse 参数只接受 "true"/"false"
	r := strings.ToLower(reverse)
	switch r {
	case "1", "yes", "on":
		reverse = "true"
	case "0", "no", "off":
		reverse = "false"
	default:
		// 保持原值，让 qB 自己校验
	}

	// limit 处理：默认 500，最大 2000，0 表示全量（加 warning）
	var limit int
	limitStr := c.Query("limit")
	if limitStr == "" {
		limit = defaultLimit
	} else if n, e := strconv.Atoi(limitStr); e == nil {
		switch {
		case n == 0:
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

	// 拉 qBittorrent（用映射后的 filter 参数）
	list, err := s.qbClient.GetTorrents(qbFilter, sortField, reverse)
	if err != nil {
		c.JSON(502, models.APIResponse{Success: false, Message: err.Error()})
		return
	}

	// stoppedUP / stoppedDL 共用 qB filter="stopped"，拉回后按 state 二次过滤
	if needsStateFilter[filter] {
		allowed := stateFilterSets[filter]
		filtered := make([]models.QBTorrent, 0, len(list))
		for _, t := range list {
			if allowed[t.State] {
				filtered = append(filtered, t)
			}
		}
		list = filtered
	}

	// 写缓存
	s.tcacheMu.Lock()
	s.tcache[cacheKey] = &cacheEntry{
		data:      list,
		expiresAt: time.Now().Add(torrentCacheTTL),
	}
	s.tcacheMu.Unlock()

	// limit 截断
	if limit > 0 && limit < len(list) {
		list = list[:limit]
	}

	c.JSON(200, models.APIResponse{Success: true, Data: list})
}

// torrentsStats 是概览页用的轻量统计，不走 info 大接口。
// 拉一次 transfer info（全局速度） + active/stoppedUP/stoppedDL 的数量，3 秒缓存。

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

	activeList, _ := s.qbClient.GetTorrents("active", "", "")
	// qBittorrent v5.0+ stopped filter 包含 stoppedUP + stoppedDL
	stoppedList, _ := s.qbClient.GetTorrents("stopped", "", "")

	var (
		stoppedUpCount int
		stoppedDlCount int
		downloadingCount int
		seedingCount  int
		stalledCount  int
		erroredCount  int
	)
	for _, t := range stoppedList {
		switch t.State {
		case "stoppedUP":
			stoppedUpCount++
		case "stoppedDL":
			stoppedDlCount++
		}
	}
	for _, t := range activeList {
		switch t.State {
		case "downloading", "forcedDL":
			downloadingCount++
		case "uploading", "forcedUP", "checkingUP", "queuedUP":
			seedingCount++
		case "stalledDL", "stalledUP", "metaDL", "checkingDL", "queuedDL":
			stalledCount++
		case "errored", "error", "missingFiles":
			erroredCount++
		}
	}

	out := map[string]interface{}{
		"dlSpeed":         ti.DlSpeed,
		"upSpeed":         ti.UpSpeed,
		"dlSpeedLimit":    ti.DlSpeedLimit,
		"upSpeedLimit":    ti.UpSpeedLimit,
		"activeCount":     len(activeList),
		"stoppedUP":       stoppedUpCount,
		"stoppedDL":       stoppedDlCount,
		"downloadingCount": downloadingCount,
		"seedingCount":    seedingCount,
		"stalledCount":    stalledCount,
		"erroredCount":    erroredCount,
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
	c.JSON(200, models.APIResponse{Success: true})
}

func (s *Server) forceRSS(c *gin.Context) {
	s.rssEngine.ForceFetch()
	c.JSON(200, models.APIResponse{Success: true, Message: "RSS fetch triggered"})
}

// rssReset 清空指定 feed (feedId) 或全部 (feedId=空字符串) 的 seen 状态。
// 用户修改匹配规则后调这个 + 立即拉取，让新规则对历史条目重新评估。
func (s *Server) rssReset(c *gin.Context) {
	var body struct {
		FeedID string `json:"feedId"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, models.APIResponse{Success: false, Message: err.Error()})
		return
	}
	if body.FeedID == "" {
		s.rssEngine.ResetAll()
	} else {
		s.rssEngine.ResetFeed(body.FeedID)
	}
	s.rssEngine.ForceFetch() // 重置后立刻拉一次，新规则立即生效
	c.JSON(200, models.APIResponse{Success: true, Message: "RSS seen reset, fetch triggered"})
}

func (s *Server) rssStatus(c *gin.Context) {
	c.JSON(200, models.APIResponse{Success: true, Data: s.rssEngine.Status()})
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
