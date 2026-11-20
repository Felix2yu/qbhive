package archive

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/Felix2yu/qbhive/internal/config"
	"github.com/Felix2yu/qbhive/internal/models"
	qb "github.com/Felix2yu/qbhive/internal/qbittorrent"
)

// 两个并发的「保存配置」请求会同时走到 scheduler.ReloadAll → Reload。
// tickerStop 的读-close-重建必须串行化，否则 close of closed channel 直接崩进程。
// 间隔设成 1 小时，测试窗口内 ticker 不会触发，files 依赖也就用不上。
func TestRemoverReloadConcurrentNoPanic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	t.Setenv("QBHIVE_CONFIG", path)
	cm := config.New(path)
	full := cm.Get()
	full.Archive.Enabled = true
	full.Archive.Interval = 3600
	cm.Set(full)

	r := NewRemover(cm, qb.New("http://127.0.0.1:1", "u", "p", ""),
		NewStore(filepath.Join(dir, "archive.jsonl")), nil)
	r.Start()
	defer r.Stop()

	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				r.Reload(nil)
			}
		}()
	}
	wg.Wait()
}

func TestGuardReloadConcurrentNoPanic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	t.Setenv("QBHIVE_CONFIG", path)
	cm := config.New(path)
	full := cm.Get()
	full.Archive.Guard.Mode = models.GuardModeNotify
	full.Archive.Guard.Interval = 3600
	cm.Set(full)

	g := NewGuard(cm, qb.New("http://127.0.0.1:1", "u", "p", ""),
		NewStore(filepath.Join(dir, "archive.jsonl")), nil)
	g.Start()
	defer g.Stop()

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				g.Reload(nil)
			}
		}()
	}
	wg.Wait()
}
