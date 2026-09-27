package filemgr

import (
	"os"
	"path/filepath"
	"strings"
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
}

func (m *Manager) scan() {
	list, err := m.client.GetTorrents()
	if err != nil {
		logger.Warn.Printf("文件管理获取任务列表失败：%v", err)
		return
	}
	for _, t := range list {
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

// handleCompleted 检查并扁平化只含单个普通文件的 torrent 目录。
// 返回值:
//   already=true  — 确认不是目标场景（single file torrent、内容路径不存在、文件数不符），
//                   调用方应标记 done 不再重试
//   didWork=true  — 本轮工作已完成（残留目录已清理），调用方应标记 done
//   两者都 false  — 临时错误，或 qB 移动已提交但待下轮清理，下次 scan 重试
func (m *Manager) handleCompleted(t models.QBTorrent) (already bool, didWork bool) {
	// 5.x 的 content_path 是最准确的内容路径（单文件种子=文件完整路径，
	// 多文件种子=内容根目录），避免任务名与磁盘目录名不一致时误判「目录不存在」；
	// 为空时回退 save_path/name
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
		// 目录已无可见文件：多是上一轮移动落地后的残留空壳（可能还有隐藏文件），
		// 直接删除不会丢数据
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
	// qB renameFile 的路径基准是相对 save_path 的 torrent 内部路径；
	// content_path 的目录名可能与任务名不一致，按实际目录相对 save_path 推导
	torrentRelativeOld := t.Name + "/" + fileName
	if rel, err := filepath.Rel(t.SavePath, torrentDir); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
		torrentRelativeOld = filepath.ToSlash(filepath.Join(rel, fileName))
	}
	torrentRelativeNew := fileName

	// 1) 先通过 qB API 停止 torrent
	if err := m.client.StopTorrents(t.Hash); err != nil {
		logger.Warn.Printf("文件管理 [%s] 停止失败：%v", t.Name, err)
	}

	// 2) 走 qB renameFile API，失败时回退本地 os.Rename
	viaQB := true
	if err := m.client.RenameFile(t.Hash, torrentRelativeOld, torrentRelativeNew); err != nil {
		logger.Warn.Printf("文件管理 [%s] qB renameFile 失败（%s → %s）：%v，回退本地重命名",
			t.Name, torrentRelativeOld, torrentRelativeNew, err)
		viaQB = false
		src := filepath.Join(torrentDir, fileName)
		dst := filepath.Join(t.SavePath, fileName)
		if _, e := os.Stat(dst); e == nil {
			ext := filepath.Ext(fileName)
			base := fileName[:len(fileName)-len(ext)]
			suffix := t.Hash
			if len(suffix) > 8 {
				suffix = suffix[:8]
			}
			dst = filepath.Join(t.SavePath, base+"_"+suffix+ext)
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
		logger.Info.Printf("文件管理 [%s] 已提交移动 %s → %s（qB API）", t.Name, torrentRelativeOld, torrentRelativeNew)
		// qB renameFile 的磁盘移动是异步的（libtorrent disk thread），
		// API 返回 200 时文件可能还在原地，立即 RemoveAll 会把尚未移走的
		// 文件一起删掉造成数据丢失。这里不删目录，返回 false,false 使
		// 任务不标 done，等下一轮扫描时目录已空再清理。
		return false, false
	}
	logger.Info.Printf("文件管理 [%s] 已本地移动 %s → %s", t.Name, torrentRelativeOld, torrentRelativeNew)
	// 本地 os.Rename 是同步的，移动已落地，可直接清理残留目录
	if err := os.RemoveAll(torrentDir); err != nil {
		logger.Warn.Printf("文件管理 [%s] 清理目录 %s 失败：%v，下次重试", t.Name, torrentDir, err)
		return false, false
	}
	return false, true
}

// Reset 重置完成状态（服务重启时调用）
func (m *Manager) Reset() {
	m.done = make(map[string]bool)
}
