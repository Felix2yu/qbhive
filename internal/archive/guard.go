package archive

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Felix2yu/qbhive/internal/config"
	"github.com/Felix2yu/qbhive/internal/filemgr"
	"github.com/Felix2yu/qbhive/internal/logger"
	"github.com/Felix2yu/qbhive/internal/models"
	qb "github.com/Felix2yu/qbhive/internal/qbittorrent"
)

const (
	defaultGuardInterval = 5
	defaultMaxPerRound   = 20
	defaultGuardCooldown = 10
)

// FileDeleter 落地删除并写不可回退的审计（由 filemgr.Manager 实现）。
// 审计日志与通知通道都由 filemgr 持有，守卫不另起一套。
type FileDeleter interface {
	AuditGuardDelete(t models.QBTorrent, path string) bool
}

// Guard 重复下载守卫：定期比对 qB 现有任务与归档账本，发现「退休后又被重新添加」
// 的同一 infohash 就按档位处置。
//
// 走事后检测而不是添加时拦截：qB WebUI、原生客户端、RSS 三条添加路径都被覆盖，
// 用户不需要改在 qB 里加磁链的习惯。代价是检测窗口内会真的下几秒（见 README 已知限制）。
//
// 只读共享快照，5s 一轮不产生任何额外 qB 请求；只有命中候选时才发一次 torrents/files。
type Guard struct {
	cfg    *config.Manager
	client *qb.Client
	store  *Store
	files  FileDeleter

	torrentCache *qb.TorrentCache

	stop       chan struct{}
	tickerStop chan struct{}

	mu         sync.Mutex
	notify     func(title, body string)
	lastNotify map[string]int64 // hash → 上次告警时刻，用于冷却
	lastBurst  int64            // 上次熔断告警时刻
}

func NewGuard(cfg *config.Manager, client *qb.Client, store *Store, files FileDeleter) *Guard {
	return &Guard{
		cfg:        cfg,
		client:     client,
		store:      store,
		files:      files,
		stop:       make(chan struct{}),
		lastNotify: make(map[string]int64),
	}
}

// SetTorrentCache 注入共享任务快照。守卫没有直连兜底：未注入就完全不扫。
func (g *Guard) SetTorrentCache(c *qb.TorrentCache) { g.torrentCache = c }

func (g *Guard) SetNotifier(fn func(title, body string)) {
	g.mu.Lock()
	g.notify = fn
	g.mu.Unlock()
}

func (g *Guard) SetClient(c *qb.Client) { g.client = c }

func (g *Guard) interval() time.Duration {
	sec := g.cfg.Get().Archive.Guard.Interval
	if sec <= 0 {
		sec = defaultGuardInterval
	}
	return time.Duration(sec) * time.Second
}

func (g *Guard) cooldown() time.Duration {
	min := g.cfg.Get().Archive.Guard.NotifyCooldownMinutes
	if min <= 0 {
		min = defaultGuardCooldown
	}
	return time.Duration(min) * time.Minute
}

// activeMode 只认三个会告警或动手的档位。off、空值与未知值一律不扫：
// config 会把未知值归一成 off，但配置也可能被直接 Set 进来（设置页保存），
// 打错一个字就开始删任务是不可接受的。
func activeMode(mode string) bool {
	switch mode {
	case models.GuardModeNotify, models.GuardModeRemove, models.GuardModeRemoveFiles:
		return true
	}
	return false
}

func (g *Guard) enabled() bool {
	return activeMode(g.cfg.Get().Archive.Guard.Mode)
}

func (g *Guard) Start() {
	if !g.enabled() {
		logger.Info.Println("重复下载守卫已停用")
		return
	}
	logger.Info.Printf("重复下载守卫启动，档位=%s 间隔=%s",
		g.cfg.Get().Archive.Guard.Mode, g.interval())
	g.runTicker(g.interval())
}

func (g *Guard) Reload(c *qb.Client) {
	if c != nil {
		g.client = c
	}
	if g.tickerStop != nil {
		close(g.tickerStop)
		g.tickerStop = nil
	}
	if !g.enabled() {
		logger.Info.Println("重复下载守卫已停用（重载）")
		return
	}
	logger.Info.Printf("重复下载守卫重载，档位=%s 间隔=%s",
		g.cfg.Get().Archive.Guard.Mode, g.interval())
	g.runTicker(g.interval())
}

