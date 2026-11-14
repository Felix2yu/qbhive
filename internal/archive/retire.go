package archive

import (
	"net/url"
	"strconv"
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
	defaultRetireInterval = 60
	defaultBatchSize      = 100
	// progressDone 与 filemgr 的完成判定同阈值：qB 的 progress 是浮点，
	// 完成的任务偶尔报 0.9999 而不是 1
	progressDone = 0.999
)

// FileState 是退休判定需要的 filemgr 视图：文件侧还有没有在跑或待落地的操作。
// 用接口而不是 *filemgr.Manager，是为了让退休矩阵测试能直接摆出各种收敛状态，
// 不必把清理/转码整条流水线跑一遍。
type FileState interface {
	Settled(hash string, quiet time.Duration) bool
	Idle(hash string) bool
}

// Retirer 退休扫描器：把完成并收敛的任务从 qB 移除（磁盘文件保留），
// 元数据先落账本，此后仍可检索。
//
// 自带 ticker，不挂在 filemgr 的扫描循环里：filemgr 的完成态白名单不含
// missingFiles，那 7000+ 条空壳根本进不了它的循环，退休必须自己扫。
type Retirer struct {
	cfg    *config.Manager
	client *qb.Client
	store  *Store
	files  FileState

	torrentCache *qb.TorrentCache

	stop       chan struct{}
	tickerStop chan struct{}

	mu     sync.Mutex
	notify func(title, body string)
}

func NewRetirer(cfg *config.Manager, client *qb.Client, store *Store, files FileState) *Retirer {
	return &Retirer{
		cfg:    cfg,
		client: client,
		store:  store,
		files:  files,
		stop:   make(chan struct{}),
	}
}

// SetTorrentCache 注入共享任务快照（装配层在 Start 之前调用一次）
func (r *Retirer) SetTorrentCache(c *qb.TorrentCache) { r.torrentCache = c }

// SetNotifier 注入通知通道，nil 时静默跳过
func (r *Retirer) SetNotifier(fn func(title, body string)) {
	r.mu.Lock()
	r.notify = fn
	r.mu.Unlock()
}

func (r *Retirer) SetClient(c *qb.Client) { r.client = c }

func (r *Retirer) interval() time.Duration {
	sec := r.cfg.Get().Archive.Interval
	if sec <= 0 {
		sec = defaultRetireInterval
	}
	return time.Duration(sec) * time.Second
}

func (r *Retirer) Start() {
	if !r.cfg.Get().Archive.Enabled {
		logger.Info.Println("归档退休已停用")
		return
	}
	logger.Info.Printf("归档退休启动，间隔=%s", r.interval())
	r.runTicker(r.interval())
}

func (r *Retirer) Reload(c *qb.Client) {
	if c != nil {
		r.client = c
	}
	if r.tickerStop != nil {
		close(r.tickerStop)
		r.tickerStop = nil
	}
	if !r.cfg.Get().Archive.Enabled {
		logger.Info.Println("归档退休已停用（重载）")
		return
	}
	logger.Info.Printf("归档退休重载，间隔=%s", r.interval())
	r.runTicker(r.interval())
}

func (r *Retirer) runTicker(interval time.Duration) {
	r.tickerStop = make(chan struct{})
	stop := r.tickerStop
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-r.stop:
				return
			case <-ticker.C:
				r.scan()
			}
		}
	}()
}

func (r *Retirer) Stop() { close(r.stop) }

// allTorrents 优先读共享快照，未就绪时回退直连
func (r *Retirer) allTorrents() ([]models.QBTorrent, error) {
	if r.torrentCache != nil {
		if list, err := r.torrentCache.GetTorrents("all", "", ""); err == nil {
			return list, nil
		}
	}
	return r.client.GetTorrents("all", "", "")
}

