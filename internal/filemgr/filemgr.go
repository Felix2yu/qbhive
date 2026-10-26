package filemgr

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
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

// Manager 监听完成事件并处理单文件场景
type Manager struct {
	cfg    *config.Manager
	client *qb.Client
	stop   chan struct{}

	// 运行中的 ticker 控制
	tickerStop chan struct{}

	// 上一周期已完成的 hash 集合，避免重复通知/处理
	done map[string]bool

	// ---- 文件名自动清理（clean pass）----
	mu         sync.Mutex
	statePath  string                  // data/filemgr_clean.json
	cleanState map[string]*cleanRecord // hash → 清理进度（持久化，重启不丢）
	rulesCache []*regexp.Regexp        // 自定义规则编译缓存
	rulesKey   string                  // rulesCache 对应的规则指纹
	audit      *auditLog               // 重命名审计日志

	// notify 由装配层注入的通知通道（nil 表示不通知）。
	// 只在「需要人介入」或「不可逆」的事件上调用：归档本地回退、失联回滚、清理删原片、
	// 批量重命名完成、审计回退。常规成功路径不发通知，避免把用户淹在消息里。
	notify func(title, body string)

	// ---- 转码后处理 ----
	// postRunMu 保护 postBusy：全局串行闸门，一次只允许一条后处理命令在跑
	// （转码吃满 CPU 与磁盘带宽，并发跑只会互相拖慢）
	postRunMu sync.Mutex
	postBusy  bool
	// postDirty 本轮扫描里改过过滤状态（挡掉 / 因改名单解除终结），
	// 攒到扫描结束再一次性落盘：一次配置改动会同时影响几百个历史任务，
	// 逐条 saveCleanState 就是几百次全量重写 + rename
	postDirty bool
	// postSig 上一轮看到的分类/标签名单指纹（仅 scan goroutine 读写，无并发）
	postSig string
	// postSkipped 本轮被分类/标签过滤挡掉的任务数（仅 scan goroutine 读写），
	// 扫描结束时汇成一条 Info 日志，避免一次改动几百个任务时逐条刷屏
	postSkipped int
	// cleanAbandoned 本轮「磁盘上长期找不到实体、按脱钩放弃」的任务数（仅 scan goroutine 读写）。
	// 历史库里僵尸任务可能成百上千，逐条通知会把用户淹掉，扫描结束汇成一条。
	cleanAbandoned int
}

// cleanRecord 单个任务的清理进度
type cleanRecord struct {
	Pending    []cleanRename `json:"pending,omitempty"` // 已提交待确认落地的重命名
	Done       bool          `json:"done,omitempty"`    // 已无待办且全部落地
	AiAttempts int           `json:"aiAttempts,omitempty"`
	AISettled  bool          `json:"aiSettled,omitempty"` // AI 已成功调用过 / 重试耗尽
	// AISettledAt 记录「因连续失败而降级」的时刻（Unix 秒）。
	// 0 表示 AI 已成功调用过这类终态，永不重武装；非 0 表示失败降级，
	// 冷却 aiRearmAfter 后自动重试，避免一次瞬时故障让任务永久失去 AI 美化。
	AISettledAt int64 `json:"aiSettledAt,omitempty"`

	// CleanSig 记录本任务判定 Done 时生效的清理规则指纹（内置版本+自定义正则+AI 开关）。
	// 指纹一变（用户新增自定义正则、启用 AI、内置规则升级）就解除 Done，
	// 让历史任务按新规则重新评估，否则这些改动对已扫描过的任务永远不生效。
	CleanSig string `json:"cleanSig,omitempty"`

	// FlattenDir 记录单文件归档已提交 qB 异步移动、等待清空后删除的原目录（绝对路径）。
	// 非空表示上一轮已把 savePath 子目录里唯一的文件移到保存根，qB 的磁盘移动是异步的，
	// 必须等原目录真正变空才能删，否则会连未移走的文件一起删掉。持久化以便重启后续清。
	FlattenDir string `json:"flattenDir,omitempty"`
	// FlattenAttempts 归档清理连续等待/失败的轮数，超过上限则放弃（绝不删非空目录）
	FlattenAttempts int `json:"flattenAttempts,omitempty"`

	// ---- 下载完成后处理（外部命令，如调用 macOS 快捷指令转码）----
	// PostDone 该任务已无待转文件（含未开启之外的所有终结情形），
	// 持久化避免重启后重复解析、重复派发
	PostDone bool `json:"postDone,omitempty"`
	// PostDispatched 已交给 shell 的目标文件绝对路径（含失败的），文件级去重用：
	// 一个种子里多个视频逐个派发，重启也不会把跑到一半的那个再转一遍
	PostDispatched []string `json:"postDispatched,omitempty"`
	// PostWaits 命中白名单的文件连续几轮都取不到（qB 临时名 / 磁盘上还没有），
	// 超过 postMaxWaits 才认定这个任务确实没有可转的文件
	PostWaits int `json:"postWaits,omitempty"`
	// PostFilterList 记下「本任务是被分类/标签过滤挡掉的」以及当时生效的名单指纹。
	// 非空表示终结原因是过滤而非「转完了」：名单一改（指纹变了）就解除终结、重新判定，
	// 用户先过滤错、之后放宽名单时，历史任务能补转，不用手改状态文件。
	// 正常转码完成的任务这里为空，改名单不会把它们重新拖回转码。
	PostFilterList string `json:"postFilterList,omitempty"`
	// PostCleanWaits 转码因「文件名清理未收敛」而让路的累计轮数。到 postCleanMaxWaits
	// 就放行并告警一次：清理卡住不能让转码永久停摆。清理一旦收敛即清零。
	PostCleanWaits int `json:"postCleanWaits,omitempty"`
	// PostCleanWarned 已就「清理未收敛仍派发转码」告警过，避免每轮刷通知
	PostCleanWarned bool `json:"postCleanWarned,omitempty"`
	// UnresolvedCleans 「qB 报了名字但磁盘上找不到实体」的连续轮数。到上限按脱钩放弃，
	// 不落盘（只在内存里推进，重启归零），与 PostWaits 同一套思路。
	UnresolvedCleans int `json:"unresolvedCleans,omitempty"`
}

const cleanStateFile = "filemgr_clean.json"