func (g *Guard) runTicker(interval time.Duration) {
	g.tickerStop = make(chan struct{})
	stop := g.tickerStop
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-g.stop:
				return
			case <-ticker.C:
				g.scan()
			}
		}
	}()
}

func (g *Guard) Stop() { close(g.stop) }

type dupHit struct {
	t   models.QBTorrent
	rec Record
}

// scan 一轮检测。快照未就绪就跳过本轮，绝不直连兜底——
// 5s 一次直连全表是 20.6MB × 12 次/分，比要修的问题更糟。
func (g *Guard) scan() {
	gc := g.cfg.Get().Archive.Guard
	if !activeMode(gc.Mode) {
		return
	}
	if g.torrentCache == nil {
		return
	}
	list, err := g.torrentCache.GetTorrents("all", "", "")
	if err != nil {
		return
	}

	now := time.Now().Unix()
	var hits []dupHit
	for _, t := range list {
		if t.Hash == "" {
			continue
		}
		rec, ok := g.store.Get(t.Hash)
		// RetiredAt==0 是干跑候选或正在退休：任务本来就该在 qB 里，不是重复下载。
		// 这一条同时也挡住了「退休批次进行中」的窗口，不需要另维护在途集合。
		if !ok || rec.RetiredAt <= 0 {
			continue
		}
		// 严格大于：qB 的 added_on 与 RetiredAt 都是秒刻度，用 >= 会让
		// 「添加与退休落在同一秒」的任务在快照残留窗口里被当成重复下载，
		// 进而删掉它刚下完的文件。宁可漏检一次重下，也不能误删。
		if t.AddedOn <= rec.RetiredAt {
			continue
		}
		// 校验中/移动中：progress 与磁盘状态都不可信，等下一轮
		if isVolatileState(t.State) {
			continue
		}
		hits = append(hits, dupHit{t: t, rec: rec})
	}
	if len(hits) == 0 {
		return
	}

	maxPerRound := gc.MaxPerRound
	if maxPerRound <= 0 {
		maxPerRound = defaultMaxPerRound
	}
	// 单轮熔断：候选异常多通常是账本被误导入，此时批量删文件不可接受
	if len(hits) > maxPerRound {
		logger.Warn.Printf("重复守卫：本轮候选 %d 条超过上限 %d，只告警不动手", len(hits), maxPerRound)
		g.notifyBurst(now, len(hits), maxPerRound)
		return
	}
	for _, h := range hits {
		g.act(h, gc, now)
	}
}

// act 按档位处置一条命中。notify 档什么都不动，就是干跑，用它核对判定准不准。
func (g *Guard) act(h dupHit, gc models.GuardConfig, now int64) {
	t := h.t
	body := dupBody(t, h.rec)

	if gc.Mode == models.GuardModeNotify {
		logger.Info.Printf("重复守卫 [%s] 命中归档（完成于 %d，本次添加于 %d），仅告警",
			t.Name, h.rec.CompletedOn, t.AddedOn)
		g.notifyOnce(t.Hash, now, "QBHive 发现重复下载", body+"\n处置：仅告警，任务与文件均未改动。")
		return
	}

	var deleted []string
	var blind bool
	if gc.Mode == models.GuardModeRemoveFiles {
		// 必须先取文件列表再移除任务：任务没了元数据也就没了
		deleted, blind = g.pruneFiles(t)
	}
	if err := g.client.DeleteTorrents([]string{t.Hash}, false); err != nil {
		logger.Warn.Printf("重复守卫 [%s] 移除任务失败：%v", t.Name, err)
		return
	}
	logger.Info.Printf("重复守卫 [%s] 已移除任务（档位 %s，删除文件 %d 个）", t.Name, gc.Mode, len(deleted))

	switch gc.Mode {
	case models.GuardModeRemoveFiles:
		body += "\n处置：已移除任务，并清掉本次重下写出的文件"
		if len(deleted) > 0 {
			body += "：\n" + strings.Join(deleted, "\n")
		} else {
			body += "（一个都没删）"
		}
		if blind {
			body += "\n注意：qbhive 看不到该任务的存储路径，已降级为只移除任务。"
		}
	case models.GuardModeRemove:
		body += "\n处置：已移除任务，磁盘文件保留（本次重下的半成品会留在盘上）。"
	}
	g.notifyOnce(t.Hash, now, "QBHive 拦下重复下载", body)
}

