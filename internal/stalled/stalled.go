package stalled

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Felix2yu/qbhive/internal/config"
	"github.com/Felix2yu/qbhive/internal/logger"
	"github.com/Felix2yu/qbhive/internal/models"
	qb "github.com/Felix2yu/qbhive/internal/qbittorrent"
)

const (
	stateFile  = "stalled_state.json"
	auditFile  = "stalled_audit.jsonl"
	auditCap   = 1000
	defInterv  = 60 // 默认扫描间隔（秒）
	defIdle    = 30 // 默认零速时长门槛（分钟）
	defSpeedKB = 5  // 默认速度阈值（KiB/s）
)

// stallStates 会被「速度长期为 0」判定覆盖的活动下载态。
//
// 刻意只包含三类真正占用下载名额、且有可能永久卡住的状态：
//   - stalledDL  下载中但无连接（死种子的典型形态）
//   - downloading 下载中（瞬时速度为 0 很常见，需靠时长门槛过滤）
//   - metaDL     磁链取元数据（卡住时名字还是 40 位 hash，永不推进）
//
// 明确排除：
//   - forcedDL / forcedMetaDL —— 用户强制启动，属于明确意图，绝不自动暂停
//   - checkingDL / checkingResumeData —— 校验中的假性零速
//   - queuedDL —— 本就在排队，没占活动名额
//   - 所有上传/做种态 —— 本功能只回收下载名额
var stallStates = map[string]bool{
	"stalledDL": true,
	"downloading": true,
	"metaDL":     true,
}

// Manager 长期零速任务回收引擎。自带 ticker，不挂在别的扫描循环里。
//
// 判定同时看两个独立证据，取较大者：
//   1) qB 的 last_activity 距今（好处：进程重启后仍有效）；
//   2) 我们自己的连续零速观测首次时间戳（好处：不受 tracker 心跳刷新
//      last_activity 的干扰——这正是 qB 内置「慢速种子」阈值失效的原因）。
//
// 两者都要再过 capByObservation 封顶：零速时长不能超过我们实际观测到它
// 连续占用活动名额的跨度，否则刚被 start 的老任务会凭旧 last_activity 立即定罪。
type Manager struct {
	cfg    *config.Manager
	client *qb.Client

	torrentCache *qb.TorrentCache

	stop       chan struct{}
	tickerStop chan struct{}

	// lifeMu 串行化 Start/Reload/Stop 对 tickerStop 的读-close-重建。
	// 两次并发的配置保存会同时走到 Reload，不加锁就是 close of closed channel。
	lifeMu sync.Mutex

	// saveMu 串行化落盘：ticker 的 ScanNow、API 触发的 ScanNow、Stop 都会调 save，
	// 而它们共用同一个 .tmp 路径。mu 只护内存快照，放开后两个写者会互相截断临时文件，
	// rename 出去的就是半截 JSON，重启时整份观测时钟与审计被丢弃。
	saveMu sync.Mutex

	mu       sync.Mutex
	notify   func(title, body string)
	statePath string
	auditPath string
	// firstZero: hash → 首次观测到「状态+速度都符合条件」的时刻（Unix 秒）
	firstZero map[string]int64
	// activeSince: hash → 首次观测到它连续处于活动下载态的时刻（Unix 秒）。
	// 与 firstZero 的区别：有速度时 firstZero 会清零，但名额仍被占用，此时钟不清。
	// 用来给零速时长封顶，见 capByObservation。
	activeSince map[string]int64
	// lastNotify: hash → 上次告警时刻（Unix 秒），notify 档位去重
	lastNotify map[string]int64
	// lastBurst: 上次「本轮超限」告警时刻，避免每轮刷屏
	lastBurst int64
	audits      []auditEntry
}

type auditEntry struct {
	TS      int64  `json:"ts"`
	Hash    string `json:"hash"`
	Name    string `json:"name"`
	State   string `json:"state"`
	IdleMin int    `json:"idleMin"` // 判定时已零速的分钟数
	DL      int64  `json:"dl"`
	UL      int64  `json:"ul"`
	Action  string `json:"action"` // notify / paused
	OK      bool   `json:"ok"`
	Detail  string `json:"detail,omitempty"`
}

