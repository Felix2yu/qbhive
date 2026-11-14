package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/Felix2yu/qbhive/internal/models"
)

var (
	defaultConfig = models.AppConfig{
		Server: models.ServerConfig{Listen: ":8088"},
		Qbittorrent: models.QBConfig{
			URL:      "http://127.0.0.1:8080",
			Username: "admin",
			Password: "admin",
		},
		Notifier: models.NotifierConfig{
			Enabled: false,
		},
		RSS: models.RSSConfig{
			Enabled:  false,
			Interval: 15,
		},
		Limiter: models.LimiterConfig{
			Enabled:  false,
			Interval: 10,
		},
		FileManager: models.FileManagerConfig{
			Enabled:      false,
			ScanInterval: 15,
			CleanEnabled: false,
		},
		Archive: models.ArchiveConfig{
			Enabled:      false,
			Interval:     60,
			BatchSize:    100,
			QuietMinutes: 5,
			// 做种门槛起步值：实施后按真机 ratio / 完成至今分布再定
			MinRatio: 1.0,
			MinHours: 24,
			Guard: models.GuardConfig{
				Mode:                  models.GuardModeOff,
				Interval:              5,
				MaxPerRound:           20,
				NotifyCooldownMinutes: 10,
			},
		},
	}
)

type Manager struct {
	path string
	mu   sync.RWMutex
	cfg  models.AppConfig
}

func New(path string) *Manager {
	m := &Manager{path: path}
	if err := m.Load(); err != nil {
		m.cfg = defaultConfig
		applyConfigDefaults(&m.cfg)
		_ = m.Save()
	}
	return m
}

func (m *Manager) Load() error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	data, err := os.ReadFile(m.path)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		m.cfg = defaultConfig
		applyConfigDefaults(&m.cfg)
		return nil
	}
	if err := json.Unmarshal(data, &m.cfg); err != nil {
		return err
	}
	applyConfigDefaults(&m.cfg)
	return nil
}

// applyConfigDefaults 补齐「老配置文件里没有、但语义上必须有默认值」的字段。
// 重点是几个安全闸门（后处理 Verify、自动分类 OnlyIfEmpty、归档 DryRun / ExemptLostFiles）：
// 它们缺省必须视为开启，不能因为 JSON 里缺这个键就退化成关闭。
func applyConfigDefaults(cfg *models.AppConfig) {
	// 必须取指针：FileManagerConfig.PostProcess 是值字段，
	// 拷贝一份再改 Verify 只会改到副本，调用方看到的仍是 nil
	pp := &cfg.FileManager.PostProcess
	if pp.Verify == nil {
		on := true
		pp.Verify = &on
	}
	// 自动分类的「只处理未分类任务」同样是安全闸门：语义上默认开启，
	// 老配置缺这个键时若落成 false，一轮扫描就会把用户手工整理好的分类全冲掉
	ac := &cfg.AutoCategory
	if ac.OnlyIfEmpty == nil {
		on := true
		ac.OnlyIfEmpty = &on
	}
	if ac.Interval <= 0 {
		ac.Interval = 60
	}
	applyArchiveDefaults(&cfg.Archive)
}

// applyArchiveDefaults 补齐归档/守卫的缺省值。
//
// 两个指针布尔是安全闸门，缺键必须视为开启：DryRun 落成 false 会让老配置
// 一升级就开始真删任务；ExemptLostFiles 落成 false 只是少退一批，方向也安全。
//
// 做种门槛刻意不在这里补默认值：三个判据都是「<=0 表示不启用」，
// 老配置全为 0 时健康完成任务一条都不会退休，只有失去实体的那批会退——
// 这是升级后最安全的方向。新装的默认值由 defaultConfig 给。
func applyArchiveDefaults(a *models.ArchiveConfig) {
	if a.DryRun == nil {
		on := true
		a.DryRun = &on
	}
	if a.ExemptLostFiles == nil {
		on := true
		a.ExemptLostFiles = &on
	}
	if a.Interval <= 0 {
		a.Interval = 60
	}
	if a.BatchSize <= 0 {
		a.BatchSize = 100
	}
	if a.QuietMinutes <= 0 {
		a.QuietMinutes = 5
	}
	// 未知档位一律降为 off：配置里打错一个字不该变成「开始删文件」
	switch a.Guard.Mode {
	case models.GuardModeNotify, models.GuardModeRemove, models.GuardModeRemoveFiles:
	default:
		a.Guard.Mode = models.GuardModeOff
	}
	if a.Guard.Interval <= 0 {
		a.Guard.Interval = 5
	}
	if a.Guard.MaxPerRound <= 0 {
		a.Guard.MaxPerRound = 20
	}
	if a.Guard.NotifyCooldownMinutes <= 0 {
		a.Guard.NotifyCooldownMinutes = 10
	}
}

func (m *Manager) Save() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m.cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, m.path)
}

func (m *Manager) Get() models.AppConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg
}

func (m *Manager) Set(cfg models.AppConfig) {
	m.mu.Lock()
	m.cfg = cfg
	m.mu.Unlock()
}
