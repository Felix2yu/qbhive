package rss

import (
	"container/list"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Felix2yu/qbhive/internal/config"
	"github.com/Felix2yu/qbhive/internal/logger"
	"github.com/Felix2yu/qbhive/internal/models"
	qb "github.com/Felix2yu/qbhive/internal/qbittorrent"
)

// FeedStatus 是对外暴露的订阅源运行状态（纯数据，可 JSON 序列化）
type FeedStatus struct {
	FeedID      string       `json:"feedId"`
	FeedName    string       `json:"feedName"`
	URL         string       `json:"url"`
	Enabled     bool         `json:"enabled"`
	LastFetchAt time.Time    `json:"lastFetchAt"` // 最近一次拉取开始时间
	Fetching    bool         `json:"fetching"`    // 此刻是否正在拉取
	LastOK      *bool        `json:"lastOk"`      // 最近一次拉取是否成功（nil = 还没拉过）
	LastError   string       `json:"lastError"`   // 最近一次拉取错误消息（空=无错）
	ItemCount   int          `json:"itemCount"`   // 最近一次拉取解析到的条目数
	Matched     int          `json:"matched"`     // 命中规则的条目数
	Downloaded  int          `json:"downloaded"`  // 成功提交给 qB 的条目数
	Failed      int          `json:"failed"`      // 提交 qB 失败（会重试）的条目数
	Snapshot    bool         `json:"snapshot"`    // 最近一次是快照（首次接入，只 mark seen 不下）
	RecentItems []RecentItem `json:"recentItems"` // 最近处理过的条目（最多 100 条，按时间倒序）
	SeenCount   int          `json:"seenCount"`   // 该 feed 已见过（去重）的条目总数
}

// RecentItem 是最近一次 fetch 里处理过的某条记录
type RecentItem struct {
	Title       string    `json:"title"`
	RuleName    string    `json:"ruleName,omitempty"`
	Action      string    `json:"action"` // downloaded / skipped_no_rule / skipped_snapshot / failed
	Error       string    `json:"error,omitempty"`
	ProcessedAt time.Time `json:"processedAt"`
}

// feedState 是 Engine 内部的运行态（含 list.List，JSON 不友好，所以 Status() 会做转换）
type feedState struct {
	mu          sync.Mutex
	fetching    bool
	lastFetchAt time.Time
	lastOK      *bool
	lastError   string
	itemCount   int
	matched     int
	downloaded  int
	failed      int
	snapshot    bool
	recent      *list.List // 值 = RecentItem（按时间从新到旧；最多 100）
	seenCount   int
	forceRescan bool // 由 ResetFeed 置 true：下次 fetch 强制跳过快照，对所有条目重新跑规则
}

func newFeedState() *feedState {
	return &feedState{recent: list.New()}
}

func (s *feedState) pushRecent(r RecentItem) {
	// 最多保留 100 条，足够看清楚整轮拉取的处理结果，又不会无限膨胀
	s.recent.PushFront(r)
	for s.recent.Len() > 100 {
		s.recent.Remove(s.recent.Back())
	}
}

func (s *feedState) collectRecent() []RecentItem {
	out := make([]RecentItem, 0, s.recent.Len())
	for e := s.recent.Front(); e != nil; e = e.Next() {
		out = append(out, e.Value.(RecentItem))
	}
	return out
}

type rssFeed struct {
	XMLName xml.Name `xml:"rss"`
	Channel struct {
		Title string    `xml:"title"`
		Items []rssItem `xml:"item"`
	} `xml:"channel"`
}

type rssItem struct {
	Title     string       `xml:"title"`
	Link      string       `xml:"link"`
	Enclosure rssEnclosure `xml:"enclosure"`
	GUID      string       `xml:"guid"`
	PubDate   string       `xml:"pubDate"`
}

type rssEnclosure struct {
	URL  string `xml:"url,attr"`
	Type string `xml:"type,attr"`
}

// seenPersist 是持久化到 JSON 时的结构：feedID -> keys
type seenPersist map[string][]string

const (
	// stateFileName 和 scheduler 的 finished.json 同目录
	stateFileName = "rss_seen.json"
)