// flattenMaxAttempts 等待 qB 异步移动落地、清理归档残留目录的最大轮数。
// 同一卷内移动是元数据操作（秒级），20 轮（默认间隔约 5 分钟）足够；
// 超过说明移动异常，放弃清理但绝不删除仍有文件的目录。
const flattenMaxAttempts = 20

// cleanUnresolvedMaxWaits 「qB 报了名字、磁盘上找不到实体」连续多少轮后按存储脱钩放弃。
// 这些文件下一轮可能就能洗（下载完成去掉 .!qB、转码产物落盘），所以不能立刻判完成；
// 但真脱钩的任务每轮都找不到，无限重试只会空转，到上限就放弃并告警一次。
const cleanUnresolvedMaxWaits = 20

// defaultCleanStatePath 推导清理状态文件路径（与 scheduler 的 finished.json 同目录规则）
func defaultCleanStatePath() string {
	if cfg := os.Getenv("QBHIVE_CONFIG"); cfg != "" {
		return filepath.Join(filepath.Dir(cfg), cleanStateFile)
	}
	return filepath.Join("data", cleanStateFile)
}

func New(cfg *config.Manager, client *qb.Client) *Manager {
	m := &Manager{
		cfg:        cfg,
		client:     client,
		stop:       make(chan struct{}),
		done:       make(map[string]bool),
		statePath:  defaultCleanStatePath(),
		cleanState: make(map[string]*cleanRecord),
	}
	m.loadCleanState()
	m.audit = newAuditLog(defaultAuditPath())
	return m
}

// SetClient 热替换 qb 客户端
func (m *Manager) SetClient(c *qb.Client) {
	m.client = c
}

// SetNotifier 注入通知通道（由装配层传入 notifier.Notify）。
// filemgr 不直接依赖 notifier 包：这里只留一个函数值，nil 时通知静默跳过。
func (m *Manager) SetNotifier(fn func(title, body string)) {
	m.mu.Lock()
	m.notify = fn
	m.mu.Unlock()
}

// notifyEvent 发送一条通知（未注入通道时静默跳过，绝不因通知失败影响主流程）
func (m *Manager) notifyEvent(title, body string) {
	m.mu.Lock()
	fn := m.notify
	m.mu.Unlock()
	if fn == nil {
		return
	}
	fn(title, body)
}

// Reload 根据最新配置重启（或停止）扫描轮询
func (m *Manager) Reload(c *qb.Client) {
	if c != nil {
		m.client = c
	}
	if m.tickerStop != nil {
		close(m.tickerStop)
		m.tickerStop = nil
	}
	cfg := m.cfg.Get().FileManager
	if !m.scanEnabled() {
		logger.Info.Println("文件管理已停用（重载）")
		return
	}
	interval := time.Duration(cfg.ScanInterval) * time.Second
	if interval <= 0 {
		interval = 15 * time.Second
	}
	logger.Info.Printf("文件管理重载，间隔=%s", interval)
	m.runTicker(interval)
}

// scanEnabled 是否需要跑扫描循环：自动归档或后处理任一开启即可（后处理单独开启时
// 也要扫描，否则归挡关掉后转码命令永远不会触发）
func (m *Manager) scanEnabled() bool {
	c := m.cfg.Get().FileManager
	return c.Enabled || c.PostProcess.Enabled
}

func (m *Manager) Start() {
	c := m.cfg.Get().FileManager
	if !m.scanEnabled() {
		if c.PostProcess.Enabled {
			logger.Info.Println("文件管理停用，但后处理已开启，扫描循环保持运行")
		} else {
			logger.Info.Println("文件管理已停用")
		}
		return
	}
	interval := time.Duration(c.ScanInterval) * time.Second
	if interval <= 0 {
		interval = 15 * time.Second
	}
	logger.Info.Printf("文件管理启动，间隔=%s", interval)
	m.runTicker(interval)
}

func (m *Manager) runTicker(interval time.Duration) {
	m.tickerStop = make(chan struct{})
	stop := m.tickerStop
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-m.stop:
				return
			case <-ticker.C:
				m.scan()
			}
		}
	}()
}

func (m *Manager) Stop() {
	close(m.stop)
	m.saveCleanState()
}

func (m *Manager) scan() {
	list, err := m.client.GetTorrents()
	if err != nil {
		logger.Warn.Printf("文件管理获取任务列表失败：%v", err)
		return
	}
	m.ensurePostFilterRescan(m.cfg.Get().FileManager.PostProcess)
	for _, t := range list {
		// clean pass 先于 flatten：文件名清理独立于单文件归档，
		// flatten 已 done 的任务照样进入清理（文件躺在 save_path 根也常带域名）
		if m.cfg.Get().FileManager.CleanEnabled {
			m.handleClean(t)
		}
		if m.done[t.Hash] {
			continue
		}
		if t.Progress < 0.999 {
			continue
		}
		// 处理所有已完成的做种态（与 scheduler.isDoneState 保持一致）。
		// 不能只认 stoppedUP：未配置「完成即暂停」的任务完成后是
		// uploading/stalledUP/queuedUP/forcedUP，只认 stoppedUP 永远轮不到。
		// checkingUP（校验中）移动文件有风险，跳过且不标 done，校验结束后下轮处理。
		switch t.State {
		case "stoppedUP", "stalledUP", "uploading", "queuedUP", "forcedUP":
		default:
			continue
		}
		// done 标记在 handleCompleted 确认后设置，避免早返回导致永久跳过。
		// 后处理必须排在归档之后：归档走 qB renameFile 时磁盘移动是异步的、
		// 且会把文件挪到落点，只有归档落地（didWork/already）才说明目标路径稳定，
		// 此时交给后处理的才是最终文件。
		already, didWork := m.handleCompleted(t)
		done := already || didWork
		if m.postEnabled() && done {
			ppDone, _ := m.handlePostProcess(t)
			done = done && ppDone
		}
		if done {
			// 连同「已确认不是目标场景」（single file 模式等）一并标 done，不再重试
			m.done[t.Hash] = true
		}
	}
	m.flushPostState()
	if m.cleanAbandoned > 0 {
		n := m.cleanAbandoned
		m.cleanAbandoned = 0
		logger.Info.Printf("文件清理：本轮 %d 个任务的文件长期在磁盘上找不到，已按存储脱钩跳过", n)
		m.notifyEvent("QBHive 文件清理放弃",
			fmt.Sprintf("本轮有 %d 个任务的文件连续 %d 轮在磁盘上找不到实体，已按存储脱钩跳过。\n常见原因：种子未下完、文件被外部移动或删除、qB 内部路径与磁盘不一致。",
				n, cleanUnresolvedMaxWaits))
	}
}

