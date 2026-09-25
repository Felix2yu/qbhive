package scheduler

import (
	"fmt"
	"os"
	"time"

	"github.com/Felix2yu/qbhive/internal/config"
	"github.com/Felix2yu/qbhive/internal/filemgr"
	"github.com/Felix2yu/qbhive/internal/limiter"
	"github.com/Felix2yu/qbhive/internal/logger"
	"github.com/Felix2yu/qbhive/internal/notifier"
	qb "github.com/Felix2yu/qbhive/internal/qbittorrent"
	"github.com/Felix2yu/qbhive/internal/rss"
)

// Scheduler 是所有后台模块的统一调度入口，同时负责完成通知事件
type Scheduler struct {
	cfg       *config.Manager
	client    *qb.Client
	notifier  *notifier.Notifier
	limiter   *limiter.Limiter
	fileMgr   *filemgr.Manager
	rssEngine *rss.Engine
	stop      chan struct{}
	finished  map[string]bool
}

func New(cfg *config.Manager, client *qb.Client,
	notifier *notifier.Notifier,
	limiter *limiter.Limiter,
	fileMgr *filemgr.Manager,
	rssEngine *rss.Engine,
) *Scheduler {
	return &Scheduler{
		cfg:       cfg,
		client:    client,
		notifier:  notifier,
		limiter:   limiter,
		fileMgr:   fileMgr,
		rssEngine: rssEngine,
		stop:      make(chan struct{}),
		finished:  make(map[string]bool),
	}
}

func (s *Scheduler) Start() {
	s.limiter.Start()
	s.fileMgr.Start()
	s.rssEngine.Start()

	// 完成通知：每 10 秒扫一次
	logger.Info.Println("Scheduler: notification scanner started")
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-ticker.C:
				s.scanCompleted()
			}
		}
	}()
}

func (s *Scheduler) Stop() {
	close(s.stop)
	s.limiter.Stop()
	s.fileMgr.Stop()
	s.rssEngine.Stop()
}

func (s *Scheduler) scanCompleted() {
	cfg := s.cfg.Get()
	list, err := s.client.GetTorrents()
	if err != nil {
		logger.Warn.Printf("scan completed: %v", err)
		return
	}
	for _, t := range list {
		if t.Progress < 0.999 {
			continue
		}
		if !isDoneState(t.State) {
			continue
		}
		if s.finished[t.Hash] {
			continue
		}
		s.finished[t.Hash] = true
		logger.Info.Printf("completed: %s", t.Name)

		// 发送通知
		if cfg.Notifier.Enabled && len(cfg.Notifier.AppriseURLs) > 0 {
			title := fmt.Sprintf("下载完成: %s", t.Name)
			body := fmt.Sprintf("大小: %s\n分类: %s\n保存路径: %s",
				humanSize(t.Size), t.Category, t.SavePath)
			if err := s.notifier.Notify(title, body); err != nil {
				logger.Warn.Printf("notify failed: %v", err)
			}
		}
	}
}

func isDoneState(state string) bool {
	switch state {
	case "pausedUP", "stalledUP", "uploading", "checkingUP", "queuedUP":
		return true
	}
	return false
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// 让 linter 满意
var _ = os.Getenv
