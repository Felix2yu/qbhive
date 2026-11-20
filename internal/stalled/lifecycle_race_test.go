package stalled

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
// 间隔设成 1 小时，测试窗口内 ticker 不会触发，不产生任何 qB 请求。
func TestReloadConcurrentNoPanic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	t.Setenv("QBHIVE_CONFIG", path)
	cm := config.New(path)
	full := cm.Get()
	full.Stalled = models.StalledConfig{
		Enabled: true, Mode: models.StalledModeNotify, Interval: 3600,
		IdleMinutes: 30, SpeedKiB: 5, MaxPerRound: 20,
	}
	cm.Set(full)

	m := New(cm, qb.New("http://127.0.0.1:1", "u", "p", ""))
	m.Start()
	defer m.Stop()

	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				m.Reload(nil)
			}
		}()
	}
	wg.Wait()
}