// handleCompleted 检查并扁平化「savePath 子目录下只含单个普通文件」的 torrent，
// 把该文件沿 qB 内部路径**只上移一层**（落点为其归档目录的父目录，可能是
// savePath 根，也可能是 savePath 下的分类目录），并清理残留空目录。
// 例：/下载/日本/ABC/x.mp4 → /下载/日本/x.mp4，到此为止；散在分类根
// （content_path == save_path，rel == "."）的文件绝不再上移、绝不删除。
//
// 安全铁律：只有当内容目录是 savePath 的**严格子目录**时才可能触发归档与删除。
// content_path == save_path（多文件种子无根目录、文件直接散在共享保存根）时
// rel == "."，此路径下既无可上移的子目录，也绝不可 os.RemoveAll —— 否则会删掉
// 整个保存根（含其它任务的文件），造成不可恢复的数据丢失。
//
// 返回值:
//
//	already=true  — 确认不是目标场景（single file torrent、内容即保存根、文件数不符、
//	                  清理多次未果而放弃），调用方标记 done 不再重试
//	didWork=true  — 本轮工作已完成（残留空目录已清理），调用方标记 done
//	两者都 false  — 临时错误，或 qB 移动已提交但待下轮清理，下次 scan 重试
func (m *Manager) handleCompleted(t models.QBTorrent) (already bool, didWork bool) {
	// 步骤 1：上一轮已提交 qB 异步移动，优先收尾清理原目录。
	// 必须排在 content_path 判定之前——移动落地后 qB 会把 content_path 改成
	// 新文件路径，届时再也无法从任务当前路径反推出需要清理的原目录。
	if dir := m.getFlattenDir(t.Hash); dir != "" {
		return m.finishFlatten(t, dir)
	}

	// 清洗 pass 若仍有已提交未落地的重命名，推迟归档：清洗可能改动目录/文件名，
	// 过早归档会与之争用同一源路径，或使随后记录的原目录因被改名而失效。
	if m.cfg.Get().FileManager.CleanEnabled {
		m.mu.Lock()
		rec := m.cleanState[t.Hash]
		cleanPending := rec != nil && len(rec.Pending) > 0
		m.mu.Unlock()
		if cleanPending {
			return false, false
		}
	}

	// 步骤 2：定位归档目录。5.x 的 content_path 最准确（单文件种子=文件完整路径，
	// 多文件种子=内容根目录），为空时回退 save_path/name。
	torrentDir := t.ContentPath
	if torrentDir == "" {
		torrentDir = filepath.Join(t.SavePath, t.Name)
	}
	info, err := os.Stat(torrentDir)
	if err != nil {
		logger.Debug.Printf("文件管理 [%s] 跳过：目录 %s 不存在（可能是 single file torrent）：%v", t.Name, torrentDir, err)
		return true, false
	}
	if !info.IsDir() {
		logger.Debug.Printf("文件管理 [%s] 跳过：%s 是文件不是目录（single file torrent）", t.Name, torrentDir)
		return true, false
	}

	// 安全闸：torrentDir 必须是 savePath 的严格子目录。rel == "."（内容即保存根，
	// 文件散在共享根）或 ".."（在保存根之外）或出错，一律跳过——既无可归档的子目录，
	// 也绝不允许对该路径做 RemoveAll。这条闸是 DSOD 删库事故的根治点。
	rel, relErr := filepath.Rel(t.SavePath, torrentDir)
	if relErr != nil || rel == "." || strings.HasPrefix(rel, "..") {
		logger.Debug.Printf("文件管理 [%s] 跳过：内容路径 %s 不是保存路径 %s 的严格子目录（rel=%q），无需归档且严禁删除",
			t.Name, torrentDir, t.SavePath, rel)
		return true, false
	}

	entries, err := os.ReadDir(torrentDir)
	if err != nil {
		logger.Warn.Printf("文件管理 [%s] 读取目录 %s 失败：%v，下次重试", t.Name, torrentDir, err)
		return false, false
	}
	var files []os.DirEntry
	for _, e := range entries {
		if e.Name()[0] == '.' {
			continue
		}
		files = append(files, e)
	}
	if len(files) == 0 {
		// 目录已无可见文件：残留空壳（已确认是 savePath 严格子目录），整体删除不会丢数据
		if err := os.RemoveAll(torrentDir); err != nil {
			logger.Warn.Printf("文件管理 [%s] 清理空目录 %s 失败：%v，下次重试", t.Name, torrentDir, err)
			return false, false
		}
		logger.Info.Printf("文件管理 [%s] 已清理空目录 %s", t.Name, torrentDir)
		return false, true
	}
	if len(files) != 1 {
		logger.Debug.Printf("文件管理 [%s] 跳过：目录 %s 有 %d 个可见文件（非单文件场景）", t.Name, torrentDir, len(files))
		return true, false
	}
	if !files[0].Type().IsRegular() {
		logger.Debug.Printf("文件管理 [%s] 跳过：%s 不是普通文件", t.Name, files[0].Name())
		return true, false
	}

	fileName := files[0].Name()
	// 归档目录的父目录就是本任务允许的落点：move 只上升一层，绝不跨过它
	// （rel 可能是 "Cat/ABC" 多层，落点 = savePath/Cat 而非 savePath 根）。
	dstDir := filepath.Dir(torrentDir)

	// renameFile 的路径必须用 qB 自己的内部文件路径（/torrents/files 返回的 name，
	// 相对 save_path 且带根目录段）——按任务名猜测会 409，并触发危险的本地回退。
	qbOld := ""
	fetched := true
	if qf, e := m.client.GetTorrentFiles(t.Hash); e == nil {
		diskSize := int64(-1)
		if fi, e2 := files[0].Info(); e2 == nil {
			diskSize = fi.Size()
		}
		for _, f := range qf {
			if path.Base(f.Name) != fileName {
				continue
			}
			if qbOld == "" {
				qbOld = f.Name // 同名多条目时的兜底；大小一致者优先（磁盘唯一对应）
			}
			if f.Size == diskSize {
				qbOld = f.Name
				break
			}
		}
	} else {
		fetched = false
		logger.Warn.Printf("文件管理 [%s] 获取 qB 文件列表失败：%v，回退按相对路径推导", t.Name, e)
	}
	if qbOld == "" {
		if fetched {
			// 列表里没有磁盘上这个文件名（可能是残留 tmp 之类）：不猜路径，本轮跳过
			logger.Debug.Printf("文件管理 [%s] 未在 qB 文件列表中找到 %s，跳过归档", t.Name, fileName)
			return true, false
		}
		// 列表取不到：用已核验的 rel 推导（torrentDir 确为 savePath 严格子目录）
		qbOld = filepath.ToSlash(filepath.Join(rel, fileName))
	}
	qbNew := upOneLevel(qbOld)
	if qbNew == "" || qbNew == qbOld {
		// 文件已在种子内部路径顶端，再上移就会离开分类目录：止步，不动
		logger.Debug.Printf("文件管理 [%s] %s 无法只上移一层（已到分类目录），跳过归档", t.Name, qbOld)
		return true, false
	}
	// 目标冲突预检：落点目录下已存在同名文件时加 _hash8 后缀，绝不覆盖
	newBase := path.Base(qbNew)
	if _, e := os.Lstat(filepath.Join(dstDir, newBase)); e == nil {
		qbNew = path.Join(path.Dir(qbNew), conflictFreeName(newBase, t.Hash))
	}

	// 1) 先通过 qB API 停止 torrent。
	//    记下操作前的暂停态：stoppedUP 通常来自用户「下载完成即暂停」的设置，
	//    归档结束后必须还原成暂停，把任务重新拉起做种属于覆盖用户意图。
	wasPaused := isPausedState(t.State)
	if err := m.client.StopTorrents(t.Hash); err != nil {
		logger.Warn.Printf("文件管理 [%s] 停止失败：%v", t.Name, err)
	}

	// 2) 走 qB renameFile API，失败时回退本地 os.Rename。
	//    本地回退的目标固定在 dstDir（归档目录的父目录）内，任何情况下都
	//    不会逃出分类目录，src==dst 时直接放弃。
	viaQB := true
	if err := m.client.RenameFile(t.Hash, qbOld, qbNew); err != nil {
		logger.Warn.Printf("文件管理 [%s] qB renameFile 失败（%s → %s）：%v，回退本地重命名",
			t.Name, qbOld, qbNew, err)
		viaQB = false
		src := filepath.Join(torrentDir, fileName)
		dst := filepath.Join(dstDir, filepath.FromSlash(path.Base(qbNew)))
		if src == dst {
			// 防御性兜底（理论不可达）：同路径无需移动，更不可删目录
			logger.Warn.Printf("文件管理 [%s] 本地重命名源与目标相同（%s），跳过并保留目录", t.Name, src)
			if !wasPaused {
				_ = m.client.StartTorrents(t.Hash)
			}
			return true, false
		}
		if err := moveFileSafe(src, dst); err != nil {
			logger.Warn.Printf("文件管理 [%s] 本地重命名 %s → %s 失败：%v，恢复 torrent 状态后下次重试",
				t.Name, src, dst, err)
			if !wasPaused {
				_ = m.client.StartTorrents(t.Hash)
			}
			return false, false
		}
		// 磁盘已经动了，但 qB 的内部路径还指着旧位置 —— 这会让任务立刻变成
		// missingFiles（qB 找不到文件），而清理逻辑又因该状态拒绝处理，形成死结。
		// 所以本地移动之后必须把 qB 元数据同步到新路径；同步失败就把文件移回去，
		// 宁可整轮重试，也绝不留下「磁盘已移动、qB 未同步」的失联状态。
		if syncErr := m.client.RenameFile(t.Hash, qbOld, qbNew); syncErr != nil {
			logger.Error.Printf("文件管理 [%s] 本地已移动但 qB 路径同步失败（%s → %s）：%v，回滚本地移动",
				t.Name, qbOld, qbNew, syncErr)
			if rbErr := moveFileSafe(dst, src); rbErr != nil {
				// 最坏情况：两边对不上，只能如实告知用户，并停止对该任务的自动处理
				logger.Error.Printf("文件管理 [%s] 危险：回滚 %s → %s 也失败：%v；文件已在新位置 %s，任务将显示缺少文件",
					t.Name, dst, src, rbErr, dst)
				m.audit.Append(auditEntry{
					Hash: t.Hash, Torrent: t.Name, Old: qbOld, New: qbNew,
					Via: auditViaLocalMove, Status: AuditFailed,
				})
				m.notifyEvent("QBHive 归档异常",
					fmt.Sprintf("任务「%s」的文件已移动到 %s，但 qBittorrent 记录未能同步，任务会显示缺文件。请手动校验或重新添加。",
						t.Name, dst))
				if !wasPaused {
					_ = m.client.StartTorrents(t.Hash)
				}
				return true, true // 不再重试：反复搬动只会让状态更乱
			}
			logger.Info.Printf("文件管理 [%s] 已回滚本地移动，文件回到 %s，下轮重试", t.Name, src)
			if !wasPaused {
				_ = m.client.StartTorrents(t.Hash)
			}
			return false, false
		}
		logger.Info.Printf("文件管理 [%s] 本地移动已同步到 qB：%s → %s", t.Name, qbOld, qbNew)
	}

	// 3) 恢复 torrent：只在操作前处于运行态时才启动。
	//    用户主动暂停的任务（stoppedUP）保持暂停，绝不擅自拉起。
	if !wasPaused {
		if err := m.client.StartTorrents(t.Hash); err != nil {
			logger.Warn.Printf("文件管理 [%s] 启动失败：%v", t.Name, err)
		}
	}

	if viaQB {
		logger.Info.Printf("文件管理 [%s] 已提交移动 %s → %s（qB API），等待异步落地后清理原目录",
			t.Name, qbOld, qbNew)
		// qB renameFile 的磁盘移动是异步的（libtorrent disk thread），API 返回 200 时
		// 文件可能还在原地，立即 RemoveAll 会把尚未移走的文件一起删掉造成数据丢失。
		// 记录原目录绝对路径，后续轮询确认它变空后再清理。
		m.setFlattenDir(t.Hash, torrentDir)
		return false, false
	}

	logger.Info.Printf("文件管理 [%s] 已本地移动 %s → %s", t.Name, qbOld, qbNew)
	// 写审计：本地回退的移动同样是一次改名，qB 元数据已同步，因此可以正常回退。
	// 不写审计的话这类改动在 UI 上完全隐身，用户既看不到也回不去。
	m.audit.Append(auditEntry{
		Hash: t.Hash, Torrent: t.Name, Old: qbOld, New: qbNew,
		Via: auditViaLocalMove, Status: AuditConfirmed,
	})
	// 本地回退说明 qB 的 renameFile 有问题（路径不一致/权限/瞬时故障），
	// 这类异常需要用户知道，否则以后所有归档都会静默走同一条路径。
	m.notifyEvent("QBHive 归档走了本地回退",
		fmt.Sprintf("任务「%s」的 qBittorrent 重命名失败，已改用本地移动并同步 qB 记录：\n%s → %s",
			t.Name, qbOld, qbNew))
	// 本地 os.Rename 是同步的，移动已落地，原目录（savePath 严格子目录）可直接清理
	if err := os.RemoveAll(torrentDir); err != nil {
		logger.Warn.Printf("文件管理 [%s] 清理目录 %s 失败：%v，下次重试", t.Name, torrentDir, err)
		return false, false
	}
	return false, true
}