type Engine struct {
	cfg       *config.Manager
	client    *qb.Client
	stop      chan struct{}
	stateFile string

	// 运行中的后台协程控制（Reload/Stop 时重建/关闭）
	fetchStop chan struct{} // fetchAll 的 ticker
	saveStop  chan struct{} // saveSeen 的 ticker

	// 已处理的条目，按 feed ID 分组去重
	seen map[string]map[string]bool
	mu   sync.Mutex

	// 每个 feed ID 的运行态（状态面板用）
	states map[string]*feedState
}

func New(cfg *config.Manager, client *qb.Client) *Engine {
	e := &Engine{
		cfg:    cfg,
		client: client,
		stop:   make(chan struct{}),
		seen:   make(map[string]map[string]bool),
		states: make(map[string]*feedState),
	}
	e.stateFile = defaultStatePath()
	e.loadSeen()
	return e
}

// defaultStatePath 返回 rss_seen.json 的默认路径，与 config.json 同目录
func defaultStatePath() string {
	if cfg := os.Getenv("QBHIVE_CONFIG"); cfg != "" {
		dir := filepath.Dir(cfg)
		return filepath.Join(dir, stateFileName)
	}
	return filepath.Join("data", stateFileName)
}

func (e *Engine) loadSeen() {
	data, err := os.ReadFile(e.stateFile)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Warn.Printf("RSS 读取已见状态失败：%v", err)
		}
		return
	}
	var p seenPersist
	if err := json.Unmarshal(data, &p); err != nil {
		logger.Warn.Printf("RSS 解析已见状态失败：%v", err)
		return
	}
	total := 0
	for feedID, keys := range p {
		m := make(map[string]bool, len(keys))
		for _, k := range keys {
			m[k] = true
		}
		e.seen[feedID] = m
		total += len(keys)
	}
	logger.Info.Printf("RSS 已加载 %d 条历史已见条目，覆盖 %d 个订阅源", total, len(p))
}

func (e *Engine) saveSeen() {
	e.mu.Lock()
	p := make(seenPersist, len(e.seen))
	for feedID, m := range e.seen {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		p[feedID] = keys
	}
	e.mu.Unlock()
	data, _ := json.MarshalIndent(p, "", "  ")
	dir := filepath.Dir(e.stateFile)
	_ = os.MkdirAll(dir, 0o755)
	tmp := e.stateFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		logger.Warn.Printf("RSS 写入已见状态失败：%v", err)
		return
	}
	_ = os.Rename(tmp, e.stateFile)
}

// isFreshFeed 判断某个 feed 是否是"第一次见到"（seen 里为空）
func (e *Engine) isFreshFeed(feedID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	m, ok := e.seen[feedID]
	return !ok || len(m) == 0
}

func (e *Engine) getOrCreateState(feedID string) *feedState {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.states[feedID]
	if !ok {
		s = newFeedState()
		e.states[feedID] = s
	}
	return s
}

// Status 返回当前所有订阅源的运行状态。前端状态面板用。
// 返回 key 是 feedID；配置里已禁用/删除的 feed 不会出现在 map 里。
func (e *Engine) Status() []FeedStatus {
	c := e.cfg.Get().RSS
	e.mu.Lock()
	// 拍一份 seen 的计数快照，避免在 state 锁里再去碰 seen
	seenCounts := make(map[string]int, len(e.seen))
	for fid, m := range e.seen {
		seenCounts[fid] = len(m)
	}
	statesCopy := make(map[string]*feedState, len(e.states))
	for fid, s := range e.states {
		statesCopy[fid] = s
	}
	e.mu.Unlock()

	out := make([]FeedStatus, 0, len(c.Feeds))
	for _, f := range c.Feeds {
		s := statesCopy[f.ID]
		fs := FeedStatus{
			FeedID:   f.ID,
			FeedName: f.Name,
			URL:      f.URL,
			Enabled:  f.Enabled,
		}
		if s != nil {
			s.mu.Lock()
			fs.LastFetchAt = s.lastFetchAt
			fs.Fetching = s.fetching
			fs.LastOK = s.lastOK
			fs.LastError = s.lastError
			fs.ItemCount = s.itemCount
			fs.Matched = s.matched
			fs.Downloaded = s.downloaded
			fs.Failed = s.failed
			fs.Snapshot = s.snapshot
			fs.RecentItems = s.collectRecent()
			s.mu.Unlock()
		}
		fs.SeenCount = seenCounts[f.ID]
		out = append(out, fs)
	}
	return out
}