// scan 一轮退休：判定 → 先建档并 Sync → 再批量移除任务 → 回填退休时刻。
// 顺序不能反：账本写失败却删了任务，就是不可逆的元数据丢失。
func (r *Retirer) scan() {
	ac := r.cfg.Get().Archive
	if !ac.Enabled {
		return
	}
	list, err := r.allTorrents()
	if err != nil {
		logger.Warn.Printf("归档退休获取任务列表失败：%v", err)
		return
	}

	batchSize := ac.BatchSize
	if batchSize <= 0 {
		batchSize = defaultBatchSize
	}
	dryRun := ac.DryRun == nil || *ac.DryRun
	exempt := ac.ExemptLostFiles == nil || *ac.ExemptLostFiles
	quiet := time.Duration(ac.QuietMinutes) * time.Minute
	now := time.Now().Unix()

	var batch []string
	var recorded int
	for _, t := range list {
		if t.Hash == "" {
			continue
		}
		reason, ok := r.candidate(t, ac, exempt, quiet, now)
		if !ok {
			continue
		}
		rec, exists := r.store.Get(t.Hash)
		// 已退休但快照还残留着这条（快照最多旧 2s）：不再重复处理
		if exists && rec.RetiredAt > 0 {
			continue
		}
		if !exists {
			if err := r.store.Append(buildRecord(t, reason)); err != nil {
				// 写失败绝不删：这条留在 qB 里，下轮重试
				logger.Warn.Printf("归档退休建档失败，跳过 %s：%v", t.Name, err)
				continue
			}
			recorded++
		}
		if dryRun {
			continue
		}
		batch = append(batch, t.Hash)
		if len(batch) >= batchSize {
			break
		}
	}

	if len(batch) == 0 {
		if dryRun && recorded > 0 {
			logger.Info.Printf("归档退休干跑：本轮新建档 %d 条候选，未发出任何删除请求（账本共 %d 条）",
				recorded, r.store.Len())
			r.sendNotify("归档退休干跑",
				"本轮新增 "+strconv.Itoa(recorded)+" 条候选，未移除任何任务。核对历史页后关闭干跑即可开始退休。")
		}
		return
	}
	r.remove(batch)
}

// remove 提交一批移除并回填退休时刻。deleteFiles 恒 false：
// qB 的删除按元数据路径走、不看完成度，未转码任务的完好原片会被一把删光。
func (r *Retirer) remove(batch []string) {
	if err := r.client.DeleteTorrents(batch, false); err != nil {
		logger.Warn.Printf("归档退休移除 %d 条任务失败（文件未动，下轮重试）：%v", len(batch), err)
		return
	}
	ts := time.Now().Unix()
	for _, h := range batch {
		if err := r.store.MarkRetired(h, ts); err != nil {
			logger.Warn.Printf("归档退休回填时刻失败 %s：%v", h, err)
		}
	}
	logger.Info.Printf("归档退休：已移除 %d 条任务（磁盘文件保留），账本共 %d 条", len(batch), r.store.Len())
}

// ManualRetire 手动退休指定 hash（历史页的「确认移除」），绕过 dryRun。
// 只处理快照里还在的任务：已经不在 qB 里的没什么可移除的。
func (r *Retirer) ManualRetire(hashes []string) (int, error) {
	if len(hashes) == 0 {
		return 0, nil
	}
	list, err := r.allTorrents()
	if err != nil {
		return 0, err
	}
	want := make(map[string]bool, len(hashes))
	for _, h := range hashes {
		want[h] = true
	}
	var batch []string
	for _, t := range list {
		if !want[t.Hash] {
			continue
		}
		if rec, ok := r.store.Get(t.Hash); !ok {
			if err := r.store.Append(buildRecord(t, ReasonManual)); err != nil {
				logger.Warn.Printf("归档手动退休建档失败，跳过 %s：%v", t.Name, err)
				continue
			}
		} else if rec.RetiredAt > 0 {
			continue
		}
		batch = append(batch, t.Hash)
	}
	if len(batch) == 0 {
		return 0, nil
	}
	if err := r.client.DeleteTorrents(batch, false); err != nil {
		return 0, err
	}
	ts := time.Now().Unix()
	for _, h := range batch {
		if err := r.store.MarkRetired(h, ts); err != nil {
			logger.Warn.Printf("归档手动退休回填时刻失败 %s：%v", h, err)
		}
	}
	return len(batch), nil
}

