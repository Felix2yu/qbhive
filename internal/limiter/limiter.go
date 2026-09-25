package limiter

import (
	"regexp"
	"time"

	"github.com/Felix2yu/qbhive/internal/config"
	"github.com/Felix2yu/qbhive/internal/logger"
	qb "github.com/Felix2yu/qbhive/internal/qbittorrent"
)

// Limiter 定时扫描 torrent 列表，根据匹配规则设置上传限速
type Limiter struct {
	cfg    *config.Manager
	client *qb.Client
	stop   chan struct{}

	// 运行中的 ticker 控制：每次 Reload/Start 时重建
	tickerStop chan struct{}
	running    bool

	// 上一次下发的限速（hash → limit 字节/秒），避免对值未变的 torrent 重复调用 qB API
	lastLimits map[string]int64
}

func New(cfg *config.Manager, client *qb.Client) *Limiter {
	return &Limiter{cfg: cfg, client: client, stop: make(chan struct{}), lastLimits: make(map[string]int64)}
}

// SetClient 热替换 qb 客户端（配置改了连哪台 qB 都变了）
func (l *Limiter) SetClient(c *qb.Client) {
	l.client = c
}

// Reload 根据最新配置重启（或停止）限速轮询。
// client 和 cfg 可分别传入 nil / 空，Reload 会沿用已有指针。
func (l *Limiter) Reload(c *qb.Client) {
	if c != nil {
		l.client = c
	}
	// 停掉旧的 ticker goroutine
	if l.tickerStop != nil {
		close(l.tickerStop)
		l.tickerStop = nil
		l.running = false
	}
	// 规则可能变更了，清空前次记录让所有 torrent 重新走一次匹配+下发
	l.lastLimits = make(map[string]int64)
	cfg := l.cfg.Get().Limiter
	if !cfg.Enabled {
		logger.Info.Println("Limiter disabled (via reload)")
		return
	}
	interval := time.Duration(cfg.Interval) * time.Second
	if interval <= 0 {
		interval = 10 * time.Second
	}
	logger.Info.Printf("Limiter reloaded, interval=%s", interval)
	l.runTicker(interval)
}

func (l *Limiter) Start() {
	c := l.cfg.Get().Limiter
	if !c.Enabled {
		logger.Info.Println("Limiter disabled")
		return
	}
	interval := time.Duration(c.Interval) * time.Second
	if interval <= 0 {
		interval = 10 * time.Second
	}
	logger.Info.Printf("Limiter started, interval=%s", interval)
	l.runTicker(interval)
}

func (l *Limiter) runTicker(interval time.Duration) {
	l.tickerStop = make(chan struct{})
	l.running = true
	stop := l.tickerStop
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-l.stop:
				return
			case <-ticker.C:
				l.apply()
			}
		}
	}()
}

func (l *Limiter) Stop() {
	close(l.stop)
}

func (l *Limiter) apply() {
	rules := l.cfg.Get().Limiter.Rules
	torrents, err := l.client.GetTorrents("all")
	if err != nil {
		logger.Warn.Printf("Limiter get torrents failed: %v", err)
		return
	}
	// 本轮命中的 hash 集合：结束后把没命中过的 hash 从 lastLimits 里剔掉（对应 torrent 删了/规则改了）
	seen := make(map[string]bool, len(torrents))
	changes := 0
	for _, t := range torrents {
		seen[t.Hash] = true
		var (
			limit int64 = -1 // 默认没命中规则 = 无限制
			matched      = false
		)
		for _, r := range rules {
			if !r.Enabled {
				continue
			}
			ok, err := regexp.MatchString(r.Match, t.Name)
			if err != nil {
				continue
			}
			if ok {
				if r.UploadLimit > 0 {
					limit = int64(r.UploadLimit) * 1024
				}
				matched = true
				break
			}
		}
		// 没命中规则的 torrent，保持 qB 原生限速不动（不下发 -1 覆盖用户手动限速）
		if !matched {
			continue
		}
		prev, known := l.lastLimits[t.Hash]
		if known && prev == limit {
			continue // 值没变，跳过 API 调用
		}
		if err := l.client.SetUploadLimit(t.Hash, limit); err != nil {
			logger.Warn.Printf("limiter set limit hash=%s err=%v", t.Hash, err)
			continue
		}
		l.lastLimits[t.Hash] = limit
		changes++
		logger.Debug.Printf("limiter: torrent=%s limit=%d (matched)", t.Name, limit)
	}
	// 清理已消失 torrent 的 lastLimits
	for h := range l.lastLimits {
		if !seen[h] {
			delete(l.lastLimits, h)
		}
	}
	if changes > 0 {
		logger.Info.Printf("Limiter apply: %d torrents changed", changes)
	}
}

// ApplyToTorrent 立即对指定 hash 应用当前配置
func (l *Limiter) ApplyToTorrent(hash, name string) {
	rules := l.cfg.Get().Limiter.Rules
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		ok, err := regexp.MatchString(r.Match, name)
		if err != nil || !ok {
			continue
		}
		var limit int64
		if r.UploadLimit > 0 {
			limit = int64(r.UploadLimit) * 1024
		} else {
			limit = -1
		}
		_ = l.client.SetUploadLimit(hash, limit)
		logger.Info.Printf("apply rule=%s to %s uploadLimit=%d", r.Name, name, r.UploadLimit)
		return
	}
}