// SetTorrentCache 注入共享任务快照（装配层在 Start 之前调用一次）
func (m *Manager) SetTorrentCache(c *qb.TorrentCache) { m.torrentCache = c }

// SetNotifier 注入通知通道，nil 时静默跳过
func (m *Manager) SetNotifier(fn func(title, body string)) {
	m.mu.Lock()
	m.notify = fn
	m.mu.Unlock()
}

func defaultPaths() (state, audit string) {
	if cfg := os.Getenv("QBHIVE_CONFIG"); cfg != "" {
		dir := filepath.Dir(cfg)
		return filepath.Join(dir, stateFile), filepath.Join(dir, auditFile)
	}
	return filepath.Join("data", stateFile), filepath.Join("data", auditFile)
}

func New(cfg *config.Manager, client *qb.Client) *Manager {
	sp, ap := defaultPaths()
	m := &Manager{
		cfg:        cfg,
		client:     client,
		stop:       make(chan struct{}),
		statePath:  sp,
		auditPath:  ap,
		firstZero:  make(map[string]int64),
		activeSince: make(map[string]int64),
		lastNotify: make(map[string]int64),
	}
	m.load()
	return m
}

func (m *Manager) interval() time.Duration {
	sec := m.cfg.Get().Stalled.Interval
	if sec <= 0 {
		sec = defInterv
	}
	return time.Duration(sec) * time.Second
}

// Start 按配置启动 ticker（off 或未 Enabled 时不启动）
func (m *Manager) Start() {
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	c := m.cfg.Get().Stalled
	if !c.Enabled || c.Mode == models.StalledModeOff {
		logger.Info.Println("停滞回收已停用")
		return
	}
	logger.Info.Printf("停滞回收启动，间隔=%s 档位=%s 零速门槛=%d分钟", m.interval(), c.Mode, m.idleMinutes(c))
	m.runTicker(m.interval())
}

// Reload 按最新配置重启（或停止）轮询
func (m *Manager) Reload(client *qb.Client) {
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	if client != nil {
		m.client = client
	}
	if m.tickerStop != nil {
		close(m.tickerStop)
		m.tickerStop = nil
	}
	c := m.cfg.Get().Stalled
	if !c.Enabled || c.Mode == models.StalledModeOff {
		logger.Info.Println("停滞回收已停用（重载）")
		return
	}
	logger.Info.Printf("停滞回收重载，间隔=%s 档位=%s", m.interval(), c.Mode)
	m.runTicker(m.interval())
}

// runTicker 调用方必须持有 lifeMu
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
				m.ScanNow()
			}
		}
	}()
}

func (m *Manager) Stop() {
	m.lifeMu.Lock()
	close(m.stop)
	if m.tickerStop != nil {
		close(m.tickerStop)
		m.tickerStop = nil
	}
	m.lifeMu.Unlock()
	m.save()
}

func (m *Manager) idleMinutes(c models.StalledConfig) int {
	if c.IdleMinutes > 0 {
		return c.IdleMinutes
	}
	return defIdle
}

// allTorrents 优先读共享快照，未就绪时回退直连
func (m *Manager) allTorrents() ([]models.QBTorrent, error) {
	if m.torrentCache != nil {
		if list, err := m.torrentCache.GetTorrents("all", "", ""); err == nil {
			return list, nil
		}
	}
	return m.client.GetTorrents("all", "", "")
}

// candidate 一条判定候选（内部 + 试算共用）
type candidate struct {
	t       models.QBTorrent
	idleSec int64 // 已零速持续的秒数（取两个证据的较大值）
	reason  string
}

// speedThreshold 把 KiB/s 的阈值换成 B/s；负数按默认处理
func speedThreshold(c models.StalledConfig) int64 {
	kb := c.SpeedKiB
	if kb < 0 {
		kb = defSpeedKB
	}
	return int64(kb) * 1024
}

