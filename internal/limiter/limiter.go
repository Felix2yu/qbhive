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
}

func New(cfg *config.Manager, client *qb.Client) *Limiter {
	return &Limiter{cfg: cfg, client: client, stop: make(chan struct{})}
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
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
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
	torrents, err := l.client.GetTorrents()
	if err != nil {
		logger.Warn.Printf("Limiter get torrents failed: %v", err)
		return
	}
	for _, t := range torrents {
		for _, r := range rules {
			if !r.Enabled {
				continue
			}
			ok, err := regexp.MatchString(r.Match, t.Name)
			if err != nil {
				continue
			}
			if ok {
				var limit int64
				if r.UploadLimit > 0 {
					limit = int64(r.UploadLimit) * 1024
				} else {
					limit = -1 // 无限制
				}
				_ = l.client.SetUploadLimit(t.Hash, limit)
				logger.Debug.Printf("limiter: torrent=%s matched rule=%s limit=%d", t.Name, r.Name, limit)
				break
			}
		}
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