// finishFlatten 收尾一次已提交 qB 异步移动的归档：轮询原目录，待其变空后删除。
// dir 是提交时记录的绝对路径，删除前再次校验它仍是 savePath 的严格子目录，
// 任何异常（保存根变更、目录仍有文件、读取出错）都只放弃清理，绝不删非空目录。
func (m *Manager) finishFlatten(t models.QBTorrent, dir string) (already bool, didWork bool) {
	if !isStrictSubdir(t.SavePath, dir) {
		logger.Warn.Printf("文件管理 [%s] 待清理目录 %s 不再是保存路径 %s 的严格子目录，放弃清理（绝不删除）",
			t.Name, dir, t.SavePath)
		m.clearFlattenDir(t.Hash)
		return true, false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			// 目录已不存在：qB 自行清理或上一轮已删，视为完成
			m.clearFlattenDir(t.Hash)
			return false, true
		}
		if m.bumpFlattenAttempts(t.Hash) {
			logger.Warn.Printf("文件管理 [%s] 读取待清理目录 %s 多次失败，放弃清理：%v", t.Name, dir, err)
			m.clearFlattenDir(t.Hash)
			return true, false
		}
		return false, false
	}
	if countVisible(entries) > 0 {
		// qB 异步移动尚未落地（文件仍在原目录），等待下一轮；多次不落地则放弃但不删
		if m.bumpFlattenAttempts(t.Hash) {
			logger.Warn.Printf("文件管理 [%s] 待清理目录 %s 多轮后仍有文件，放弃清理（绝不删非空目录）", t.Name, dir)
			m.clearFlattenDir(t.Hash)
			return true, false
		}
		return false, false
	}
	// 目录已空（仅剩隐藏残留也算空）→ 安全整体删除
	if err := os.RemoveAll(dir); err != nil {
		if m.bumpFlattenAttempts(t.Hash) {
			logger.Warn.Printf("文件管理 [%s] 清理空目录 %s 多次失败，放弃：%v", t.Name, dir, err)
			m.clearFlattenDir(t.Hash)
			return true, false
		}
		return false, false
	}
	logger.Info.Printf("文件管理 [%s] 已清理归档残留空目录 %s", t.Name, dir)
	m.clearFlattenDir(t.Hash)
	return false, true
}

