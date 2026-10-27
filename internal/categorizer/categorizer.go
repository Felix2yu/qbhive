package categorizer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/Felix2yu/qbhive/internal/config"
	"github.com/Felix2yu/qbhive/internal/logger"
	"github.com/Felix2yu/qbhive/internal/models"
	qb "github.com/Felix2yu/qbhive/internal/qbittorrent"
)

// stateFile 持久化「每个任务当前被打到哪个分类、以及当时生效的规则指纹」。
// 作用有两个：一是避免每轮重复调用 qB 接口（幂等减负），二是规则变更后能识别
// 出「这条结论是旧规则得出的」，从而重新评估。
const stateFile = "categorizer_state.json"

// setCategoryBatchSize 单次 setCategory 提交的 hash 数量上限。
// qB 用表单字段传 hashes（| 分隔），一次塞几千个会撞到 URL/表单长度限制。
const setCategoryBatchSize = 200

// defaultInterval 默认扫描间隔（秒）。分类是低频动作，不需要像限速那样十几秒一轮。
const defaultInterval = 60

// Manager 自动分类引擎：按用户配置的规则给种子任务打 qB 分类。
//
// 自带 ticker，不挂在任何别的模块的扫描循环里——分类与文件管理是两件独立的
// 事，共用循环会让两者的开关、间隔互相绑死（关掉归档就顺带关掉分类，
// 或反过来为了分类必须打开归档）。
type Manager struct {
	cfg    *config.Manager
	client *qb.Client

	// 共享任务快照：就绪后不再让 qB 重新序列化全表（7000+ 条约 20MB）
	torrentCache *qb.TorrentCache

	stop       chan struct{}
	tickerStop chan struct{}

	mu        sync.Mutex
	statePath string
	state     map[string]*catRecord

	// 编译缓存：规则指纹没变就不重新编译正则（仅 scan goroutine 读写，无并发）
	rulesKey     string
	rulesCache   []compiledRule
	fallbackCache string
}

type catRecord struct {
	Cat string `json:"cat,omitempty"` // 本引擎给该任务打上的分类
	Sig string `json:"sig,omitempty"` // 打分类时生效的规则指纹
}

// SetTorrentCache 注入共享任务快照（装配层在 Start 之前调用一次）
func (m *Manager) SetTorrentCache(c *qb.TorrentCache) { m.torrentCache = c }

// SetClient 热替换 qb 客户端
func (m *Manager) SetClient(c *qb.Client) { m.client = c }

func defaultStatePath() string {
	if cfg := os.Getenv("QBHIVE_CONFIG"); cfg != "" {
		return filepath.Join(filepath.Dir(cfg), stateFile)
	}
	return filepath.Join("data", stateFile)
}

func New(cfg *config.Manager, client *qb.Client) *Manager {
	m := &Manager{
		cfg:       cfg,
		client:    client,
		stop:      make(chan struct{}),
		statePath: defaultStatePath(),
		state:     make(map[string]*catRecord),
	}
	m.loadState()
	return m
}

func (m *Manager) loadState() {
	data, err := os.ReadFile(m.statePath)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Warn.Printf("自动分类读取状态失败：%v", err)
		}
		return
	}
	if err := json.Unmarshal(data, &m.state); err != nil {
		logger.Warn.Printf("自动分类解析状态失败：%v", err)
		m.state = make(map[string]*catRecord)
	}
}

func (m *Manager) saveState() {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, err := json.MarshalIndent(m.state, "", "  ")
	if err != nil {
		return
	}
	dir := filepath.Dir(m.statePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	tmp := m.statePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		logger.Warn.Printf("自动分类写入状态失败：%v", err)
		return
	}
	_ = os.Rename(tmp, m.statePath)
}

