package scheduler

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Felix2yu/qbhive/internal/config"
	"github.com/Felix2yu/qbhive/internal/filemgr"
	"github.com/Felix2yu/qbhive/internal/limiter"
	"github.com/Felix2yu/qbhive/internal/logger"
	"github.com/Felix2yu/qbhive/internal/models"
	"github.com/Felix2yu/qbhive/internal/notifier"
	qb "github.com/Felix2yu/qbhive/internal/qbittorrent"
	"github.com/Felix2yu/qbhive/internal/rss"
)

// Scheduler 是所有后台模块的统一调度入口，同时负责完成通知事件
type Scheduler struct {
	cfg      *config.Manager
	client   *qb.Client
	notifier *notifier.Notifier
	limiter  *limiter.Limiter
	fileMgr  *filemgr.Manager
	rssEngine *rss.Engine
	stop      chan struct{}
	mu        sync.Mutex
	finished  map[string]bool // 已经发过完成通知的 hash；持久化到文件
	stateFile string
}

// finished 记录文件（放在 config 同目录下，随容器卷一起持久化）
const finishedFile = "finished.json"

func New(cfg *config.Manager, client *qb.Client,
	notifier *notifier.Notifier,
	limiter *limiter.Limiter,
	fileMgr *filemgr.Manager,
	rssEngine *rss.Engine,
) *Scheduler {
	s := &Scheduler{
		cfg:       cfg,
		client:    client,
		notifier:  notifier,
		limiter:   limiter,
		fileMgr:   fileMgr,
		rssEngine: rssEngine,
		stop:      make(chan struct{}),
		finished:  make(map[string]bool),
	}
	// 推导状态文件路径（和 config.json 同目录）
	s.stateFile = defaultStatePath()
	s.loadFinished()
	return s
}

// ReloadAll 在配置变更后热更新所有后台模块：
//   - 用 SetCredentials 热替换 qB client 凭据（保留原指针，所有引擎自动跟进）
//   - 通知/限速/文件管理/RSS 按新 cfg 重新启停 ticker
//   - 调度器自身（完成通知扫描）会在读 cfg 时自动生效，无需重启
func (s *Scheduler) ReloadAll(qbCfg models.QBConfig) {
	// 1) qB client 热换凭据（Web Server 也持有同一个 *Client 指针，SetCredentials 即可）
	s.client.SetCredentials(qbCfg.URL, qbCfg.Username, qbCfg.Password, qbCfg.APIKey)

	// 2) 各子引擎 Reload（内部会根据新 cfg.Enabled 启停 ticker）
	s.limiter.Reload(nil)
	s.fileMgr.Reload(nil)
	s.rssEngine.Reload(nil)

	// 3) notifier 内部已经 Reload，在 web.server.saveConfig 里单独调
	logger.Info.Println("Scheduler: ReloadAll done (qb credentials + all subsystems)")
}

// defaultStatePath 返回 finished.json 的默认路径。
// 优先用 QBHIVE_CONFIG 环境变量所在的目录，没有就 ./data/finished.json
func defaultStatePath() string {
	if cfg := os.Getenv("QBHIVE_CONFIG"); cfg != "" {
		dir := filepath.Dir(cfg)
		return filepath.Join(dir, finishedFile)
	}
	return filepath.Join("data", finishedFile)
}

func (s *Scheduler) loadFinished() {
	data, err := os.ReadFile(s.stateFile)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Warn.Printf("scheduler: read finished state: %v", err)
		}
		return
	}
	var list []string
	if err := json.Unmarshal(data, &list); err != nil {
		logger.Warn.Printf("scheduler: parse finished state: %v", err)
		return
	}
	for _, h := range list {
		s.finished[h] = true
	}
	logger.Info.Printf("scheduler: loaded %d previously-notified hashes", len(list))
}

func (s *Scheduler) saveFinished() {
	s.mu.Lock()
	list := make([]string, 0, len(s.finished))
	for h := range s.finished {
		list = append(list, h)
	}
	s.mu.Unlock()
	data, _ := json.MarshalIndent(list, "", "  ")
	dir := filepath.Dir(s.stateFile)
	_ = os.MkdirAll(dir, 0o755)
	tmp := s.stateFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		logger.Warn.Printf("scheduler: write finished state: %v", err)
		return
	}
	_ = os.Rename(tmp, s.stateFile)
}

func (s *Scheduler) Start() {
	s.limiter.Start()
	s.fileMgr.Start()
	s.rssEngine.Start()

	// 启动后先扫一遍历史已完成任务，预填充 finished map，避免重启后给老任务重发通知
	go s.prefillFinished()

	// 完成通知：每 10 秒扫一次
	logger.Info.Println("Scheduler: notification scanner started")
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		// 启动 5 秒后先跑一次，后面按 ticker 来
		time.AfterFunc(5*time.Second, s.scanCompleted)
		for {
			select {
			case <-s.stop:
				return
			case <-ticker.C:
				s.scanCompleted()
			}
		}
	}()

	// 每 60 秒持久化一次 finished map（避免进程异常退出丢太多）
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-s.stop:
				s.saveFinished()
				return
			case <-ticker.C:
				s.saveFinished()
			}
		}
	}()
}