// pruneFiles 清掉「本次重下写出来的」文件，返回删掉的路径与是否完全看不到存储。
//
// 不用 qB 的 deleteFiles：它按元数据路径删、不看完成度，未转码任务的完好原片会被一把删光。
// 逐个文件过四道闸门，任一不过就不动它。
func (g *Guard) pruneFiles(t models.QBTorrent) (deleted []string, blind bool) {
	files, err := g.client.GetTorrentFiles(t.Hash)
	if err != nil {
		logger.Warn.Printf("重复守卫 [%s] 取文件列表失败，降级为只移除任务：%v", t.Name, err)
		return nil, true
	}
	added := time.Unix(t.AddedOn, 0)
	var statOK int
	for _, f := range files {
		name := filepath.ToSlash(f.Name)
		cands := []string{name}
		// qB 可能仍用未完成临时名报告，也可能已报最终名：两个候选都过同一套闸门
		if trimmed := strings.TrimSuffix(name, filemgr.PartialFileSuffix); trimmed != name {
			cands = append(cands, trimmed)
		}
		for _, cand := range cands {
			p, ok := filemgr.SafeTargetPath(t.SavePath, cand)
			if !ok {
				continue // 种子自带的越界文件名：永久忽略
			}
			fi, err := os.Stat(p)
			if err != nil {
				continue
			}
			statOK++
			if !fi.Mode().IsRegular() {
				break // 绝不删目录，绝不 RemoveAll
			}
			// 只认「不早于本次添加时刻」的文件，证明文件是这次重下写出来的。
			// 与 postPrune 同一套判据、同一个失败方向：粗粒度文件系统把新文件
			// mtime 向下取整 → 看起来更旧 → 被滤掉 → 留下孤儿半成品，
			// 而不是误删上一轮留下的完好原片。
			if fi.ModTime().Before(added) {
				break
			}
			if g.files.AuditGuardDelete(t, p) {
				deleted = append(deleted, p)
			}
			break
		}
	}
	return deleted, len(files) > 0 && statOK == 0
}

// isVolatileState 校验中/移动中：此时 progress 与磁盘状态都不可信
func isVolatileState(state string) bool {
	switch state {
	case "checkingDL", "checkingUP", "checkingResumeData", "moving":
		return true
	}
	return false
}

func dupBody(t models.QBTorrent, rec Record) string {
	var b strings.Builder
	fmt.Fprintf(&b, "任务「%s」的 infohash 已在归档里。", t.Name)
	appendLine(&b, "归档完成时间", fmtTS(rec.CompletedOn))
	appendLine(&b, "归档退休时间", fmtTS(rec.RetiredAt))
	appendLine(&b, "本次添加时间", fmtTS(t.AddedOn))
	appendLine(&b, "退休理由", rec.Reason)
	appendLine(&b, "保存路径", t.SavePath)
	return b.String()
}

func appendLine(b *strings.Builder, label, val string) {
	if val == "" {
		return
	}
	b.WriteString("\n")
	b.WriteString(label)
	b.WriteString("：")
	b.WriteString(val)
}

func fmtTS(ts int64) string {
	if ts <= 0 {
		return ""
	}
	return time.Unix(ts, 0).Format("2006-01-02 15:04:05")
}

// notifyOnce 同一 hash 在冷却期内只告警一次。冷却只压制通知，不压制处置动作。
func (g *Guard) notifyOnce(hash string, now int64, title, body string) {
	g.mu.Lock()
	last := g.lastNotify[hash]
	fn := g.notify
	if now-last < int64(g.cooldown().Seconds()) {
		fn = nil
	} else {
		g.lastNotify[hash] = now
	}
	g.mu.Unlock()
	if fn != nil {
		fn(title, body)
	}
}

func (g *Guard) notifyBurst(now int64, n, max int) {
	g.mu.Lock()
	fn := g.notify
	if now-g.lastBurst < int64(g.cooldown().Seconds()) {
		fn = nil
	} else {
		g.lastBurst = now
	}
	g.mu.Unlock()
	if fn == nil {
		return
	}
	fn("QBHive 重复守卫熔断",
		fmt.Sprintf("本轮发现 %d 条重复下载候选，超过单轮上限 %d，已停止处置只告警。\n"+
			"这通常意味着账本被误导入。请核对历史页，必要时调大 guard.maxPerRound。", n, max))
}