// Reload 按最新配置重启（或停止）扫描轮询
func (m *Manager) Reload(c *qb.Client) {
	if c != nil {
		m.client = c
	}
	if m.tickerStop != nil {
		close(m.tickerStop)
		m.tickerStop = nil
	}
	ac := m.cfg.Get().AutoCategory
	if !ac.Enabled {
		logger.Info.Println("自动分类已停用（重载）")
		return
	}
	logger.Info.Printf("自动分类重载，间隔=%s", m.interval())
	m.runTicker(m.interval())
}

func (m *Manager) interval() time.Duration {
	sec := m.cfg.Get().AutoCategory.Interval
	if sec <= 0 {
		sec = defaultInterval
	}
	return time.Duration(sec) * time.Second
}

func (m *Manager) Start() {
	ac := m.cfg.Get().AutoCategory
	if !ac.Enabled {
		logger.Info.Println("自动分类已停用")
		return
	}
	logger.Info.Printf("自动分类启动，间隔=%s", m.interval())
	m.runTicker(m.interval())
}

func (m *Manager) runTicker(interval time.Duration) {
	m.tickerStop = make(chan struct{})
	stop := m.tickerStop
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		// 启动后 10 秒先跑一次，避免刚配好规则要等满一个间隔才看到效果
		time.AfterFunc(10*time.Second, func() {
			select {
			case <-stop:
			case <-m.stop:
			default:
				m.scan()
			}
		})
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
	m.saveState()
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

// scan 一轮分类：求目标分类 → 按分类聚合 → 批量提交 qB
func (m *Manager) scan() {
	ac := m.cfg.Get().AutoCategory
	if !ac.Enabled {
		return
	}
	rules, err := m.ensureRules(ac)
	if err != nil {
		// 规则写坏了：本轮整体不动作，绝不带着错误的匹配器去改用户的数据
		logger.Warn.Printf("自动分类规则编译失败，本轮跳过：%v", err)
		return
	}
	sig := rulesSignature(ac)

	list, err := m.allTorrents()
	if err != nil {
		logger.Warn.Printf("自动分类获取任务列表失败：%v", err)
		return
	}

	onlyIfEmpty := ac.OnlyIfEmpty == nil || *ac.OnlyIfEmpty
	// 分类 → 待设置的任务，聚合后每个分类只调一次 qB 接口
	pending := make(map[string][]string)
	applied := make(map[string]string)   // hash → 目标分类，调 qB 成功后才落状态
	refreshed := make(map[string]string) // hash → 现状已符合、只需刷新指纹的
	var skipped, unchanged, metaPending int
	var toClear []string // 之前误打、现在仍未就绪、需清除错误分类的任务

	for _, t := range list {
		if !readyForCategory(t) {
			metaPending++
			// 元数据未就绪却被记了分类：必是历史误打（就绪前我们从不分类）。
			// 若 qB 现状仍等于 categorizer 记录值（用户没手动改过），才清掉这个
			// 错误分类；用户改过的尊重用户、不碰。不论哪种情况都让 categorizer
			// 忘记这条记录，元数据到位后下一轮重新评估。
			m.mu.Lock()
			rec := m.state[t.Hash]
			m.mu.Unlock()
			if rec != nil && rec.Cat != "" {
				if t.Category == rec.Cat {
					toClear = append(toClear, t.Hash)
				}
				m.mu.Lock()
				delete(m.state, t.Hash)
				m.mu.Unlock()
			}
			continue
		}
		if onlyIfEmpty && t.Category != "" {
			skipped++
			continue
		}
		target, _ := evaluate(t.Name, rules, ac.Fallback)
		if target == "" {
			skipped++
			continue
		}
		m.mu.Lock()
		rec := m.state[t.Hash]
		m.mu.Unlock()
		if rec != nil && rec.Cat == target && rec.Sig == sig {
			unchanged++
			continue
		}
		// 现状已经等于目标分类：只把状态刷新到新指纹，没必要再调一次 qB。
		// 常见于规则改了但结论没变（例如把阈值从 15 调到 20，这个任务两头都命中）。
		if target == t.Category {
			refreshed[t.Hash] = target
			unchanged++
			continue
		}
		pending[target] = append(pending[target], t.Hash)
		applied[t.Hash] = target
	}

	var failed int
	for cat, hashes := range pending {
		for i := 0; i < len(hashes); i += setCategoryBatchSize {
			end := i + setCategoryBatchSize
			if end > len(hashes) {
				end = len(hashes)
			}
			batch := hashes[i:end]
			if err := m.client.SetCategory(batch, cat); err != nil {
				logger.Warn.Printf("自动分类设置分类 %s 失败（%d 个任务）：%v", cat, len(batch), err)
				failed += len(batch)
				for _, h := range batch {
					delete(applied, h)
				}
				continue
			}
		}
	}

	// 清除历史误打的分类：仅对 qB 现状仍等于记录值（用户没手动改过）的任务调
	// setCategory("") 取消分类。传空字符串是 qB 取消分类的标准用法。
	var clearFailed int
	for i := 0; i < len(toClear); i += setCategoryBatchSize {
		end := i + setCategoryBatchSize
		if end > len(toClear) {
			end = len(toClear)
		}
		batch := toClear[i:end]
		if err := m.client.SetCategory(batch, ""); err != nil {
			logger.Warn.Printf("自动分类清除误打分类失败（%d 个任务）：%v", len(batch), err)
			clearFailed += len(batch)
			continue
		}
	}

	m.mu.Lock()
	for h, cat := range applied {
		m.state[h] = &catRecord{Cat: cat, Sig: sig}
	}
	for h, cat := range refreshed {
		m.state[h] = &catRecord{Cat: cat, Sig: sig}
	}
	m.mu.Unlock()

	changed := len(applied)
	if changed > 0 || len(refreshed) > 0 || len(toClear) > 0 {
		m.saveState()
	}
	if changed > 0 || failed > 0 || metaPending > 0 || len(toClear) > 0 {
		logger.Info.Printf("自动分类：设置 %d，跳过 %d，已符合 %d，待元数据 %d，清误打 %d，失败 %d",
			changed, skipped, unchanged, metaPending, len(toClear)-clearFailed, failed+clearFailed)
	}
}

// ensureRules 规则指纹变化时重新编译（仅 scan goroutine 调用，无并发）
func (m *Manager) ensureRules(ac models.AutoCategoryConfig) ([]compiledRule, error) {
	key := rulesSignature(ac)
	if key == m.rulesKey && m.rulesCache != nil {
		return m.rulesCache, nil
	}
	compiled, err := compileRules(ac.Rules)
	if err != nil {
		return nil, err
	}
	m.rulesKey = key
	m.rulesCache = compiled
	m.fallbackCache = ac.Fallback
	return compiled, nil
}

// unreadyNameRe 匹配「元数据尚未就绪」时 qB 返回的名字：40 位十六进制 infohash
// 原文，或以 magnet: 开头的 magnet 链接。这两类名字的拉丁字母数在 17~42，必然
// 越过任何 latinCount 阈值而被判成「外文」。更麻烦的是：一旦在这个阶段打上分类，
// 后续轮次就会被「只处理未分类任务」的闸门挡住，永远等不到用真实名字重新评估
// 的机会——一个错分类被彻底固化。
//
// 用名字特征而非枚举 state 判定：qB 排队中的磁链（state=queuedDL）名字同样是
// hex/magnet 原文，却不在 metaDL/forcedMetaDL 这组里，单纯枚举状态会漏判
// （这就是 ddc2b109... 被误打成「外文」的真实原因）。只要名字还是 hash/magnet，
// 无论处于哪个未就绪态都先跳过，覆盖最全。
var unreadyNameRe = regexp.MustCompile(`^(?:magnet:|[0-9a-fA-F]{40}$)`)

// readyForCategory 判断一个任务现在能不能安全地定分类。
//
// 磁链刚添加时 qB 手上只有 infohash，还没有种子元数据，所以 info 接口返回的
// 名字是 40 位十六进制 hash 或 magnet 链接原文。元数据到位之前一律不动，
// 等名字变成真实种子名后再说。
func readyForCategory(t models.QBTorrent) bool {
	if unreadyNameRe.MatchString(t.Name) {
		return false
	}
	// 第二道兜底：元数据未到位时 qB 的 total size 是 -1，防止将来 qB 在名字
	// 仍是真实名、却把 size 填成 -1 的异常情况。刻意只拦负值而不要求 Size > 0
	// ——万一 qB 某个版本不返回 size（字段缺失=0），要求正数会让整个功能静默
	// 失效，那比偶尔误判一个任务的代价大得多。
	return t.Size >= 0
}

// evaluate 求一个任务名的分类结论：按规则顺序先命中先得，全不中则回落到兜底。
// 返回目标分类与命中的规则名（命中为空表示走的是兜底）。
func evaluate(name string, rules []compiledRule, fallback string) (target string, ruleName string) {
	for _, r := range rules {
		if r.Match(name) {
			return r.rule.Category, r.rule.Name
		}
	}
	return fallback, ""
}

// PreviewItem 一条试算结果，只用于设置页展示，不改变任何数据
type PreviewItem struct {
	Name     string `json:"name"`
	Current  string `json:"current"`  // 当前 qB 分类
	Target   string `json:"target"`   // 将要打上的分类（空表示不动）
	Rule     string `json:"rule"`     // 命中的规则名（空表示兜底或未命中）
	Reason   string `json:"reason"`   // 不动的原因
}

// Preview 试算分类结果，不写 qB、不落状态。
//
// ac 由调用方传入而不是内部读配置：设置页要在**还没保存**的规则上看效果，
// 若这里读已保存的配置，用户改完规则点试算看到的仍是旧结论，比不试算更误导。
//
// names 非空时只试算这些名字；否则从 qB 任务列表里取至多 limit 条
// （优先未分类的，未分类的不够就用已有的补齐，保证一定能看到效果）。
func (m *Manager) Preview(ac models.AutoCategoryConfig, names []string, limit int) ([]PreviewItem, error) {
	rules, err := compileRules(ac.Rules)
	if err != nil {
		return nil, err
	}
	onlyIfEmpty := ac.OnlyIfEmpty == nil || *ac.OnlyIfEmpty

	if len(names) > 0 {
		out := make([]PreviewItem, 0, len(names))
		for _, n := range names {
			target, rule := evaluate(n, rules, ac.Fallback)
			it := PreviewItem{Name: n, Target: target, Rule: rule}
			if target == "" {
				it.Reason = "没有规则命中且未设置兜底分类"
			}
			out = append(out, it)
		}
		return out, nil
	}

	list, err := m.allTorrents()
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 20
	}
	// 未分类的优先，能让用户一眼看到规则对真正会被处理的任务的效果
	var picked []models.QBTorrent
	for _, t := range list {
		if t.Category == "" {
			picked = append(picked, t)
			if len(picked) >= limit {
				break
			}
		}
	}
	for _, t := range list {
		if len(picked) >= limit {
			break
		}
		if t.Category != "" {
			picked = append(picked, t)
		}
	}

	out := make([]PreviewItem, 0, len(picked))
	for _, t := range picked {
		it := PreviewItem{Name: t.Name, Current: t.Category}
		if !readyForCategory(t) {
			it.Reason = "磁链尚未获取元数据，等元数据到位后才会分类"
			out = append(out, it)
			continue
		}
		if onlyIfEmpty && t.Category != "" {
			it.Reason = "已有分类，按「只处理未分类任务」跳过"
			out = append(out, it)
			continue
		}
		target, rule := evaluate(t.Name, rules, ac.Fallback)
		it.Target, it.Rule = target, rule
		if target == "" {
			it.Reason = "没有规则命中且未设置兜底分类"
		}
		out = append(out, it)
	}
	return out, nil
}
