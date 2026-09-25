package filemgr

import (
	"os"
	"path/filepath"
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
	// 上一周期已完成的 hash 集合，避免重复通知/处理
	done map[string]bool
}

func New(cfg *config.Manager, client *qb.Client) *Manager {
	return &Manager{cfg: cfg, client: client, stop: make(chan struct{}), done: make(map[string]bool)}
}

func (m *Manager) Start() {
	c := m.cfg.Get().FileManager
	if !c.Enabled {
		logger.Info.Println("FileManager disabled")
		return
	}
	interval := time.Duration(c.ScanInterval) * time.Second
	if interval <= 0 {
		interval = 15 * time.Second
	}
	logger.Info.Printf("FileManager started, interval=%s", interval)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
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
}

func (m *Manager) scan() {
	list, err := m.client.GetTorrents()
	if err != nil {
		logger.Warn.Printf("FileManager get torrents failed: %v", err)
		return
	}
	for _, t := range list {
		if t.State != "pausedUP" && t.State != "stalledUP" && t.State != "uploading" {
			continue
		}
		if m.done[t.Hash] {
			continue
		}
		// 通过 progress==1 确认真的完成
		if t.Progress < 0.999 {
			continue
		}
		m.done[t.Hash] = true
		m.handleCompleted(t)
	}
}

func (m *Manager) handleCompleted(t models.QBTorrent) {
	// 检查 save_path/torrentname 目录下是否只有一个文件
	torrentDir := filepath.Join(t.SavePath, t.Name)
	info, err := os.Stat(torrentDir)
	if err != nil {
		logger.Debug.Printf("FileManager: dir %s not exist, maybe already flat: %v", torrentDir, err)
		return
	}
	if !info.IsDir() {
		return
	}
	entries, err := os.ReadDir(torrentDir)
	if err != nil {
		logger.Warn.Printf("FileManager read dir %s failed: %v", torrentDir, err)
		return
	}
	// 过滤掉隐藏文件
	var files []os.DirEntry
	for _, e := range entries {
		if e.Name()[0] == '.' {
			continue
		}
		files = append(files, e)
	}
	if len(files) != 1 || !files[0].Type().IsRegular() {
		// 不是单文件场景
		return
	}

	src := filepath.Join(torrentDir, files[0].Name())
	dst := filepath.Join(t.SavePath, files[0].Name())
	if _, err := os.Stat(dst); err == nil {
		// 目标已存在，加后缀
		ext := filepath.Ext(files[0].Name())
		base := files[0].Name()[:len(files[0].Name())-len(ext)]
		dst = filepath.Join(t.SavePath, base+"_"+t.Hash[:8]+ext)
	}
	if err := os.Rename(src, dst); err != nil {
		logger.Warn.Printf("FileManager rename %s -> %s failed: %v", src, dst, err)
		return
	}
	// 再尝试移除空目录
	if err := os.Remove(torrentDir); err != nil {
		logger.Debug.Printf("FileManager remove dir %s failed: %v", torrentDir, err)
	} else {
		logger.Info.Printf("FileManager moved %s -> %s", files[0].Name(), dst)
	}
}

// Reset 重置完成状态（服务重启时调用）
func (m *Manager) Reset() {
	m.done = make(map[string]bool)
}
