package scheduler

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

// saveFinished 有四类并发调用者（60s ticker、scanCompleted、prefillFinished、Stop），
// 却共用同一个 .tmp 路径。mu 只护内存快照，放开后 A 写到一半被 B 截断，A 再 rename
// 出去的就是半截 JSON；重启时已完成集合整份丢失，老任务被当成新完成重发一遍通知。
//
// 两个细节决定这条测试能不能真的抓到问题：
//  1. 每次落盘内容必须长度不同（不断追加 hash），否则交错写出来的字节仍然一模一样；
//  2. 必须在写的过程中读回校验，损坏是瞬时的，只查最后一次会被后续的完整写覆盖掉。
func TestSaveFinishedConcurrentAlwaysValidJSON(t *testing.T) {
	s := &Scheduler{
		finished:  make(map[string]bool),
		stateFile: filepath.Join(t.TempDir(), "finished.json"),
	}

	var bad atomic.Int64
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				s.mu.Lock()
				s.finished[fmt.Sprintf("g%d-%d", g, i)] = true
				s.mu.Unlock()
				s.saveFinished()
				if data, err := os.ReadFile(s.stateFile); err == nil {
					var list []string
					if json.Unmarshal(data, &list) != nil {
						bad.Add(1)
					}
				}
			}
		}(g)
	}
	wg.Wait()

	if n := bad.Load(); n > 0 {
		t.Errorf("并发落盘期间读到 %d 次坏 JSON（重启时已完成集合整份丢失，老任务会重发通知）", n)
	}
	data, err := os.ReadFile(s.stateFile)
	if err != nil {
		t.Fatalf("读回落盘文件：%v", err)
	}
	var list []string
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatalf("最终文件也是坏的：%v", err)
	}
	if len(list) != 8*200 {
		t.Errorf("已完成集合条目数=%d want %d", len(list), 8*200)
	}
}