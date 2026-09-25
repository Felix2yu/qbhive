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

	// 运行中的 ticker 控制
	tickerStop chan struct{}

	// 上一周期已完成的 hash 集合，避免重复通知/处理
	done map[string]bool
}

func New(cfg *config.Manager, client *qb.Client) *Manager {
	return &Manager{cfg: cfg, client: client, stop: make(chan struct{}), done: make(map[string]bool)}
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
		logger.Info.Println("FileManager disabled (via reload)")
		return
	}
	interval := time.Duration(cfg.ScanInterval) * time.Second
	if interval <= 0 {
		interval = 15 * time.Second
	}
	logger.Info.Printf("FileManager reloaded, interval=%s", interval)
	m.runTicker(interval)
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
}

func (m *Manager) scan() {
	list, err := m.client.GetTorrents()
	if err != nil {
		logger.Warn.Printf("FileManager get torrents failed: %v", err)
		return
	}
	for _, t := range list {
		// 只处理已暂停的已完成任务，避免破坏正在做种/下载的 torrent 数据库
		if t.State != "pausedUP" {
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

	fileName := files[0].Name()
	torrentRelativeOld := t.Name + "/" + fileName // qB renameFile 需要的 torrent 内相对路径
	torrentRelativeNew := fileName                // 目标：直接放到 torrent 根目录（即 savePath 下）

	// 1) 先通过 qB API 暂停 torrent（已经是 pausedUP，这里只是防御）
	if err := m.client.PauseTorrents(t.Hash); err != nil {
		logger.Warn.Printf("FileManager pause torrent %s failed: %v", t.Name, err)
		// 继续尝试，失败再回退
	}

	// 2) 走 qB renameFile API，让 qB 感知文件移动，保护做种一致性
	done := false
	if err := m.client.RenameFile(t.Hash, torrentRelativeOld, torrentRelativeNew); err != nil {
		logger.Warn.Printf("FileManager qB renameFile failed (%s -> %s): %v, falling back to local os.Rename",
			torrentRelativeOld, torrentRelativeNew, err)
		// 回退：本地 os.Rename（仅在 qB renameFile 不可用时，且任务已暂停）
		src := filepath.Join(torrentDir, fileName)
		dst := filepath.Join(t.SavePath, fileName)
		if _, e := os.Stat(dst); e == nil {
			ext := filepath.Ext(fileName)
			base := fileName[:len(fileName)-len(ext)]
			dst = filepath.Join(t.SavePath, base+"_"+t.Hash[:8]+ext)
		}
		if err := os.Rename(src, dst); err != nil {
			logger.Warn.Printf("FileManager local rename %s -> %s failed: %v", src, dst, err)
			// 失败尝试恢复 torrent，避免用户以为还在暂停
			_ = m.client.ResumeTorrents(t.Hash)
			return
		}
	} else {
		done = true
	}

	// 3) 恢复 torrent（仅针对 qB renameFile 成功 / 本地回退成功）
	if err := m.client.ResumeTorrents(t.Hash); err != nil {
		logger.Warn.Printf("FileManager resume torrent %s failed: %v", t.Name, err)
	}

	// 4) 尝试移除空目录（qB renameFile 不会自动清理空目录）
	if err := os.Remove(torrentDir); err != nil {
		logger.Debug.Printf("FileManager remove dir %s failed: %v", torrentDir, err)
	} else {
		logger.Info.Printf("FileManager moved %s -> %s (%s)", torrentRelativeOld, torrentRelativeNew,
			map[bool]string{true: "via qB API", false: "fallback local"}[done])
	}
}

// Reset 重置完成状态（服务重启时调用）
func (m *Manager) Reset() {
	m.done = make(map[string]bool)
}