// ---- 归档 pending 状态（持久化于 cleanState）与工具函数 ----

// getFlattenDir 读取某任务待清理的归档原目录（绝对路径），无则返回空串
func (m *Manager) getFlattenDir(hash string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rec := m.cleanState[hash]; rec != nil {
		return rec.FlattenDir
	}
	return ""
}

// setFlattenDir 记录待清理的归档原目录并落盘（qB 移动提交成功后调用）
func (m *Manager) setFlattenDir(hash, dir string) {
	m.mu.Lock()
	rec := m.cleanState[hash]
	if rec == nil {
		rec = &cleanRecord{}
		m.cleanState[hash] = rec
	}
	rec.FlattenDir = dir
	rec.FlattenAttempts = 0
	m.mu.Unlock()
	m.saveCleanState()
}

// clearFlattenDir 清除归档 pending 记录并落盘
func (m *Manager) clearFlattenDir(hash string) {
	m.mu.Lock()
	if rec := m.cleanState[hash]; rec != nil {
		rec.FlattenDir = ""
		rec.FlattenAttempts = 0
	}
	m.mu.Unlock()
	m.saveCleanState()
}

// bumpFlattenAttempts 自增等待轮数，返回是否已达上限（仅内存计数，不逐轮落盘）
func (m *Manager) bumpFlattenAttempts(hash string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec := m.cleanState[hash]
	if rec == nil {
		return true
	}
	rec.FlattenAttempts++
	return rec.FlattenAttempts >= flattenMaxAttempts
}