// evalState 判断一个任务是否命中「活动下载态 + 速度足够低」。
// firstZero 从状态快照传入（0 表示首次观测）。返回是否处于零速形态、原因。
func evalState(t models.QBTorrent, thr int64, firstZero, now int64) (zero bool, idleSec int64, reason string) {
	if !stallStates[t.State] {
		return false, 0, "状态不属于可回收的活动下载态"
	}
	if t.DownloadSpeed > thr || t.UploadSpeed > thr {
		return false, 0, "当前仍有速度"
	}
	// 证据 1：qB 自己的最后活动时间
	var byQb int64
	if t.LastActivity > 0 && now > t.LastActivity {
		byQb = now - t.LastActivity
	}
	// 证据 2：我们自己的连续零速观测
	var byOwn int64
	if firstZero > 0 && now > firstZero {
		byOwn = now - firstZero
	}
	idleSec = byQb
	if byOwn > idleSec {
		idleSec = byOwn
	}
	return true, idleSec, ""
}

// capByObservation 把零速时长压进「我们实际观测到它连续占用活动名额」的跨度内。
//
// last_activity 记的是最后一次有效载荷传输，任务暂停期间它同样不前进。不封顶的话，
// 用户刚 start 一个停了很久的任务，首轮扫描就会凭旧 last_activity 判它「已零速数小时」
// 并立刻重新暂停——配置里的 30 分钟宽限完全没生效。activeSince 未知时按 0 处理，
// 宁可多等一轮，也不凭一个我们无法佐证的旧时间戳动手。
func capByObservation(idle, activeSince, now int64) int64 {
	if activeSince <= 0 {
		return 0
	}
	if span := now - activeSince; span < idle {
		return span
	}
	return idle
}

// protected 判断任务是否受保护：强制态、保护标签。
func protected(t models.QBTorrent, c models.StalledConfig) bool {
	if strings.HasPrefix(t.State, "forced") {
		return true // 强制启动是明确意图
	}
	if tag := strings.TrimSpace(c.ExcludeTag); tag != "" {
		for _, have := range strings.Split(t.Tags, ",") {
			if strings.TrimSpace(have) == tag {
				return true
			}
		}
	}
	return false
}

// collectCandidates 扫描一遍，返回已达门槛的候选，以及被时长挡下的数量（诊断用）
func (m *Manager) collectCandidates(list []models.QBTorrent, c models.StalledConfig, now int64) ([]candidate, int) {
	idleSec := int64(m.idleMinutes(c)) * 60
	thr := speedThreshold(c)
	m.mu.Lock()
	fz := make(map[string]int64, len(m.firstZero))
	for k, v := range m.firstZero {
		fz[k] = v
	}
	as := make(map[string]int64, len(m.activeSince))
	for k, v := range m.activeSince {
		as[k] = v
	}
	m.mu.Unlock()

	var out []candidate
	var waiting int
	seen := make(map[string]bool, len(list))
	for _, t := range list {
		if t.Hash == "" {
			continue
		}
		seen[t.Hash] = true
		if !stallStates[t.State] {
			// 脱离活动下载态：名额已经让出来了，两个时钟一起作废
			m.mu.Lock()
			delete(m.firstZero, t.Hash)
			delete(m.activeSince, t.Hash)
			m.mu.Unlock()
			continue
		}
		if as[t.Hash] == 0 {
			m.mu.Lock()
			m.activeSince[t.Hash] = now
			m.mu.Unlock()
			as[t.Hash] = now
		}
		zero, idle, _ := evalState(t, thr, fz[t.Hash], now)
		if !zero {
			// 仍有速度：零速计时重来，但名额还占着，activeSince 保留
			m.mu.Lock()
			delete(m.firstZero, t.Hash)
			m.mu.Unlock()
			continue
		}
		if protected(t, c) {
			continue
		}
		if fz[t.Hash] == 0 {
			m.mu.Lock()
			m.firstZero[t.Hash] = now
			m.mu.Unlock()
		}
		idle = capByObservation(idle, as[t.Hash], now)
		if idle < idleSec {
			waiting++
			continue
		}
		out = append(out, candidate{t: t, idleSec: idle})
	}
	// 清掉已不在 qB 里的任务的观测状态，避免无限累积
	m.mu.Lock()
	for h := range m.firstZero {
		if !seen[h] {
			delete(m.firstZero, h)
		}
	}
	for h := range m.activeSince {
		if !seen[h] {
			delete(m.activeSince, h)
		}
	}
	for h := range m.lastNotify {
		if !seen[h] {
			delete(m.lastNotify, h)
		}
	}
	m.mu.Unlock()

	// 最卡得久的排在前面：名额有限时优先回收它们
	sort.Slice(out, func(i, j int) bool { return out[i].idleSec > out[j].idleSec })
	return out, waiting
}

