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
}

// cleanRecord 单个任务的清理进度
type cleanRecord struct {
	Pending    []cleanRename `json:"pending,omitempty"`  // 已提交待确认落地的重命名
	Done       bool          `json:"done,omitempty"`     // 已无待办且全部落地
	AiAttempts int           `json:"aiAttempts,omitempty"`
	AISettled  bool          `json:"aiSettled,omitempty"` // AI 已成功调用过 / 无可用通道 / 重试耗尽

	// FlattenDir 记录单文件归档已提交 qB 异步移动、等待清空后删除的原目录（绝对路径）。
	// 非空表示上一轮已把 savePath 子目录里唯一的文件移到保存根，qB 的磁盘移动是异步的，
	// 必须等原目录真正变空才能删，否则会连未移走的文件一起删掉。持久化以便重启后续清。
	FlattenDir string `json:"flattenDir,omitempty"`
	// FlattenAttempts 归档清理连续等待/失败的轮数，超过上限则放弃（绝不删非空目录）
	FlattenAttempts int `json:"flattenAttempts,omitempty"`
}

const cleanStateFile = "filemgr_clean.json"

// flattenMaxAttempts 等待 qB 异步移动落地、清理归档残留目录的最大轮数。
// 同一卷内移动是元数据操作（秒级），20 轮（默认间隔约 5 分钟）足够；
// 超过说明移动异常，放弃清理但绝不删除仍有文件的目录。
const flattenMaxAttempts = 20

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
	if !cfg.Enabled {
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

func (m *Manager) Start() {
	c := m.cfg.Get().FileManager
	if !c.Enabled {
		logger.Info.Println("文件管理已停用")
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
		// done 标记在 handleCompleted 确认后设置，避免早返回导致永久跳过
		already, didWork := m.handleCompleted(t)
		if didWork {
			m.done[t.Hash] = true
		} else if already {
			// torrent 已被确认不是目标场景（如 single file 模式），不再重试
			m.done[t.Hash] = true
		}
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

	// 1) 先通过 qB API 停止 torrent
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
			_ = m.client.StartTorrents(t.Hash)
			return true, false
		}
		if err := os.Rename(src, dst); err != nil {
			logger.Warn.Printf("文件管理 [%s] 本地重命名 %s → %s 失败：%v，启动 torrent 后下次重试",
				t.Name, src, dst, err)
			_ = m.client.StartTorrents(t.Hash)
			return false, false
		}
	}

	// 3) 启动 torrent
	if err := m.client.StartTorrents(t.Hash); err != nil {
		logger.Warn.Printf("文件管理 [%s] 启动失败：%v", t.Name, err)
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
	m.mu.Lock()
	rec := m.cleanState[t.Hash]
	if rec == nil {
		rec = &cleanRecord{}
		m.cleanState[t.Hash] = rec
	}
	if rec.Done {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()

	files, err := m.client.GetTorrentFiles(t.Hash)
	if err != nil {
		logger.Warn.Printf("文件清理 [%s] 获取文件列表失败：%v，下轮重试", t.Name, err)
		return
	}
	changed := false

	// 1) 核销 pending：qB renameFile 异步，按 base 名消失判定落地（防父目录改名误判）
	m.mu.Lock()
	if len(rec.Pending) > 0 {
		baseSet := make(map[string]bool, len(files))
		for _, f := range files {
			baseSet[path.Base(f.Name)] = true
		}
		var remaining []cleanRename
		for _, p := range rec.Pending {
			if !baseSet[path.Base(p.Old)] {
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
		m.mu.Lock()
		settled := rec.AISettled
		m.mu.Unlock()
		if !settled {
			ch := activeAIChannel(cfg)
			if ch == nil {
				m.mu.Lock()
				rec.AISettled = true
				m.mu.Unlock()
				changed = true
			} else {
				names := collectAINames(files, m.rulesCache)
				aiNames = aiFormatNames(*ch, names, t.Name)
				m.mu.Lock()
				if aiNames != nil {
					rec.AISettled = true
				} else {
					rec.AiAttempts++
					if rec.AiAttempts >= aiMaxAttempts {
						logger.Warn.Printf("文件清理 [%s] AI 连续 %d 次失败，降级为仅正则清洗", t.Name, rec.AiAttempts)
						rec.AISettled = true
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
	plan := buildCleanPlan(files, m.rulesCache, aiNames, t.SavePath, t.Hash)
	if len(plan) == 0 {
		m.mu.Lock()
		if len(rec.Pending) == 0 && !aiPending {
			rec.Done = true
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
	committed := m.applyRenames(t.Hash, plan)
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

// applyRenames 一次 Stop → 逐条 RenameFile → 必然 Start。
// 返回提交成功的条目（失败的条目下轮重试）；任何情况下不覆盖已有文件
// （冲突已在 buildCleanPlan 预检兜底）。
func (m *Manager) applyRenames(hash string, plan []cleanRename) []cleanRename {
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
	if err := m.client.StartTorrents(hash); err != nil {
		logger.Warn.Printf("文件清理：恢复任务 %s 失败：%v", hash, err)
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
	foundNew, foundOld := false, false
	for _, f := range files {
		b := path.Base(f.Name)
		if b == path.Base(e.New) {
			foundNew = true
		}
		if b == path.Base(e.Old) {
			foundOld = true
		}
	}
	if !foundNew {
		return fmt.Errorf("当前文件列表中找不到 %s，可能已被其他操作修改", path.Base(e.New))
	}
	if foundOld {
		return fmt.Errorf("旧名 %s 已存在于任务中，拒绝回退（绝不覆盖）", path.Base(e.Old))
	}
	dst := filepath.Join(tor.SavePath, filepath.FromSlash(e.Old))
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("磁盘上已存在 %s，拒绝回退（绝不覆盖）", e.Old)
	}
	committed := m.applyRenames(e.Hash, []cleanRename{{Old: e.New, New: e.Old, Via: e.Via}})
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
