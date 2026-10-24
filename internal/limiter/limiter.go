package limiter

import (
	"math"
	"regexp"
	"time"

	"github.com/Felix2yu/qbhive/internal/config"
	"github.com/Felix2yu/qbhive/internal/logger"
	qb "github.com/Felix2yu/qbhive/internal/qbittorrent"
)

// ratioDeadbandPct 比率每轮都在涨，重算出的限速也会跟着微小变化；
// 与上次下发值相差在此百分比内就不调用 qB 接口，否则每次扫描都会全量刷一遍限速。
const ratioDeadbandPct = 10

// ratioScaledLimit 按分享比率连续压速：比率不超过目标时保持基准值，
// 超出后按 target/ratio 双曲下降，最低 1KB/s——qB 的 0 不表示「压到底」，不能下发。
func ratioScaledLimit(baseKB int, target, ratio float64) int {
	if baseKB <= 0 || target <= 0 || ratio <= target {
		return baseKB
	}
	kb := int(math.Round(target / ratio * float64(baseKB)))
	if kb < 1 {
		kb = 1
	}
	return kb
}

// withinRatioDeadband 判断比率压速的新值是否离上次下发值足够近、可以跳过下发
func withinRatioDeadband(prev, next int64) bool {
	if prev <= 0 || next <= 0 {
		return false
	}
	d := prev - next
	if d < 0 {
		d = -d
	}
	return d*100 <= prev*ratioDeadbandPct
}

// Limiter 定时扫描 torrent 列表，根据匹配规则设置上传限速
type Limiter struct {
	cfg    *config.Manager
	client *qb.Client
	stop   chan struct{}

	// 运行中的 ticker 控制：每次 Reload/Start 时重建
	tickerStop chan struct{}
	running    bool

	// 上一次下发的限速（hash → limit 字节/秒），避免对值未变的 torrent 重复调用 qB API
	lastLimits map[string]int64
}

func New(cfg *config.Manager, client *qb.Client) *Limiter {
	return &Limiter{cfg: cfg, client: client, stop: make(chan struct{}), lastLimits: make(map[string]int64)}
}

// SetClient 热替换 qb 客户端（配置改了连哪台 qB 都变了）
func (l *Limiter) SetClient(c *qb.Client) {
	l.client = c
}

// Reload 根据最新配置重启（或停止）限速轮询。
// client 和 cfg 可分别传入 nil / 空，Reload 会沿用已有指针。
func (l *Limiter) Reload(c *qb.Client) {
	if c != nil {
		l.client = c
	}
	// 停掉旧的 ticker goroutine
	if l.tickerStop != nil {
		close(l.tickerStop)
		l.tickerStop = nil
		l.running = false
	}
	// 规则可能变更了，清空前次记录让所有 torrent 重新走一次匹配+下发
	l.lastLimits = make(map[string]int64)
	cfg := l.cfg.Get().Limiter
	if !cfg.Enabled {
		logger.Info.Println("限速器已停用（重载）")
		return
	}
	interval := time.Duration(cfg.Interval) * time.Second
	if interval <= 0 {
		interval = 10 * time.Second
	}
	logger.Info.Printf("限速器重载，间隔=%s", interval)
	l.runTicker(interval)
}

func (l *Limiter) Start() {
	c := l.cfg.Get().Limiter
	if !c.Enabled {
		logger.Info.Println("限速器已停用")
		return
	}
	interval := time.Duration(c.Interval) * time.Second
	if interval <= 0 {
		interval = 10 * time.Second
	}
	logger.Info.Printf("限速器启动，间隔=%s", interval)
	l.runTicker(interval)
}

func (l *Limiter) runTicker(interval time.Duration) {
	l.tickerStop = make(chan struct{})
	l.running = true
	stop := l.tickerStop
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
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
	torrents, err := l.client.GetTorrents("all")
	if err != nil {
		logger.Warn.Printf("限速器获取任务列表失败：%v", err)
		return
	}
	// 本轮命中的 hash 集合：结束后把没命中过的 hash 从 lastLimits 里剔掉（对应 torrent 删了/规则改了）
	seen := make(map[string]bool, len(torrents))
	changes := 0
	for _, t := range torrents {
		seen[t.Hash] = true
		var (
			limit    int64 = -1 // 默认没命中规则 = 无限制
			matched        = false
			viaRatio       = false
		)
		for _, r := range rules {
			if !r.Enabled {
				continue
			}
			ok, err := regexp.MatchString(r.Match, t.Name)
			if err != nil {
				continue
			}
			if ok {
				kb := r.UploadLimit
				// 比率维度要求基准限速是个确定值：基准「不限制」无从缩放
				if r.RatioTarget > 0 && kb > 0 && t.Uploaded >= r.MinUploaded*1024*1024 {
					if scaled := ratioScaledLimit(kb, r.RatioTarget, t.Ratio); scaled < kb {
						kb = scaled
						viaRatio = true
					}
				}
				if kb > 0 {
					limit = int64(kb) * 1024
				}
				matched = true
				break
			}
		}
		// 没命中规则的 torrent，保持 qB 原生限速不动（不下发 -1 覆盖用户手动限速）
		if !matched {
			continue
		}
		prev, known := l.lastLimits[t.Hash]
		if known && prev == limit {
			continue // 值没变，跳过 API 调用
		}
		// 比率压速用相对迟滞：比率的微小漂移不值得再打一次 qB 接口
		if known && viaRatio && withinRatioDeadband(prev, limit) {
			continue
		}
		if err := l.client.SetUploadLimit(t.Hash, limit); err != nil {
			logger.Warn.Printf("限速器设置限速 hash=%s 失败：%v", t.Hash, err)
			continue
		}
		l.lastLimits[t.Hash] = limit
		changes++
		if viaRatio {
			logger.Debug.Printf("限速器：任务 %s 比率 %.2f 已超目标，压速到 %d", t.Name, t.Ratio, limit)
		} else {
			logger.Debug.Printf("限速器：任务 %s 限速 %d（规则命中）", t.Name, limit)
		}
	}
	// 清理已消失 torrent 的 lastLimits
	for h := range l.lastLimits {
		if !seen[h] {
			delete(l.lastLimits, h)
		}
	}
	if changes > 0 {
		logger.Info.Printf("限速器应用完成：%d 个任务被修改", changes)
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
		logger.Info.Printf("应用规则 %s → %s 上传限速 %d", r.Name, name, r.UploadLimit)
		return
	}
}