// ScanNow 执行一轮扫描 + 处置。手动触发与 ticker 共用。
func (m *Manager) ScanNow() {
	c := m.cfg.Get().Stalled
	if !c.Enabled || c.Mode == models.StalledModeOff {
		return
	}
	list, err := m.allTorrents()
	if err != nil {
		logger.Warn.Printf("停滞回收获取任务列表失败：%v", err)
		return
	}
	now := time.Now().Unix()
	cands, waiting := m.collectCandidates(list, c, now)
	if len(cands) == 0 {
		if waiting > 0 {
			logger.Debug.Printf("停滞回收：命中 0，%d 个仍在时长观察中", waiting)
		}
		return
	}

	mode := c.Mode
	capN := c.MaxPerRound
	if capN <= 0 {
		capN = 20
	}

	// 名额闸门：回收的唯一收益是把活动下载名额让给排队任务。queuedDL 就是
	// 「在等名额」的任务，一条都没有时暂停/下沉都是空操作，只会白打标、白刷审计。
	// notify 档是只读信息，不受闸门影响。
	if mode != models.StalledModeNotify {
		queued := 0
		for _, t := range list {
			if t.State == "queuedDL" {
				queued++
			}
		}
		if queued == 0 {
			logger.Debug.Printf("停滞回收：候选 %d，但没有 queuedDL 在等名额，本轮不动手", len(cands))
			return
		}
		// 要腾几个名额就处置几条：多停掉的没有任务接手，只是白白中断下载
		if queued < capN {
			capN = queued
		}
	}

	// 配额只计真正动手的条数。notify 档冷却中的候选不占配额，否则最卡的一批
	// 刚告警过就把名额吃光，后面从未告警的一条都轮不上。
	targets := make([]candidate, 0, capN)
	var cooled, overCap int
	for _, cand := range cands {
		if mode == models.StalledModeNotify && m.inCooldown(cand.t.Hash, c, now) {
			cooled++
			continue
		}
		if len(targets) >= capN {
			overCap++
			continue
		}
		targets = append(targets, cand)
	}

	var acted, failed, notified, demoted int
	switch mode {
	case models.StalledModeNotify:
		for _, cand := range targets {
			t := cand.t
			m.addAudit(auditEntry{TS: now, Hash: t.Hash, Name: t.Name, State: t.State,
				IdleMin: int(cand.idleSec / 60), DL: t.DownloadSpeed, UL: t.UploadSpeed,
				Action: "notify", OK: true})
			m.mu.Lock()
			m.lastNotify[t.Hash] = now
			m.mu.Unlock()
			notified++
		}
		if notified > 0 {
			m.sendNotify(notified, targets)
		}
	case models.StalledModePause, models.StalledModeDemote:
		acted, failed, demoted = m.actBatch(targets, c, mode, now)
	default:
		return // 未知档位一律不动手（config 层已兜底，这里是第二道闸）
	}

	if overCap > 0 {
		m.notifyOverCap(now, len(cands), capN, overCap, c)
	}
	if acted > 0 || failed > 0 || notified > 0 || demoted > 0 {
		m.save()
		logger.Info.Printf("停滞回收：候选 %d（观察中 %d，冷却中 %d）档位=%s 告警 %d 暂停 %d 下沉 %d 失败 %d 本轮超限未处理 %d",
			len(cands), waiting, cooled, mode, notified, acted, demoted, failed, overCap)
	}
}