// ResetFeed 清空指定 feed 的已见过条目和运行态。
// 用户修改了匹配规则后调这个 → 下次拉取会重新对所有条目跑规则，而不是因为 seen 跳过。
// 同时调 ForceFetch 立即可见效果。
func (e *Engine) ResetFeed(feedID string) {
	e.mu.Lock()
	delete(e.seen, feedID)
	if s, ok := e.states[feedID]; ok {
		s.mu.Lock()
		s.fetching = false
		s.lastOK = nil
		s.lastError = ""
		s.itemCount = 0
		s.matched = 0
		s.downloaded = 0
		s.failed = 0
		s.snapshot = false
		s.forceRescan = true
		s.recent = list.New()
		s.mu.Unlock()
	}
	e.mu.Unlock()
	e.saveSeen()
	logger.Info.Printf("RSS 已重置订阅源 %s 的已见状态（下次拉取将重新扫描所有条目）", feedID)
}

// ResetAll 清空所有 feed 的 seen。
func (e *Engine) ResetAll() {
	e.mu.Lock()
	for fid := range e.seen {
		delete(e.seen, fid)
	}
	for _, s := range e.states {
		s.mu.Lock()
		s.fetching = false
		s.lastOK = nil
		s.lastError = ""
		s.itemCount = 0
		s.matched = 0
		s.downloaded = 0
		s.failed = 0
		s.snapshot = false
		s.forceRescan = true
		s.recent = list.New()
		s.mu.Unlock()
	}
	e.mu.Unlock()
	e.saveSeen()
	logger.Info.Println("RSS 已重置全部已见状态（下次拉取将重新扫描所有条目）")
}

// SetClient 热替换 qb 客户端
func (e *Engine) SetClient(c *qb.Client) {
	e.client = c
}

// Reload 根据最新配置重启（或停止）RSS 轮询。
// 换了 qB 凭据、改了开关或间隔后调用。
func (e *Engine) Reload(c *qb.Client) {
	if c != nil {
		e.client = c
	}
	// 停掉旧 goroutine
	if e.fetchStop != nil {
		close(e.fetchStop)
		e.fetchStop = nil
	}
	if e.saveStop != nil {
		close(e.saveStop)
		e.saveStop = nil
	}
	cfg := e.cfg.Get().RSS
	if !cfg.Enabled {
		logger.Info.Println("RSS 已停用（重载）")
		return
	}
	e.runPoller()
}

func (e *Engine) Start() {
	e.runPoller()
}

func (e *Engine) runPoller() {
	c := e.cfg.Get().RSS
	if !c.Enabled {
		logger.Info.Println("RSS 已停用")
		return
	}
	interval := time.Duration(c.Interval) * time.Minute
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	logger.Info.Printf("RSS 启动，间隔=%s", interval)

	// 启动时立即跑一次
	go e.fetchAll()

	// fetchAll ticker
	e.fetchStop = make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-e.fetchStop:
				return
			case <-e.stop:
				return
			case <-ticker.C:
				e.fetchAll()
			}
		}
	}()

	// saveSeen ticker（常驻，不受 enabled 影响）
	e.saveStop = make(chan struct{})
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-e.saveStop:
				return
			case <-e.stop:
				e.saveSeen()
				return
			case <-ticker.C:
				e.saveSeen()
			}
		}
	}()
}

func (e *Engine) Stop() {
	if e.fetchStop != nil {
		close(e.fetchStop)
	}
	if e.saveStop != nil {
		close(e.saveStop)
	}
	close(e.stop)
	e.saveSeen()
}

func (e *Engine) fetchAll() {
	c := e.cfg.Get().RSS
	var wg sync.WaitGroup
	for _, feed := range c.Feeds {
		if !feed.Enabled {
			continue
		}
		wg.Add(1)
		go func(f models.RSSFeed) {
			defer wg.Done()
			e.fetchFeed(f)
		}(feed)
	}
	wg.Wait()
}