func (s *Scheduler) prefillFinished() {
	list, err := s.client.GetTorrents("completed", "", "")
	if err != nil {
		logger.Warn.Printf("scheduler: prefill get completed: %v", err)
		return
	}
	s.mu.Lock()
	n := 0
	for _, t := range list {
		// 只把 completed_on > 0 的（即真正完成过的）预填充进去，
		// uploading 状态还在做种但可能刚完成，不要预填充
		if t.CompletedOn > 0 {
			if !s.finished[t.Hash] {
				s.finished[t.Hash] = true
				n++
			}
		}
	}
	s.mu.Unlock()
	if n > 0 {
		logger.Info.Printf("scheduler: prefilled %d historical completed torrents into finished set", n)
		s.saveFinished()
	}
}

func (s *Scheduler) Stop() {
	close(s.stop)
	s.saveFinished()
	s.limiter.Stop()
	s.fileMgr.Stop()
	s.rssEngine.Stop()
}

func (s *Scheduler) scanCompleted() {
	cfg := s.cfg.Get()
	list, err := s.client.GetTorrents("all", "", "")
	if err != nil {
		logger.Warn.Printf("scheduler: scan all torrents failed: %v", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var (
		total     = len(list)
		nearDone  int
		newlyDone []models.QBTorrent
		// 诊断：nearDone 但没触发通知的原因统计
		stateMismatch int // progress>=0.98 但 state 不是 done 状态
		alreadyNotified int // 已经在 finished 里
		noCompletedOn int // completed_on=0
	)
	for _, t := range list {
		if t.Progress >= 0.98 {
			nearDone++
			// 诊断：记录 nearDone 但没被选中的原因
			if !isDoneState(t.State) {
				stateMismatch++
			} else if s.finished[t.Hash] {
				alreadyNotified++
			} else if t.CompletedOn == 0 {
				noCompletedOn++
			}
		}
		if t.Progress >= 0.98 && isDoneState(t.State) && !s.finished[t.Hash] {
			if t.CompletedOn > 0 {
				s.finished[t.Hash] = true
				newlyDone = append(newlyDone, t)
			} else {
				logger.Info.Printf("scheduler: skip %s (progress=%.4f state=%s completed_on=0)", t.Name, t.Progress, t.State)
			}
		}
	}

	// 每次扫描都输出诊断日志，方便排查
	logger.Info.Printf("scheduler: scan done — total=%d nearDone(>=0.98)=%d newlyDone=%d | skipped reasons: stateMismatch=%d alreadyNotified=%d noCompletedOn=%d",
		total, nearDone, len(newlyDone), stateMismatch, alreadyNotified, noCompletedOn)

	// 如果 nearDone > 0 但 newlyDone == 0 且 stateMismatch > 0，打印 nearDone torrent 的 state
	// 这是最常见的"静默失败"场景
	if nearDone > 0 && len(newlyDone) == 0 && stateMismatch > 0 {
		logger.Info.Printf("scheduler: found %d torrents with progress>=0.98 but non-done state — dumping details:", stateMismatch)
		for _, t := range list {
			if t.Progress >= 0.98 && !isDoneState(t.State) {
				logger.Info.Printf("  → %s | progress=%.4f | state=%s | completed_on=%d | hash=%s",
					t.Name, t.Progress, t.State, t.CompletedOn, t.Hash[:10])
			}
		}
	}

	if len(newlyDone) > 0 {
		for _, t := range newlyDone {
			logger.Info.Printf("completed: %s | size=%s | state=%s | category=%s",
				t.Name, humanSize(t.Size), t.State, t.Category)

			if cfg.Notifier.Enabled && len(cfg.Notifier.AppriseURLs) > 0 {
				title := fmt.Sprintf("✅ 下载完成 · %s", t.Name)
				body := buildCompletedBody(t)
				if err := s.notifier.Notify(title, body); err != nil {
					logger.Warn.Printf("scheduler: notify failed for %s: %v", t.Name, err)
				} else {
					logger.Info.Printf("scheduler: notification sent for %s", t.Name)
				}
			} else {
				logger.Warn.Printf("scheduler: notifier disabled or no URLs — skipping notification for %s", t.Name)
			}
		}
		s.saveFinished()
	}
}

// buildCompletedBody 构造通知正文，包含用户期望的所有字段：
// 任务名、大小、开始时间、完成时间、分类
func buildCompletedBody(t models.QBTorrent) string {
	var startedAt, finishedAt string
	if t.AddedOn > 0 {
		startedAt = time.Unix(t.AddedOn, 0).Format("2006-01-02 15:04:05")
	} else {
		startedAt = "-"
	}
	if t.CompletedOn > 0 {
		finishedAt = time.Unix(t.CompletedOn, 0).Format("2006-01-02 15:04:05")
	} else {
		finishedAt = "-"
	}

	return fmt.Sprintf(
		"📦 任务名: %s\n"+
			"💾 文件大小: %s\n"+
			"🗂️ 分类: %s\n"+
			"🕐 添加时间: %s\n"+
			"✅ 完成时间: %s\n"+
			"📁 保存路径: %s\n"+
			"🏷️ 标签: %s\n"+
			"🔗 Hash: %s",
		t.Name,
		humanSize(t.Size),
		t.Category,
		startedAt,
		finishedAt,
		t.SavePath,
		t.Tags,
		t.Hash,
	)
}

func isDoneState(state string) bool {
	// qBittorrent v5.0+ 状态：stoppedUP 是暂停且已完成，
	// 其他都是做种相关状态
	switch state {
	case "stoppedUP", "stalledUP", "uploading", "checkingUP", "queuedUP", "forcedUP":
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
