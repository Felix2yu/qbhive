package logger

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// resetRing 把全局缓冲换成空的一份，避免各用例互相看到对方的日志。
func resetRing() {
	global = &ring{buf: make([]Entry, 0, RingSize)}
}

// log 包 LstdFlags 拼出的时间头：年/月/日 时:分:秒 + 一个空格
func stamp() string { return time.Now().Format("2006/01/02 15:04:05 ") }

func TestCaptureStripsPrefixAndStamp(t *testing.T) {
	resetRing()
	c := capture{level: "info", head: "[INFO]  "}
	if _, err := c.Write([]byte("[INFO]  " + stamp() + "代理：已装配\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	entries, cursor := Query(0, 0)
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	if entries[0].Level != "info" {
		t.Errorf("level = %q, want info", entries[0].Level)
	}
	if entries[0].Msg != "代理：已装配" {
		t.Errorf("级别前缀和时间头没剥干净: %q", entries[0].Msg)
	}
	if cursor != 1 || entries[0].Seq != 1 {
		t.Errorf("cursor=%d seq=%d, want 1/1", cursor, entries[0].Seq)
	}
	if entries[0].TS == 0 {
		t.Error("TS 未填写")
	}
}

// 正文里出现 "[WARN]" 这类字样时不能被误剥（例如日志内容本身在转述一行日志）
func TestCaptureKeepsBodyVerbatim(t *testing.T) {
	resetRing()
	c := capture{level: "warn", head: "[WARN]  "}
	body := "[WARN]  " + stamp() + "上一行日志是 " + stamp() + "正文开头"
	if _, err := c.Write([]byte(body)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	entries, _ := Query(0, 0)
	want := "上一行日志是 " + stamp() + "正文开头"
	if entries[0].Msg != want {
		t.Errorf("msg = %q, want %q", entries[0].Msg, want)
	}
}

func TestRingEvictsOldest(t *testing.T) {
	resetRing()
	total := RingSize + 5
	for i := 1; i <= total; i++ {
		global.add("info", fmt.Sprintf("m%d", i))
	}
	entries, cursor := Query(0, RingSize)
	if len(entries) != RingSize {
		t.Fatalf("缓冲应固定在 %d 条，实际 %d", RingSize, len(entries))
	}
	// 最前面的 5 条已被淘汰，剩下的仍是旧→新
	if entries[0].Msg != "m6" || entries[len(entries)-1].Msg != fmt.Sprintf("m%d", total) {
		t.Errorf("首尾 = %q / %q, want m6 / m%d", entries[0].Msg, entries[len(entries)-1].Msg, total)
	}
	for i := 1; i < len(entries); i++ {
		if entries[i].Seq <= entries[i-1].Seq {
			t.Fatalf("第 %d 条 Seq=%d 不大于前一条 %d，环形读序乱了", i, entries[i].Seq, entries[i-1].Seq)
		}
	}
	if cursor != uint64(total) {
		t.Errorf("cursor = %d, want %d", cursor, total)
	}
}

func TestQueryIncremental(t *testing.T) {
	resetRing()
	for i := 0; i < 3; i++ {
		global.add("info", fmt.Sprintf("m%d", i))
	}
	entries, cursor := Query(0, 0)
	if len(entries) != 3 {
		t.Fatalf("首拉 %d 条, want 3", len(entries))
	}

	// 没有新日志时增量为空，cursor 原样返回，前端下一轮还用它
	if entries, again := Query(cursor, 10); len(entries) != 0 || again != cursor {
		t.Errorf("无新日志时得到 %d 条 / cursor=%d, want 0 条 / %d", len(entries), again, cursor)
	}

	global.add("warn", "新的一条")
	entries, cursor = Query(cursor, 10)
	if len(entries) != 1 || entries[0].Msg != "新的一条" || entries[0].Level != "warn" {
		t.Fatalf("增量拉取 = %+v, want 只剩新的一条", entries)
	}
	if cursor != 4 {
		t.Errorf("cursor = %d, want 4", cursor)
	}
}

func TestQueryLimitKeepsNewest(t *testing.T) {
	resetRing()
	for i := 1; i <= 10; i++ {
		global.add("info", fmt.Sprintf("m%02d", i))
	}
	entries, cursor := Query(0, 3)
	if len(entries) != 3 {
		t.Fatalf("limit=3 实际返回 %d 条", len(entries))
	}
	if entries[0].Msg != "m08" || entries[2].Msg != "m10" {
		t.Errorf("应保留最近的 3 条，实际 %q..%q", entries[0].Msg, entries[2].Msg)
	}
	if cursor != 10 {
		t.Errorf("cursor = %d, want 10", cursor)
	}
}

// limit 非法（0 / 负数 / 超过 RingSize）时退化成整个缓冲，不能让接口拿不到数据
func TestQueryLimitFallbacks(t *testing.T) {
	resetRing()
	for i := 0; i < 5; i++ {
		global.add("info", "m")
	}
	for _, limit := range []int{0, -1, RingSize + 100} {
		if entries, _ := Query(0, limit); len(entries) != 5 {
			t.Errorf("limit=%d 返回 %d 条, want 5", limit, len(entries))
		}
	}
}

func TestRingConcurrentWriters(t *testing.T) {
	resetRing()
	const writers, perWriter = 8, 50
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				global.add("info", fmt.Sprintf("w%d-%d", w, i))
			}
		}(w)
	}
	wg.Wait()

	entries, cursor := Query(0, RingSize)
	want := writers * perWriter
	if cursor != uint64(want) {
		t.Errorf("cursor = %d, want %d（有写入丢了）", cursor, want)
	}
	if len(entries) != want {
		t.Fatalf("保留 %d 条, want %d", len(entries), want)
	}
	for i := 1; i < len(entries); i++ {
		if entries[i].Seq != entries[i-1].Seq+1 {
			t.Fatalf("Seq 不连续：%d 后是 %d", entries[i-1].Seq, entries[i].Seq)
		}
	}
}

// 四把 logger 都要既照旧打到 stdout/stderr，又进环形缓冲
func TestLoggersFeedRing(t *testing.T) {
	resetRing()
	Info.Printf("hello %d", 1)
	Warn.Print("careful")
	Error.Print("boom")
	Debug.Print("peek")

	entries, _ := Query(0, 0)
	if len(entries) != 4 {
		t.Fatalf("got %d entries, want 4", len(entries))
	}
	want := map[string]string{"info": "hello 1", "warn": "careful", "error": "boom", "debug": "peek"}
	for _, e := range entries {
		if want[e.Level] != e.Msg {
			t.Errorf("%q 里混进了前缀或时间头: level=%q msg=%q", e.Level, e.Level, e.Msg)
		}
		delete(want, e.Level)
	}
	if len(want) != 0 {
		t.Errorf("这些级别没进缓冲: %v", want)
	}
}
