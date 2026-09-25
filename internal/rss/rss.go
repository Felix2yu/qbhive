package rss

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
		Title string      `xml:"title"`
		Items []rssItem   `xml:"item"`
	} `xml:"channel"`
}

type rssItem struct {
	Title       string       `xml:"title"`
	Link        string       `xml:"link"`
	Enclosure   rssEnclosure `xml:"enclosure"`
	GUID        string       `xml:"guid"`
	PubDate     string       `xml:"pubDate"`
}

type rssEnclosure struct {
	URL  string `xml:"url,attr"`
	Type string `xml:"type,attr"`
}

type Engine struct {
	cfg    *config.Manager
	client *qb.Client
	stop   chan struct{}

	// 已处理的条目，按 feed ID 分组去重
	seen map[string]map[string]bool
	mu   sync.Mutex
}

func New(cfg *config.Manager, client *qb.Client) *Engine {
	return &Engine{
		cfg:    cfg,
		client: client,
		stop:   make(chan struct{}),
		seen:   make(map[string]map[string]bool),
	}
}

func (e *Engine) Start() {
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
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-e.stop:
				return
			case <-ticker.C:
				e.fetchAll()
			}
		}
	}()
}

func (e *Engine) Stop() {
	close(e.stop)
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
	e.mu.Lock()
	if _, ok := e.seen[feed.ID]; !ok {
		e.seen[feed.ID] = make(map[string]bool)
	}
	seen := e.seen[feed.ID]
	e.mu.Unlock()

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
		seen[key] = true
		e.processItem(feed, item)
	}
}

func (e *Engine) processItem(feed models.RSSFeed, item rssItem) {
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
			continue
		}
		data, err := httpGet(torrentURL)
		if err != nil {
			logger.Warn.Printf("RSS download torrent failed: %v url=%s", err, torrentURL)
			continue
		}
		if err := e.client.AddTorrent(data, rule.SavePath, rule.Category, rule.Tags, rule.UploadLimit); err != nil {
			logger.Warn.Printf("RSS add torrent failed: %v", err)
		}
		break
	}
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

var _ = url.QueryEscape
