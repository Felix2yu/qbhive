package scheduler

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	logger.Info.Println("调度器：ReloadAll 完成（qB 凭据 + 所有子系统）")
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
			logger.Warn.Printf("调度器读取已完成状态失败：%v", err)
		}
		return
	}
	var list []string
	if err := json.Unmarshal(data, &list); err != nil {
		logger.Warn.Printf("调度器解析已完成状态失败：%v", err)
		return
	}
	for _, h := range list {
		s.finished[h] = true
	}
	logger.Info.Printf("调度器已加载 %d 条历史已通知哈希", len(list))
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
		logger.Warn.Printf("调度器写入已完成状态失败：%v", err)
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
	logger.Info.Println("调度器：完成通知扫描器已启动")
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
	// 拉 "all" 而不是 "completed"：某些 qB 版本的 "completed" 过滤器
	// 只按 CompletedOn > 0 过滤，会漏掉 Progress=1.0 但 CompletedOn=0 的任务。
	list, err := s.client.GetTorrents("all", "", "")
	if err != nil {
		logger.Warn.Printf("调度器预填充获取已完成任务失败：%v", err)
		return
	}
	s.mu.Lock()
	n := 0
	for _, t := range list {
		// 和 scanCompleted 用完全相同的判断：Progress + State，不依赖 CompletedOn。
		// 这样无论 qB 有没有填 completion_on，真正完成过的任务都能被预填充进去，
		// 避免启动后第一次 scanCompleted 把老任务误判为"新完成"而错发通知。
		if t.Progress >= 0.98 && isDoneState(t.State) {
			if !s.finished[t.Hash] {
				s.finished[t.Hash] = true
				n++
			}
		}
	}
	s.mu.Unlock()
	if n > 0 {
		logger.Info.Printf("调度器已预填充 %d 条历史已完成任务到已完成集合", n)
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
		logger.Warn.Printf("调度器扫描全部任务失败：%v", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var (
		total     = len(list)
		nearDone  int
		newlyDone []models.QBTorrent
		// 诊断：nearDone 但没触发通知的原因统计
		stateMismatch   int // progress>=0.98 但 state 不是 done 状态
		alreadyNotified int // 已经在 finished 里
		noCompletedOn   int // completion_on=0（添加时即完成等场景 qB 不填时间戳）
	)
	for _, t := range list {
		if t.Progress >= 0.98 {
			nearDone++
			switch {
			case !isDoneState(t.State):
				stateMismatch++
			case s.finished[t.Hash]:
				alreadyNotified++
			case t.CompletedOn == 0:
				noCompletedOn++
			}
		}
		if t.Progress >= 0.98 && isDoneState(t.State) && !s.finished[t.Hash] {
			// 注意：不要求 CompletedOn > 0。qBittorrent 在"添加时数据已完整、
			// 跳过校验直接完成"等场景 completion_on 不会填上（保持 0）。
			// 但 Progress + State 本身就足够判定"已完成"了。
			// finished map 保证了不会重复通知。
			s.finished[t.Hash] = true
			newlyDone = append(newlyDone, t)
		}
	}

	// 每次扫描输出汇总日志（一行搞定，6000 条任务也只打一行）
	logger.Info.Printf("调度器：扫描完成 — 总数=%d 接近完成(>=0.98)=%d 新完成=%d | 跳过原因：状态不匹配=%d 已通知=%d completion_on=0=%d",
		total, nearDone, len(newlyDone), stateMismatch, alreadyNotified, noCompletedOn)

	// 诊断 dump（默认不输出，只有 DEBUG 级别或状态异常时才输出，且有数量上限）
	if nearDone > 0 && len(newlyDone) == 0 && stateMismatch > 0 {
		const dumpLimit = 20
		logger.Debug.Printf("调度器：状态不匹配详情（最多显示 %d / %d）：", dumpLimit, stateMismatch)
		dumped := 0
		for _, t := range list {
			if t.Progress >= 0.98 && !isDoneState(t.State) {
				dumped++
				logger.Debug.Printf("  → 名称=%s 进度=%.4f 状态=%s 完成时间戳=%d 哈希=%s",
					t.Name, t.Progress, t.State, t.CompletedOn, t.Hash[:10])
				if dumped >= dumpLimit {
					break
				}
			}
		}
	}

	if len(newlyDone) > 0 {
		for _, t := range newlyDone {
			logger.Info.Printf("下载完成：名称=%s 大小=%s 状态=%s 分类=%s",
				t.Name, humanSize(t.Size), t.State, t.Category)

			// 兜底：qBittorrent 在"添加时数据已完整、跳过校验直接完成"等场景
			// 不填 completion_on，此时用检测到完成的时刻近似（扫描间隔 10 秒，
			// 误差可忽略），避免通知里完成时间显示 "-"
			if t.CompletedOn == 0 {
				t.CompletedOn = time.Now().Unix()
			}

			if cfg.Notifier.Enabled && len(cfg.Notifier.AppriseURLs) > 0 {
				title := fmt.Sprintf("✅ 下载完成 · %s", t.Name)
				body := buildCompletedBody(t, cfg.Notifier.Fields)
				if err := s.notifier.Notify(title, body); err != nil {
					logger.Warn.Printf("调度器：通知发送失败 %s：%v", t.Name, err)
				} else {
					logger.Info.Printf("调度器：通知已发送 %s", t.Name)
				}
			} else {
				logger.Warn.Printf("调度器：通知器未启用或未配置 URL — 跳过通知 %s", t.Name)
			}
		}
		s.saveFinished()
	}
}

// buildCompletedBody 构造通知正文，只包含 fields 勾选的字段（key 见
// models.NotifyFields），行顺序与该清单一致；fields 为空表示全部发送。
func buildCompletedBody(t models.QBTorrent, fields []string) string {
	selected := make(map[string]bool, len(models.NotifyFields))
	if len(fields) == 0 {
		for _, f := range models.NotifyFields {
			selected[f.Key] = true
		}
	} else {
		for _, k := range fields {
			selected[k] = true
		}
	}

	startedAt, finishedAt := "-", "-"
	if t.AddedOn > 0 {
		startedAt = time.Unix(t.AddedOn, 0).Format("2006-01-02 15:04:05")
	}
	if t.CompletedOn > 0 {
		finishedAt = time.Unix(t.CompletedOn, 0).Format("2006-01-02 15:04:05")
	}

	var lines []string
	add := func(key, line string) {
		if selected[key] {
			lines = append(lines, line)
		}
	}
	add("name", fmt.Sprintf("📦 任务名: %s", t.Name))
	add("size", fmt.Sprintf("💾 文件大小: %s", humanSize(t.Size)))
	add("category", fmt.Sprintf("🗂️ 分类: %s", t.Category))
	add("addedOn", fmt.Sprintf("🕐 添加时间: %s", startedAt))
	add("completedOn", fmt.Sprintf("✅ 完成时间: %s", finishedAt))
	add("savePath", fmt.Sprintf("📁 保存路径: %s", t.SavePath))
	add("tags", fmt.Sprintf("🏷️ 标签: %s", t.Tags))
	add("hash", fmt.Sprintf("🔗 Hash: %s", t.Hash))
	return strings.Join(lines, "\n")
}

func isDoneState(state string) bool {
	// qBittorrent 5.x 状态：stoppedUP 是已停止且已完成，
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
