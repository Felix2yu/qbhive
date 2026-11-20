package filemgr

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

// saveCleanState 有 13 处调用点，跨 postprocess 协程、batch、scan 与 Stop，
// 却共用同一个 .tmp 路径。mu 只护内存快照，放开后 A 写到一半被 B 截断，A 再 rename
// 出去的就是半截 JSON；重启时清理进度整份丢失，已清理过的文件被重新处理一遍。
//
// 两个细节决定这条测试能不能真的抓到问题：
//  1. 每次落盘内容必须长度不同（不断追加记录），否则交错写出来的字节仍然一模一样；
//  2. 必须在写的过程中读回校验，损坏是瞬时的，只查最后一次会被后续的完整写覆盖掉。
func TestSaveCleanStateConcurrentAlwaysValidJSON(t *testing.T) {
	m := &Manager{
		cleanState: make(map[string]*cleanRecord),
		statePath:  filepath.Join(t.TempDir(), "filemgr_clean.json"),
	}

	var bad atomic.Int64
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				h := fmt.Sprintf("g%d-%d", g, i)
				m.mu.Lock()
				m.cleanState[h] = &cleanRecord{Done: true, CleanSig: h}
				m.mu.Unlock()
				m.saveCleanState()
				if data, err := os.ReadFile(m.statePath); err == nil {
					var st map[string]*cleanRecord
					if json.Unmarshal(data, &st) != nil {
						bad.Add(1)
					}
				}
			}
		}(g)
	}
	wg.Wait()

	if n := bad.Load(); n > 0 {
		t.Errorf("并发落盘期间读到 %d 次坏 JSON（重启时清理进度整份丢失，已清理文件会被重新处理）", n)
	}
	data, err := os.ReadFile(m.statePath)
	if err != nil {
		t.Fatalf("读回落盘文件：%v", err)
	}
	var st map[string]*cleanRecord
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("最终文件也是坏的：%v", err)
	}
	if len(st) != 8*200 {
		t.Errorf("清理状态条目数=%d want %d", len(st), 8*200)
	}
}