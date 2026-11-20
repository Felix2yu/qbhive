package rss

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Felix2yu/qbhive/internal/config"
	qb "github.com/Felix2yu/qbhive/internal/qbittorrent"
)

// 两个并发的「保存配置」请求会同时走到 scheduler.ReloadAll → Reload。
// fetchStop/saveStop 的读-close-重建必须串行化，否则 close of closed channel 直接崩进程。
// 不配任何 feed，runPoller 启动时那次立即 fetchAll 不会发出请求。
// 也兜住「Stop 返回后仍有协程落盘」——那会让 t.TempDir 清理报 directory not empty。
func TestReloadConcurrentNoPanic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	t.Setenv("QBHIVE_CONFIG", path)
	cm := config.New(path)
	full := cm.Get()
	full.RSS.Enabled = true
	full.RSS.Interval = 60
	full.RSS.Feeds = nil
	cm.Set(full)

	e := New(cm, qb.New("http://127.0.0.1:1", "u", "p", ""))
	e.Start()
	defer e.Stop()

	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				e.Reload(nil)
			}
		}()
	}
	wg.Wait()
}

// saveSeen 有多个并发调用者（ticker、fetchFeed 的首次快照、Stop），却共用同一个 .tmp 路径。
// A 写到一半被 B 截断覆盖，A 再 rename 出去的就是半截 JSON；重启时整张去重表解析失败被丢弃，
// 历史条目于是被当成新条目重新下载一遍。
//
// 两个细节决定这条测试能不能真的抓到问题：
//  1. 每次落盘内容必须长度不同（不断追加 key），否则交错写出来的字节仍然一模一样；
//  2. 必须在写的过程中读回校验，损坏是瞬时的，只查最后一次会被后续的完整写覆盖掉。
func TestSaveSeenConcurrentAlwaysValidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("QBHIVE_CONFIG", path)
	e := New(config.New(path), qb.New("http://127.0.0.1:1", "u", "p", ""))
	e.seen["f1"] = make(map[string]bool)

	var bad atomic.Int64
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				e.mu.Lock()
				e.seen["f1"][fmt.Sprintf("g%d-%d", g, i)] = true
				e.mu.Unlock()
				e.saveSeen()
				if data, err := os.ReadFile(e.stateFile); err == nil {
					var p seenPersist
					if json.Unmarshal(data, &p) != nil {
						bad.Add(1)
					}
				}
			}
		}(g)
	}
	wg.Wait()

	if n := bad.Load(); n > 0 {
		t.Errorf("并发落盘期间读到 %d 次坏 JSON（去重表会在重启时整张丢失）", n)
	}
	data, err := os.ReadFile(e.stateFile)
	if err != nil {
		t.Fatalf("读回落盘文件：%v", err)
	}
	var p seenPersist
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatalf("最终文件也是坏的：%v", err)
	}
	if len(p["f1"]) != 8*300 {
		t.Errorf("去重表条目数=%d want %d", len(p["f1"]), 8*300)
	}
}
