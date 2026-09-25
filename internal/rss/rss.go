package rss

import (
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
}

func New(cfg *config.Manager, client *qb.Client) *Engine {
	e := &Engine{
		cfg:    cfg,
		client: client,
		stop:   make(chan struct{}),
		seen:   make(map[string]map[string]bool),
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
			logger.Warn.Printf("rss: read seen state: %v", err)
		}
		return
	}
	var p seenPersist
	if err := json.Unmarshal(data, &p); err != nil {
		logger.Warn.Printf("rss: parse seen state: %v", err)
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
	logger.Info.Printf("rss: loaded %d previously-seen entries across %d feeds", total, len(p))
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
		logger.Warn.Printf("rss: write seen state: %v", err)
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
		logger.Info.Println("RSS disabled (via reload)")
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
		logger.Info.Println("RSS disabled")
		return
	}
	interval := time.Duration(c.Interval) * time.Minute
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	logger.Info.Printf("RSS started, interval=%s", interval)

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
	logger.Debug.Printf("RSS fetching %s (%s)", feed.Name, feed.URL)
	body, err := httpGet(feed.URL)
	if err != nil {
		logger.Warn.Printf("RSS feed %s fetch failed: %v", feed.Name, err)
		return
	}
	var parsed rssFeed
	if err := xml.Unmarshal(body, &parsed); err != nil {
		logger.Warn.Printf("RSS feed %s parse failed: %v", feed.Name, err)
		return
	}

	// 快照模式：该 feed 是第一次见到，只 mark seen 不下载，避免历史条目一次性全部下
	isFresh := e.isFreshFeed(feed.ID)

	e.mu.Lock()
	if _, ok := e.seen[feed.ID]; !ok {
		e.seen[feed.ID] = make(map[string]bool)
	}
	seen := e.seen[feed.ID]
	e.mu.Unlock()

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
			logger.Info.Printf("RSS [snapshot] feed=%s item=%s (first seen, skipped)", feed.Name, item.Title)
			continue
		}

		// 正常模式：mark + 处理；下载失败则回滚，下次重试
		e.mu.Lock()
		seen[key] = true
		e.mu.Unlock()
		if !e.processItem(feed, item) {
			// 处理失败（下载 torrent 失败 / AddTorrent 失败）→ 从 seen 撤出来，下次再试
			e.mu.Lock()
			delete(seen, key)
			e.mu.Unlock()
		}
	}

	if isFresh {
		logger.Info.Printf("RSS feed=%s first snapshot: %d items marked, none downloaded (save seen once per feed)",
			feed.Name, len(newlySeen))
		e.mu.Lock()
		for _, k := range newlySeen {
			seen[k] = true
		}
		e.mu.Unlock()
		// 快照后立即落盘，避免进程重启又重新快照（会漏新条目但影响不大，落盘更稳）
		e.saveSeen()
	}
}

// processItem 返回 true 表示处理成功（或不需要处理但已被 seen 覆盖），false 表示应重试
func (e *Engine) processItem(feed models.RSSFeed, item rssItem) bool {
	for _, rule := range feed.Rules {
		if !rule.Enabled {
			continue
		}
		if !matchRule(rule, item.Title) {
			continue
		}
		logger.Info.Printf("RSS matched: feed=%s item=%s rule=%s", feed.Name, item.Title, rule.Name)

		torrentURL := item.Enclosure.URL
		if torrentURL == "" {
			torrentURL = item.Link
		}
		if torrentURL == "" {
			return true // 没有可用 URL，记为 seen 不再试
		}
		data, err := httpGet(torrentURL)
		if err != nil {
			logger.Warn.Printf("RSS download torrent failed: %v url=%s (will retry next cycle)", err, torrentURL)
			return false
		}
		if err := e.client.AddTorrent(data, rule.SavePath, rule.Category, rule.Tags, rule.UploadLimit); err != nil {
			logger.Warn.Printf("RSS add torrent failed: %v (will retry next cycle)", err)
			return false
		}
		break
	}
	return true
}

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
	} else {
		if rule.Include != "" && !strings.Contains(title, strings.ToLower(rule.Include)) {
			return false
		}
		if rule.Exclude != "" && strings.Contains(title, strings.ToLower(rule.Exclude)) {
			return false
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
	req.Header.Set("User-Agent", "QBHive/1.0")
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