// actBatch 处置一批候选。打标与暂停都一次批量提交：逐条发请求时 20 条候选
// 就是 40 次串行 HTTP，而端点原生支持 hashes 用 | 分隔。
//
// demote 的 bottomPrio 刻意保持逐条：qB 对多 hash 提交的落位顺序没有保证，
// 而这里顺序是有语义的——每次调用都把该条挪到队尾，只有最后提交的才真正沉底。
// targets 按 idle 降序，因此倒过来升序提交，让最卡的那条落在最底、最晚被提回下载。
func (m *Manager) actBatch(targets []candidate, c models.StalledConfig, mode string, now int64) (acted, failed, demoted int) {
	if len(targets) == 0 {
		return 0, 0, 0
	}
	hashes := make([]string, 0, len(targets))
	for _, cand := range targets {
		hashes = append(hashes, cand.t.Hash)
	}

	// 打标失败不影响处置（标记只是方便事后筛选与批量恢复）
	var detail string
	if tag := strings.TrimSpace(c.Tag); tag != "" {
		if err := m.client.AddTags(hashes, []string{tag}); err != nil {
			detail = "打标失败：" + err.Error()
			logger.Warn.Printf("停滞回收批量打标失败（%d 条）：%v", len(hashes), err)
		}
	}

	if mode == models.StalledModePause {
		if err := m.client.StopTorrents(hashes...); err != nil {
			for _, cand := range targets {
				t := cand.t
				m.addAudit(auditEntry{TS: now, Hash: t.Hash, Name: t.Name, State: t.State,
					IdleMin: int(cand.idleSec / 60), DL: t.DownloadSpeed, UL: t.UploadSpeed,
					Action: "paused", OK: false, Detail: "暂停失败：" + err.Error()})
			}
			logger.Warn.Printf("停滞回收批量暂停失败（%d 条）：%v", len(hashes), err)
			return 0, len(targets), 0
		}
		for _, cand := range targets {
			t := cand.t
			m.clearClocks(t.Hash)
			m.addAudit(auditEntry{TS: now, Hash: t.Hash, Name: t.Name, State: t.State,
				IdleMin: int(cand.idleSec / 60), DL: t.DownloadSpeed, UL: t.UploadSpeed,
				Action: "paused", OK: true, Detail: detail})
		}
		return len(targets), 0, 0
	}

	for i := len(targets) - 1; i >= 0; i-- {
		cand := targets[i]
		t := cand.t
		if err := m.client.BottomPrio(t.Hash); err != nil {
			failed++
			m.addAudit(auditEntry{TS: now, Hash: t.Hash, Name: t.Name, State: t.State,
				IdleMin: int(cand.idleSec / 60), DL: t.DownloadSpeed, UL: t.UploadSpeed,
				Action: "demoted", OK: false, Detail: "下沉队尾失败：" + err.Error()})
			logger.Warn.Printf("停滞回收下沉队尾失败 %s：%v", t.Name, err)
			continue
		}
		m.clearClocks(t.Hash)
		demoted++
		m.addAudit(auditEntry{TS: now, Hash: t.Hash, Name: t.Name, State: t.State,
			IdleMin: int(cand.idleSec / 60), DL: t.DownloadSpeed, UL: t.UploadSpeed,
			Action: "demoted", OK: true, Detail: detail})
	}
	return 0, failed, demoted
}

// clearClocks 处置成功后作废两个时钟：任务要么已暂停、要么已沉底，下次再看到它
// 应当重新获得完整宽限期，而不是凭旧时间戳立刻再处置一遍。
func (m *Manager) clearClocks(hash string) {
	m.mu.Lock()
	delete(m.firstZero, hash)
	delete(m.activeSince, hash)
	m.mu.Unlock()
}

// notifyOverCap 候选超过单轮上限时告警。截断本身是刻意的（积压逐轮排空），
// 但阈值配错同样表现为「每轮都超限」，必须让人看见，而不是只躺在日志里。
func (m *Manager) notifyOverCap(now int64, total, capN, over int, c models.StalledConfig) {
	cd := int64(c.NotifyCooldownMinutes) * 60
	if cd <= 0 {
		cd = 60 * 60
	}
	m.mu.Lock()
	fn := m.notify
	if now-m.lastBurst < cd {
		fn = nil
	} else {
		m.lastBurst = now
	}
	m.mu.Unlock()
	if fn == nil {
		return
	}
	fn("♻️ 停滞回收本轮超限",
		fmt.Sprintf("本轮命中 %d 条长期零速任务，单轮上限 %d，已处置 %d 条、剩 %d 条留待后续轮次。\n"+
			"若持续出现，请核对零速时长门槛与速度阈值是否配得过宽。", total, capN, total-over, over))
}