func (e *Engine) fetchFeed(feed models.RSSFeed) {
	st := e.getOrCreateState(feed.ID)

	// 运行态：准备开始
	st.mu.Lock()
	st.fetching = true
	st.lastFetchAt = time.Now()
	st.itemCount = 0
	st.matched = 0
	st.downloaded = 0
	st.failed = 0
	st.snapshot = false
	st.lastError = ""
	st.mu.Unlock()

	logger.Debug.Printf("RSS 正在拉取 %s（%s）", feed.Name, feed.URL)

	body, err := httpGet(feed.URL)
	if err != nil {
		logger.Warn.Printf("RSS 订阅源 %s 拉取失败：%v", feed.Name, err)
		errStr := err.Error()
		ok := false
		st.mu.Lock()
		st.fetching = false
		st.lastOK = &ok
		st.lastError = errStr
		st.mu.Unlock()
		return
	}
	var parsed rssFeed
	if err := xml.Unmarshal(body, &parsed); err != nil {
		logger.Warn.Printf("RSS 订阅源 %s 解析失败：%v", feed.Name, err)
		errStr := err.Error()
		ok := false
		st.mu.Lock()
		st.fetching = false
		st.lastOK = &ok
		st.lastError = errStr
		st.mu.Unlock()
		return
	}

	// 快照模式：该 feed 是第一次见到，只 mark seen 不下载，避免历史条目一次性全部下
	isFresh := e.isFreshFeed(feed.ID)
	// 但 forceRescan 是用户手动 reset 的 → 强制进入正常模式，对所有条目重跑规则
	if st.forceRescan {
		st.mu.Lock()
		st.forceRescan = false
		st.mu.Unlock()
		isFresh = false
		logger.Info.Printf("RSS 订阅源 %s 强制重扫：跳过快照模式", feed.Name)
	}

	e.mu.Lock()
	if _, ok := e.seen[feed.ID]; !ok {
		e.seen[feed.ID] = make(map[string]bool)
	}
	seen := e.seen[feed.ID]
	e.mu.Unlock()

	// 本轮 itemCount 记下来
	st.mu.Lock()
	st.itemCount = len(parsed.Channel.Items)
	st.snapshot = isFresh
	st.mu.Unlock()

	// 本次 fetch 新见过的 key；快照模式下直接 mark 所有，正常模式只对处理过的 mark
	var newlySeen []string
	for _, item := range parsed.Channel.Items {
		key := item.GUID
		if key == "" {
			key = item.Link
		}
		if key == "" {
			continue
		}
		if seen[key] {
			continue
		}
		newlySeen = append(newlySeen, key)

		if isFresh {
			// 快照：第一次接入，只记下来不下载
			logger.Info.Printf("RSS [快照] 订阅源 %s 条目 %s（首次见到，跳过）", feed.Name, item.Title)
			st.mu.Lock()
			st.pushRecent(RecentItem{Title: item.Title, Action: "skipped_snapshot", ProcessedAt: time.Now()})
			st.mu.Unlock()
			continue
		}

		// 正常模式：mark + 处理；下载失败则回滚，下次重试
		e.mu.Lock()
		seen[key] = true
		e.mu.Unlock()
		ruleName, procErr := e.processItem(feed, item)
		st.mu.Lock()
		if procErr != nil {
			st.failed++
			st.pushRecent(RecentItem{Title: item.Title, RuleName: ruleName, Action: "failed", Error: procErr.Error(), ProcessedAt: time.Now()})
		} else if ruleName != "" {
			st.matched++
			st.downloaded++
			st.pushRecent(RecentItem{Title: item.Title, RuleName: ruleName, Action: "downloaded", ProcessedAt: time.Now()})
		} else {
			// 正常模式下所有规则都没命中 → 也要记下来，让用户知道哪些条目解析到了但没被规则覆盖
			st.pushRecent(RecentItem{Title: item.Title, Action: "skipped_no_rule", ProcessedAt: time.Now()})
		}
		st.mu.Unlock()

		if procErr != nil {
			// 处理失败（下载 torrent 失败 / AddTorrent 失败）→ 从 seen 撤出来，下次再试
			e.mu.Lock()
			delete(seen, key)
			e.mu.Unlock()
		}
	}

	if isFresh {
		logger.Info.Printf("RSS 订阅源 %s 首次快照：%d 条目标记，未下载（每个订阅源保存一次已见状态）",
			feed.Name, len(newlySeen))
		e.mu.Lock()
		for _, k := range newlySeen {
			seen[k] = true
		}
		e.mu.Unlock()
		// 快照后立即落盘，避免进程重启又重新快照（会漏新条目但影响不大，落盘更稳）
		e.saveSeen()
	}

	ok := true
	st.mu.Lock()
	st.fetching = false
	st.lastOK = &ok
	st.mu.Unlock()
}