// candidate 判定一条任务本轮能不能退休，返回退休理由。
//
//	失去实体：missingFiles/error 且有过完成时间 —— 原片已被转码清理，只剩空壳
//	健康完成：做种态且 progress 达标 —— 还要过做种门槛
//
// 两条路径都要求名单不跳过、文件侧已收敛。
func (r *Retirer) candidate(t models.QBTorrent, ac models.ArchiveConfig, exempt bool, quiet time.Duration, now int64) (string, bool) {
	if skip, _ := filemgr.MatchLists(t.Category, t.Tags,
		ac.CategoryInclude, ac.CategoryExclude, ac.TagInclude, ac.TagExclude); skip {
		return "", false
	}

	lost := (t.State == "missingFiles" || t.State == "error") && t.CompletedOn > 0
	healthy := isSeedingState(t.State) && t.Progress >= progressDone
	if !lost && !healthy {
		return "", false
	}

	if lost {
		// 失去实体的任务进不了 filemgr 的完成态白名单，多数根本没有记录，
		// 用 Settled 判会一条都退不掉；但仍可能有在跑的转码命令，所以看 Idle
		if !r.files.Idle(t.Hash) {
			return "", false
		}
		if exempt {
			return ReasonLostFiles, true
		}
	} else if !r.files.Settled(t.Hash, quiet) {
		return "", false
	}

	return seedingGate(t, ac, now)
}

// seedingGate 做种门槛：比率 / 时长 / 上传量三者任一达标即可。
// 门槛值 <=0 表示该判据不启用——老配置升级后三个都是 0，健康完成任务一条都不退。
func seedingGate(t models.QBTorrent, ac models.ArchiveConfig, now int64) (string, bool) {
	if ac.MinRatio > 0 && t.Ratio >= ac.MinRatio {
		return ReasonRatio, true
	}
	if ac.MinHours > 0 && t.CompletedOn > 0 && now-t.CompletedOn >= int64(ac.MinHours)*3600 {
		return ReasonHours, true
	}
	if ac.MinUploadedMB > 0 && t.Uploaded >= ac.MinUploadedMB*1024*1024 {
		return ReasonUploaded, true
	}
	return "", false
}

// isSeedingState 与 filemgr / scheduler 的完成态白名单保持一致。
// checkingUP（校验中）不算：此时磁盘状态不可信。
func isSeedingState(state string) bool {
	switch state {
	case "stoppedUP", "stalledUP", "uploading", "queuedUP", "forcedUP":
		return true
	}
	return false
}

func buildRecord(t models.QBTorrent, reason string) Record {
	return Record{
		Hash:        t.Hash,
		Name:        t.Name,
		Category:    t.Category,
		Tags:        t.Tags,
		SavePath:    t.SavePath,
		ContentPath: t.ContentPath,
		Tracker:     t.Tracker,
		Magnet:      BuildMagnet(t.Hash, t.Name, t.Tracker),
		Size:        t.Size,
		Uploaded:    t.Uploaded,
		AddedOn:     t.AddedOn,
		CompletedOn: t.CompletedOn,
		Ratio:       t.Ratio,
		Reason:      reason,
	}
}

// BuildMagnet 用 qB 已报的字段拼磁链，不调 torrents/trackers
// （7000+ 条就是 7000+ 次额外请求）。tracker 为空时只带 btih 与 dn。
func BuildMagnet(hash, name, tracker string) string {
	if hash == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("magnet:?xt=urn:btih:")
	b.WriteString(hash)
	if name != "" {
		b.WriteString("&dn=")
		b.WriteString(url.QueryEscape(name))
	}
	if tracker != "" {
		b.WriteString("&tr=")
		b.WriteString(url.QueryEscape(tracker))
	}
	return b.String()
}

func (r *Retirer) sendNotify(title, body string) {
	r.mu.Lock()
	fn := r.notify
	r.mu.Unlock()
	if fn != nil {
		fn(title, body)
	}
}