func (m *Manager) inCooldown(hash string, c models.StalledConfig, now int64) bool {
	m.mu.Lock()
	last := m.lastNotify[hash]
	m.mu.Unlock()
	cd := int64(c.NotifyCooldownMinutes) * 60
	if cd <= 0 {
		cd = 60 * 60
	}
	return last > 0 && now-last < cd
}

func (m *Manager) sendNotify(n int, cands []candidate) {
	m.mu.Lock()
	fn := m.notify
	m.mu.Unlock()
	if fn == nil {
		return
	}
	var lines []string
	for i, c := range cands {
		if i >= 8 {
			lines = append(lines, "…")
			break
		}
		lines = append(lines, "· "+c.t.Name+"（"+c.t.State+"，零速 "+fmtInt(c.idleSec/60)+" 分钟）")
	}
	fn("♻️ 停滞任务待回收 · "+strconv.Itoa(n)+" 个",
		"以下任务长期零速、占用活动下载名额（当前为只告警模式，未暂停）：\n"+strings.Join(lines, "\n"))
}

func fmtInt(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// ---- 试算（只读，不改 qB、不写状态）----

// PreviewItem 一条试算结果
type PreviewItem struct {
	Name     string `json:"name"`
	Hash     string `json:"hash"`
	State    string `json:"state"`
	IdleMin  int    `json:"idleMin"`
	DL       int64  `json:"dl"`
	UL       int64  `json:"ul"`
	WouldAct bool   `json:"wouldAct"` // 是否已达门槛会被处置
	Reason   string `json:"reason"`
}

// Preview 按传入（可能未保存）的配置试算当前任务，纯只读。
// 传 cfg=nil 时用已保存配置。
func (m *Manager) Preview(cfg *models.StalledConfig) ([]PreviewItem, error) {
	c := m.cfg.Get().Stalled
	if cfg != nil {
		c = *cfg
	}
	list, err := m.allTorrents()
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	idleSec := int64(m.idleMinutes(c)) * 60
	thr := speedThreshold(c)
	m.mu.Lock()
	fz := make(map[string]int64, len(m.firstZero))
	for k, v := range m.firstZero {
		fz[k] = v
	}
	as := make(map[string]int64, len(m.activeSince))
	for k, v := range m.activeSince {
		as[k] = v
	}
	m.mu.Unlock()

	out := make([]PreviewItem, 0, len(list))
	for _, t := range list {
		if !stallStates[t.State] {
			continue
		}
		if protected(t, c) {
			out = append(out, PreviewItem{Name: t.Name, Hash: t.Hash, State: t.State,
				Reason: "受保护（强制启动或保护标签）"})
			continue
		}
		zero, idle, why := evalState(t, thr, fz[t.Hash], now)
		if !zero {
			out = append(out, PreviewItem{Name: t.Name, Hash: t.Hash, State: t.State,
				DL: t.DownloadSpeed, UL: t.UploadSpeed, Reason: why})
			continue
		}
		// 与 collectCandidates 同一道封顶，试算结果才和真跑一轮一致
		observed := capByObservation(idle, as[t.Hash], now)
		act := observed >= idleSec
		reason := "零速时长未达门槛"
		if observed < idle {
			reason = "刚进入活动下载态，已观测 " + fmtInt(observed/60) + " 分钟（零速时长以观测跨度为上限）"
		}
		if act {
			reason = "命中：达门槛，将被" + actionLabel(c.Mode)
		}
		out = append(out, PreviewItem{Name: t.Name, Hash: t.Hash, State: t.State,
			IdleMin: int(observed / 60), DL: t.DownloadSpeed, UL: t.UploadSpeed,
			WouldAct: act, Reason: reason})
	}
	// 命中的排前面
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].WouldAct != out[j].WouldAct {
			return out[i].WouldAct
		}
		return out[i].IdleMin > out[j].IdleMin
	})
	if len(out) > 200 {
		out = out[:200]
	}
	// 与 ScanNow 的名额闸门和配额对齐，否则试算说「将被暂停」、真跑一轮却不动手
	if c.Mode == models.StalledModePause || c.Mode == models.StalledModeDemote {
		queued := 0
		for _, t := range list {
			if t.State == "queuedDL" {
				queued++
			}
		}
		limit := c.MaxPerRound
		if limit <= 0 {
			limit = 20
		}
		if queued < limit {
			limit = queued
		}
		hits := 0
		for i := range out {
			if !out[i].WouldAct {
				continue
			}
			if hits >= limit {
				out[i].WouldAct = false
				if queued == 0 {
					out[i].Reason = "没有排队任务在等名额，本轮不会处置"
				} else {
					out[i].Reason = "超出本轮名额（上限 " + strconv.Itoa(limit) + "），留待后续轮次"
				}
				continue
			}
			hits++
		}
	}
	return out, nil
}