// processItem 返回匹配到的规则名（空=无规则命中）和处理错误（nil=成功或无需处理）。
// 有规则命中且提交 qB 成功 → ("规则名", nil)；
// 有规则命中但下载/AddTorrent 失败 → ("规则名", err)；
// 无规则命中 → ("", nil)，由调用方决定是否记日志；
// 有规则命中但没有 torrent URL → ("规则名", nil)，视为已 seen 不再重试。
func (e *Engine) processItem(feed models.RSSFeed, item rssItem) (string, error) {
	for _, rule := range feed.Rules {
		if !rule.Enabled {
			continue
		}
		if !matchRule(rule, item.Title) {
			continue
		}
		logger.Info.Printf("RSS 规则命中：订阅源 %s 条目 %s 规则 %s", feed.Name, item.Title, rule.Name)

		torrentURL := item.Enclosure.URL
		if torrentURL == "" {
			torrentURL = item.Link
		}
		if torrentURL == "" {
			return rule.Name, nil // 没有可用 URL，记为 seen 不再试
		}
		data, err := httpGet(torrentURL)
		if err != nil {
			logger.Warn.Printf("RSS 下载种子失败：%v url=%s（下一周期重试）", err, torrentURL)
			return rule.Name, fmt.Errorf("下载 torrent 失败: %w", err)
		}
		if err := e.client.AddTorrent(data, rule.SavePath, rule.Category, rule.Tags, rule.UploadLimit, rule.Paused); err != nil {
			logger.Warn.Printf("RSS 添加种子失败：%v（下一周期重试）", err)
			return rule.Name, fmt.Errorf("添加到 qBittorrent 失败: %w", err)
		}
		return rule.Name, nil
	}
	return "", nil
}

// matchRule 判断标题是否命中规则。
//
// keyword 模式支持多关键词：
//   - 用 "|" 分隔 = OR（任一子串命中即可）
//   - 用空格分隔 = AND（同一 OR 组里所有子串必须同时出现）
//   - 两层组合示例："ManoJob 720p|MrLucky 1080p"
//     = (ManoJob AND 720p) OR (MrLucky AND 1080p)
//   - Exclude 同样支持：任一命中则排除
func matchRule(rule models.RSSRule, title string) bool {
	if rule.Include == "" && rule.Exclude == "" {
		return false
	}
	title = strings.ToLower(title)

	if rule.Mode == "regex" {
		if rule.Include != "" {
			re, err := regexp.Compile(rule.Include)
			if err != nil || !re.MatchString(title) {
				return false
			}
		}
		if rule.Exclude != "" {
			re, err := regexp.Compile(rule.Exclude)
			if err == nil && re.MatchString(title) {
				return false
			}
		}
		return true
	}

	// keyword 模式：include 用 "|" 分 OR 组，每组内空格分 AND 子词
	if rule.Include != "" {
		groups := strings.Split(rule.Include, "|")
		orHit := false
		for _, g := range groups {
			g = strings.TrimSpace(g)
			if g == "" {
				continue
			}
			andWords := strings.Fields(g) // 按任意空白切
			allHit := true
			for _, w := range andWords {
				if !strings.Contains(title, strings.ToLower(w)) {
					allHit = false
					break
				}
			}
			if allHit {
				orHit = true
				break
			}
		}
		if !orHit {
			return false
		}
	}
	if rule.Exclude != "" {
		groups := strings.Split(rule.Exclude, "|")
		for _, g := range groups {
			g = strings.TrimSpace(g)
			if g == "" {
				continue
			}
			andWords := strings.Fields(g)
			allHit := true
			for _, w := range andWords {
				if !strings.Contains(title, strings.ToLower(w)) {
					allHit = false
					break
				}
			}
			if allHit {
				return false // 任一 OR 组命中 → 整个排除
			}
		}
	}
	return true
}

func httpGet(target string) ([]byte, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest("GET", target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent",
		"qBittorrent/4.6.0 (https://www.qbittorrent.org)")
	req.Header.Set("Accept",
		"application/rss+xml, application/xml;q=0.9, text/xml;q=0.8, */*;q=0.7")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// ForceFetch 用于 API 调用立即触发一次 RSS 拉取
func (e *Engine) ForceFetch() {
	go e.fetchAll()
}
