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
// 目前只有后处理的 Verify：它是删除源文件前的防误删闸门，缺省open 必须视为开启，
// 不能因为 JSON 里缺这个键就退化成关闭。
func applyConfigDefaults(cfg *models.AppConfig) {
	// 必须取指针：FileManagerConfig.PostProcess 是值字段，
	// 拷贝一份再改 Verify 只会改到副本，调用方看到的仍是 nil
	pp := &cfg.FileManager.PostProcess
	if pp.Verify == nil {
		on := true
		pp.Verify = &on
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