func actionLabel(mode string) string {
	switch mode {
	case models.StalledModePause:
		return "暂停"
	case models.StalledModeDemote:
		return "下沉队尾"
	default:
		return "告警"
	}
}

// ---- 持久化 ----

type persisted struct {
	FirstZero   map[string]int64 `json:"firstZero"`
	ActiveSince map[string]int64 `json:"activeSince"`
	LastNotify  map[string]int64 `json:"lastNotify"`
}

func (m *Manager) load() {
	data, err := os.ReadFile(m.statePath)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Warn.Printf("停滞回收读取状态失败：%v", err)
		}
	} else {
		var p persisted
		if err := json.Unmarshal(data, &p); err != nil {
			logger.Warn.Printf("停滞回收解析状态失败：%v", err)
		} else {
			if p.FirstZero != nil {
				m.firstZero = p.FirstZero
			}
			if p.ActiveSince != nil {
				m.activeSince = p.ActiveSince
			}
			if p.LastNotify != nil {
				m.lastNotify = p.LastNotify
			}
		}
	}
	m.loadAudit()
}

func (m *Manager) save() {
	m.saveMu.Lock()
	defer m.saveMu.Unlock()
	m.mu.Lock()
	p := persisted{FirstZero: m.firstZero, ActiveSince: m.activeSince, LastNotify: m.lastNotify}
	data, err := json.MarshalIndent(p, "", "  ")
	m.mu.Unlock()
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(m.statePath), 0o755)
	tmp := m.statePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		logger.Warn.Printf("停滞回收写入状态失败：%v", err)
		return
	}
	_ = os.Rename(tmp, m.statePath)
	m.saveAudit()
}

func (m *Manager) addAudit(e auditEntry) {
	m.mu.Lock()
	m.audits = append(m.audits, e)
	if len(m.audits) > auditCap {
		m.audits = m.audits[len(m.audits)-auditCap:]
	}
	m.mu.Unlock()
}

// AuditList 返回最近的审计记录（新→旧）
func (m *Manager) AuditList(limit int) []auditEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 || limit > len(m.audits) {
		limit = len(m.audits)
	}
	out := make([]auditEntry, 0, limit)
	for i := len(m.audits) - 1; i >= len(m.audits)-limit; i-- {
		out = append(out, m.audits[i])
	}
	return out
}

func (m *Manager) loadAudit() {
	f, err := os.Open(m.auditPath)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Warn.Printf("停滞回收审计读取失败：%v", err)
		}
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e auditEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		m.audits = append(m.audits, e)
	}
	if len(m.audits) > auditCap {
		m.audits = m.audits[len(m.audits)-auditCap:]
	}
}

// saveAudit 调用方必须已持 saveMu（目前只有 save 调它），否则审计的 .tmp 同样会被并发截断。
func (m *Manager) saveAudit() {
	m.mu.Lock()
	entries := append([]auditEntry(nil), m.audits...)
	m.mu.Unlock()
	_ = os.MkdirAll(filepath.Dir(m.auditPath), 0o755)
	tmp := m.auditPath + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		logger.Warn.Printf("停滞回收审计写入失败：%v", err)
		return
	}
	w := bufio.NewWriter(f)
	for _, e := range entries {
		b, _ := json.Marshal(e)
		_, _ = w.Write(b)
		_, _ = w.WriteString("\n")
	}
	_ = w.Flush()
	_ = f.Close()
	_ = os.Rename(tmp, m.auditPath)
}