// isStrictSubdir 判断 child 是否为 parent 的严格子目录（不相等、不在其外）。
// 用作 RemoveAll 前的护栏，杜绝误删保存根或越界路径。
func isStrictSubdir(parent, child string) bool {
	if parent == "" || child == "" {
		return false
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != "." && !strings.HasPrefix(rel, "..")
}

// countVisible 统计目录条目中非隐藏（不以 . 开头）的数量
func countVisible(entries []os.DirEntry) int {
	n := 0
	for _, e := range entries {
		if e.Name()[0] == '.' {
			continue
		}
		n++
	}
	return n
}

// conflictFreeName 生成 base_hash8.ext 形式的兜底名（hash 不足 8 位则用全量）
func conflictFreeName(fileName, hash string) string {
	ext := filepath.Ext(fileName)
	base := fileName[:len(fileName)-len(ext)]
	suffix := hash
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	return base + "_" + suffix + ext
}

// upOneLevel 把相对路径去掉恰好一层目录（"Cat/ABC/x.mkv" → "Cat/x.mkv"，
// "ABC/x.mkv" → "x.mkv"）。路径已在顶端（"x.mkv"）时返回原名，调用方据此
// 判断「再上移就离开分类目录」；无意义路径（空/"."/"绝对路径"）返回空串。
func upOneLevel(p string) string {
	if p == "" || p == "." || p == "/" || strings.HasPrefix(p, "/") {
		return ""
	}
	dir := path.Dir(p)
	if dir == "." {
		return path.Base(p) // 已在顶端，原样返回供调用方比较
	}
	if dir == "/" {
		return ""
	}
	return path.Join(path.Dir(dir), path.Base(p))
}

// Reset 重置完成状态（服务重启时调用）
func (m *Manager) Reset() {
	m.done = make(map[string]bool)
}

// ---- 文件名自动清理（clean pass）----

// ensureRules 规则指纹变化时重新编译自定义规则（仅 scan goroutine 调用，无并发）
func (m *Manager) ensureRules() {
	rules := m.cfg.Get().FileManager.CleanRules
	key := strings.Join(rules, "\x00")
	if key == m.rulesKey {
		return
	}
	m.rulesKey = key
	m.rulesCache = compileCleanRules(rules)
}

// handleClean 处理单个任务的文件名清理：核销 pending → AI（可选）→ 构建计划 →
// Stop → 批量 renameFile → Start。全程不覆盖任何已存在的文件。
func (m *Manager) handleClean(t models.QBTorrent) {
	cfg := m.cfg.Get().FileManager
	sig := cleanRulesSignature(cfg)
	changed := false

	m.mu.Lock()
	rec := m.cleanState[t.Hash]
	if rec == nil {
		rec = &cleanRecord{}
		m.cleanState[t.Hash] = rec
	}
	// 清理规则变了（新增自定义正则、启用/关闭 AI、内置规则升级）→ 解除此前判 Done 的任务。
	// rec.Done 一旦置位本进程就不再回头看，不重置的话用户新加的正则对历史任务永远不生效。
	if rec.Done && rec.CleanSig != "" && rec.CleanSig != sig {
		logger.Info.Printf("文件清理 [%s] 清理规则已变更，重新评估该任务", t.Name)
		rec.Done = false
		// 顺带解除 AI 熔断：规则都改了，AI 通道可能也是刚配好的
		rec.AISettled = false
		rec.AISettledAt = 0
		rec.AiAttempts = 0
		changed = true
	}
	rec.CleanSig = sig
	if rec.Done {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()

	// 存储已脱钩的任务（qB 找不到实体文件）绝不清洗：renameFile 只会改内部元数据，
	// 磁盘上的脏文件名永远洗不掉，还会让日后「找回文件/重新校验」对不上路径。
	switch t.State {
	case "missingFiles", "error":
		return
	}

	files, err := m.client.GetTorrentFiles(t.Hash)
	if err != nil {
		logger.Warn.Printf("文件清理 [%s] 获取文件列表失败：%v，下轮重试", t.Name, err)
		return
	}

	// 1) 核销 pending：qB renameFile 异步，按「旧完整路径从文件列表消失」判定落地。
	//    重命名以文件为粒度（newPath 可改目录段），仅比 base 名会把换目录误判为未落地。
	m.mu.Lock()
	if len(rec.Pending) > 0 {
		pathSet := make(map[string]bool, len(files))
		for _, f := range files {
			pathSet[f.Name] = true
		}
		var remaining []cleanRename
		for _, p := range rec.Pending {
			if !pathSet[p.Old] {
				logger.Info.Printf("文件清理 [%s] 已确认重命名落地 %s → %s", t.Name, p.Old, p.New)
				if p.AuditID != "" {
					m.audit.UpdateStatus(p.AuditID, AuditConfirmed)
				}
				changed = true
				continue
			}
			p.Attempts++
			if p.Attempts >= 3 {
				logger.Warn.Printf("文件清理 [%s] 放弃重命名 %s（连续 %d 轮未确认落地）", t.Name, p.Old, p.Attempts)
				if p.AuditID != "" {
					m.audit.UpdateStatus(p.AuditID, AuditFailed)
				}
				changed = true
				continue
			}
			remaining = append(remaining, p)
		}
		rec.Pending = remaining
	}
	m.mu.Unlock()

	// 2) 规则编译缓存
	m.ensureRules()

	// 3) AI 格式化（每任务一次，失败重试至多 3 轮后降级为仅正则清洗）
	var aiNames map[string]string
	aiPending := false
	if cfg.AIEnabled {
		// 熔断冷却到点 → 重武装。此前一旦降级就永久钉死在「仅正则」，
		// 用户事后修好网络或换通道也回不来。
		m.mu.Lock()
		if rec.AISettled && rec.AISettledAt > 0 &&
			time.Since(time.Unix(rec.AISettledAt, 0)) >= aiRearmAfter {
			logger.Info.Printf("文件清理 [%s] AI 熔断冷却结束，重新尝试 AI 格式化", t.Name)
			rec.AISettled = false
			rec.AISettledAt = 0
			rec.AiAttempts = 0
		}
		settled := rec.AISettled
		m.mu.Unlock()
		if !settled {
			ch := activeAIChannel(cfg)
			if ch == nil {
				// 没有可用通道：既不消耗重试次数，也不置位熔断。
				// 用户随时可能在设置里配好通道并保存，置位会让该任务永久失去 AI 美化。
				logger.Debug.Printf("文件清理 [%s] 暂无可用 AI 通道，本轮跳过（不熔断、不消耗重试次数）", t.Name)
			} else {
				names := collectAINames(files, m.rulesCache)
				aiNames = aiFormatNames(*ch, names, t.Name)
				m.mu.Lock()
				if aiNames != nil {
					// 成功是终态：AISettledAt 留 0，永不重武装
					rec.AISettled = true
					rec.AISettledAt = 0
				} else {
					rec.AiAttempts++
					if rec.AiAttempts >= aiMaxAttempts {
						logger.Warn.Printf("文件清理 [%s] AI 连续 %d 次失败，降级为仅正则清洗（%s 后自动重试）",
							t.Name, rec.AiAttempts, aiRearmAfter)
						// 失败降级：记下时刻，冷却结束后可重武装
						rec.AISettled = true
						rec.AISettledAt = time.Now().Unix()
					} else {
						aiPending = true
					}
				}
				m.mu.Unlock()
				changed = true
			}
		}
	}

	// 4) 构建清洗计划（bottom-up 排序 + 计划内去重 + 磁盘冲突兜底，绝不覆盖）
	plan, unresolved := buildCleanPlan(files, m.rulesCache, aiNames, t.SavePath, t.Hash)
	// 已在途的改名不重复提交：files 是本轮开头抓的快照，而 qB 的 renameFile 落地是异步的，
	// 上一轮提交的路径可能还留在这一轮的快照里 → 同一条 old→new 会被再算出来一次，
	// 既写重复审计，又让每个副本各自累加 Attempts（三条同名就能撞满「3 轮未落地即放弃」）。
	//
	// 同时冻结正在转码的路径：派发给 shell 的是路径快照，清理此时把源文件搬走，
	// 外部工具会按旧名写出产物，postPrune 的源片配对也随之中断（源片和产物都留在磁盘上）。
	// 只冻结在途的那几条路径，其余文件照常清洗，避免一个长转码冻住整个任务。
	frozenOut := 0
	m.mu.Lock()
	if len(plan) > 0 {
		var pendingOld map[string]bool
		if len(rec.Pending) > 0 {
			pendingOld = make(map[string]bool, len(rec.Pending))
			for _, p := range rec.Pending {
				pendingOld[p.Old] = true
			}
		}
		var inFlight map[string]bool
		if len(rec.PostDispatched) > 0 && !rec.PostDone {
			inFlight = make(map[string]bool, len(rec.PostDispatched))
			for _, p := range rec.PostDispatched {
				inFlight[p] = true
			}
		}
		if pendingOld != nil || inFlight != nil {
			kept := plan[:0]
			for _, r := range plan {
				if pendingOld[r.Old] {
					continue
				}
				if inFlight[filepath.Join(t.SavePath, filepath.FromSlash(r.Old))] ||
					inFlight[filepath.Join(t.SavePath, filepath.FromSlash(r.New))] {
					frozenOut++
					continue
				}
				kept = append(kept, r)
			}
			plan = kept
		}
	}
	m.mu.Unlock()
	if frozenOut > 0 {
		logger.Debug.Printf("文件清理 [%s] 让路 %d 条改名：目标文件正在转码", t.Name, frozenOut)
	}
	// 磁盘上找不到实体的文件不能判完成：它可能还在下载（qB 内部名不带 .!qB、磁盘带），
	// 也可能等转码产物落盘后才有。提前钉死 Done 就再也不复查——「升了版本还是洗不掉」正是这么来的。
	// 真脱钩的僵尸任务每轮都找不到，到上限才按脱钩放弃并告警一次。
	m.mu.Lock()
	if unresolved > 0 {
		rec.UnresolvedCleans++
	} else if rec.UnresolvedCleans > 0 {
		rec.UnresolvedCleans = 0
	}
	unresolvedWaits := rec.UnresolvedCleans
	m.mu.Unlock()
	allowDone := unresolvedWaits == 0 || unresolvedWaits >= cleanUnresolvedMaxWaits
	switch {
	case unresolved > 0 && unresolvedWaits < cleanUnresolvedMaxWaits:
		logger.Debug.Printf("文件清理 [%s] %d 个文件在磁盘上找不到实体，留待下轮（第 %d/%d 轮）",
			t.Name, unresolved, unresolvedWaits, cleanUnresolvedMaxWaits)
	case unresolved > 0 && unresolvedWaits == cleanUnresolvedMaxWaits:
		// 僵尸任务可能成百上千，逐条通知会把用户淹掉：这里只留可检索的日志，
		// 通知在扫描结束时按「本轮放弃数」汇成一条（见 scan）
		logger.Warn.Printf("文件清理 [%s] %d 个文件连续 %d 轮在磁盘上找不到，按存储脱钩放弃",
			t.Name, unresolved, unresolvedWaits)
		m.cleanAbandoned++
	}
	if len(plan) == 0 {
		m.mu.Lock()
		// frozenOut > 0 说明本轮有活没干（让给转码），allowDone=false 说明还有文件等落盘，
		// 两种情况都绝不能据此判定 Done
		if len(rec.Pending) == 0 && !aiPending && frozenOut == 0 && allowDone {
			rec.Done = true
			rec.PostCleanWaits = 0
			rec.PostCleanWarned = false
			rec.UnresolvedCleans = 0
			m.mu.Unlock()
			logger.Info.Printf("文件清理 [%s] 已完成（无待清理项）", t.Name)
			m.saveCleanState()
			return
		}
		m.mu.Unlock()
		if changed {
			m.saveCleanState()
		}
		return
	}

	// 5) 应用：一次 Stop → 批量 rename → 必然 Start
	committed := m.applyRenames(t.Hash, plan, isPausedState(t.State))
	if len(committed) > 0 {
		for i := range committed {
			e := m.audit.Append(auditEntry{
				Hash:    t.Hash,
				Torrent: t.Name,
				Old:     committed[i].Old,
				New:     committed[i].New,
				Via:     committed[i].Via,
				Status:  AuditCommitted,
			})
			committed[i].AuditID = e.ID
		}
		m.mu.Lock()
		rec.Pending = append(rec.Pending, committed...)
		m.mu.Unlock()
		m.saveCleanState()
		logger.Info.Printf("文件清理 [%s] 本轮提交 %d 条重命名", t.Name, len(committed))
	} else if changed {
		m.saveCleanState()
	}
}

// cleanRulesSignature 清理配置的指纹：内置规则版本 + 自定义正则 + 清理/AI 开关。
//
// 任一项变化都意味着「用户改了清理规则」。rec.Done 一旦置位本进程就不再复查该任务，
// 没有这个指纹，用户新加的自定义正则、刚启用的 AI 对已扫描过的任务永远不生效，
// 只能靠删状态文件或重启解决。
//
// 与后处理的 postFilterSignature 同一套思路：把原文（而非摘要）存进状态文件，
// 便于人回看排查；空改动（纯空格）已归一化，不会白白重开一批任务。
func cleanRulesSignature(fm models.FileManagerConfig) string {
	rules := make([]string, 0, len(fm.CleanRules))
	for _, r := range fm.CleanRules {
		if s := strings.TrimSpace(r); s != "" {
			rules = append(rules, s)
		}
	}
	return fmt.Sprintf("v%d|clean=%t|ai=%t|rules=%s",
		cleanBuiltinVersion, fm.CleanEnabled, fm.AIEnabled, strings.Join(rules, "\x1f"))
}

// isPausedState 判断任务是否处于「被主动暂停」的暂停态。
//
// 只有 stoppedUP / stoppedDL 是真正的暂停：前者常见于是用户开启了「下载完成即暂停」，
// 后者是用户手动停下的未完成下载。stalledUP / queuedUP 只是暂时没有速度，
// 任务仍在运行队列里，不属于暂停。
//
// 归档与重命名都要 Stop → 操作 → 恢复，恢复时必须据此判断：
// 把用户主动暂停的任务重新拉起做种，是在覆盖用户的显式意图。
func isPausedState(state string) bool {
	return state == "stoppedUP" || state == "stoppedDL"
}

// applyRenames 一次 Stop → 逐条 RenameFile → 按操作前的状态决定是否恢复。
// 返回提交成功的条目（失败的条目下轮重试）；任何情况下不覆盖已有文件
// （冲突已在 buildCleanPlan 预检兜底）。
//
// wasPaused 为 true 表示调用前任务就是暂停态，此时保持暂停，不调用 StartTorrents。
func (m *Manager) applyRenames(hash string, plan []cleanRename, wasPaused bool) []cleanRename {
	if err := m.client.StopTorrents(hash); err != nil {
		logger.Warn.Printf("文件清理：停止任务 %s 失败：%v", hash, err)
	}
	var committed []cleanRename
	for _, r := range plan {
		if err := m.client.RenameFile(hash, r.Old, r.New); err != nil {
			logger.Warn.Printf("文件清理：renameFile 失败（%s → %s）：%v，下轮重试", r.Old, r.New, err)
			continue
		}
		logger.Info.Printf("文件清理：提交重命名 %s → %s（%s）", r.Old, r.New, r.Via)
		committed = append(committed, r)
	}
	// 只恢复原本在跑的任务：被用户暂停的（stoppedUP / stoppedDL）保持暂停
	if !wasPaused {
		if err := m.client.StartTorrents(hash); err != nil {
			logger.Warn.Printf("文件清理：恢复任务 %s 失败：%v", hash, err)
		}
	}
	return committed
}

// RollbackAudit 回退一条审计记录（new → old）。绝不覆盖：目标旧名在任务文件
// 列表或磁盘上已存在时拒绝回退。回退后该任务标记 Done，避免 clean pass
// 下一轮立刻把带水印的旧名再改回去。
func (m *Manager) RollbackAudit(id string) error {
	e, ok := m.audit.Get(id)
	if !ok {
		return fmt.Errorf("审计记录不存在")
	}
	if e.Status == AuditRolledback {
		return fmt.Errorf("该记录已回退过")
	}
	if e.Status != AuditCommitted && e.Status != AuditConfirmed {
		return fmt.Errorf("状态为 %s 的记录不可回退", e.Status)
	}
	list, err := m.client.GetTorrents("all", "", "")
	if err != nil {
		return fmt.Errorf("获取任务列表失败：%w", err)
	}
	var tor *models.QBTorrent
	for i := range list {
		if list[i].Hash == e.Hash {
			tor = &list[i]
			break
		}
	}
	if tor == nil {
		return fmt.Errorf("任务已从 qBittorrent 删除，无法回退")
	}
	files, err := m.client.GetTorrentFiles(e.Hash)
	if err != nil {
		return fmt.Errorf("获取文件列表失败：%w", err)
	}
	// 按完整相对路径比对，不能只比 base 名：归档类记录只改目录段、文件名不变
	// （ABC/x.mp4 → x.mp4），按 base 比对会把「新名就是旧名」判定成冲突，导致这类
	// 记录永远回退不了。
	foundNew, foundOld := false, false
	for _, f := range files {
		if f.Name == e.New {
			foundNew = true
		}
		if f.Name == e.Old {
			foundOld = true
		}
	}
	if !foundNew {
		return fmt.Errorf("当前文件列表中找不到 %s，可能已被其他操作修改", e.New)
	}
	if foundOld {
		return fmt.Errorf("旧路径 %s 已存在于任务中，拒绝回退（绝不覆盖）", e.Old)
	}
	dst := filepath.Join(tor.SavePath, filepath.FromSlash(e.Old))
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("磁盘上已存在 %s，拒绝回退（绝不覆盖）", e.Old)
	}
	committed := m.applyRenames(e.Hash, []cleanRename{{Old: e.New, New: e.Old, Via: e.Via}}, isPausedState(tor.State))
	if len(committed) == 0 {
		return fmt.Errorf("renameFile 提交失败，详见服务日志")
	}
	m.audit.UpdateStatus(id, AuditRolledback)
	m.mu.Lock()
	if rec := m.cleanState[e.Hash]; rec != nil {
		rec.Pending = nil
		rec.Done = true
	}
	m.mu.Unlock()
	m.saveCleanState()
	logger.Info.Printf("审计回退 [%s] 已提交 %s → %s", e.Torrent, e.New, e.Old)
	m.notifyEvent("QBHive 已回退改名",
		fmt.Sprintf("任务「%s」的文件已从 %s 回退为 %s", e.Torrent, e.New, e.Old))
	return nil
}

// AuditList 供 web 层分页查询审计记录（倒序）
func (m *Manager) AuditList(offset, limit int) ([]auditEntry, int) {
	return m.audit.List(offset, limit)
}

// loadCleanState 从 data/filemgr_clean.json 加载清理进度
func (m *Manager) loadCleanState() {
	data, err := os.ReadFile(m.statePath)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Warn.Printf("文件管理读取清理状态失败：%v", err)
		}
		return
	}
	if err := json.Unmarshal(data, &m.cleanState); err != nil {
		logger.Warn.Printf("文件管理解析清理状态失败：%v", err)
		return
	}
	logger.Info.Printf("文件管理已加载 %d 条清理状态", len(m.cleanState))
}

// saveCleanState 原子写清理进度（tmp + rename）
func (m *Manager) saveCleanState() {
	m.mu.Lock()
	data, err := json.Marshal(m.cleanState)
	m.mu.Unlock()
	if err != nil {
		logger.Warn.Printf("文件管理序列化清理状态失败：%v", err)
		return
	}
	dir := filepath.Dir(m.statePath)
	_ = os.MkdirAll(dir, 0o755)
	tmp := m.statePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		logger.Warn.Printf("文件管理写入清理状态失败：%v", err)
		return
	}
	_ = os.Rename(tmp, m.statePath)
}
