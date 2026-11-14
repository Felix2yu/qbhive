package filemgr

import (
	"os"
	"testing"
	"time"

	"github.com/Felix2yu/qbhive/internal/models"
)

// settledMgr 造一个指定开关组合的 Manager，并塞入一条清理记录
func settledMgr(t *testing.T, cleanEnabled, postEnabled bool, rec *cleanRecord) *Manager {
	t.Helper()
	m := setupPostProcess(t, models.PostProcessConfig{Enabled: postEnabled, Command: "echo hi"})
	cfg := m.cfg.Get()
	cfg.FileManager.Enabled = true
	cfg.FileManager.CleanEnabled = cleanEnabled
	m.cfg.Set(cfg)
	if rec != nil {
		m.cleanState["h"] = rec
	}
	return m
}

// 两个 pass 都关掉时不能自锁：Done / PostDone 只在各自的 pass 里置位，
// 无条件要求它们会让退休判定永远不通过。
func TestSettled_BothPassesDisabled(t *testing.T) {
	m := settledMgr(t, false, false, &cleanRecord{})
	if !m.Settled("h", 0) {
		t.Error("清理与转码都关闭时应视为收敛")
	}
}

func TestSettled_ScanDisabled(t *testing.T) {
	m := settledMgr(t, false, false, nil)
	cfg := m.cfg.Get()
	cfg.FileManager.Enabled = false
	m.cfg.Set(cfg)
	if !m.Settled("h", 0) {
		t.Error("扫描未启用时应视为收敛")
	}
}

func TestSettled_CleanOnly(t *testing.T) {
	if m := settledMgr(t, true, false, &cleanRecord{Done: false}); m.Settled("h", 0) {
		t.Error("清理开启但未 Done 时不该收敛")
	}
	if m := settledMgr(t, true, false, &cleanRecord{Done: true}); !m.Settled("h", 0) {
		t.Error("清理开启且已 Done 时应收敛（PostDone 为 false 不该拖累）")
	}
}

func TestSettled_PostOnly(t *testing.T) {
	// 清理关闭：rec.Done 永远是 false，不该因此判不收敛
	rec := &cleanRecord{PostDone: true, PostDispatched: []string{"/a.mov"}, PostFinished: 1}
	if m := settledMgr(t, false, true, rec); !m.Settled("h", 0) {
		t.Error("转码开启且命令已收尾时应收敛（Done 为 false 不该拖累）")
	}
}

// PostDone 只是「派发完」：命令还在跑就不能退休
func TestSettled_WaitsForCommandFinish(t *testing.T) {
	rec := &cleanRecord{PostDone: true, PostDispatched: []string{"/a.mov", "/b.mov"}, PostFinished: 1}
	m := settledMgr(t, false, true, rec)
	if m.Settled("h", 0) {
		t.Error("还有命令在跑时不该收敛")
	}
	rec.PostFinished = 2
	if !m.Settled("h", 0) {
		t.Error("命令全部收尾后应收敛")
	}
}

func TestSettled_QuietPeriod(t *testing.T) {
	rec := &cleanRecord{
		PostDone: true, PostDispatched: []string{"/a.mov"}, PostFinished: 1,
		PostLastDone: time.Now().Unix(),
	}
	m := settledMgr(t, false, true, rec)
	if m.Settled("h", 5*time.Minute) {
		t.Error("刚收尾就退休违反静默期")
	}
	if !m.Settled("h", 0) {
		t.Error("静默期为 0 时应立即收敛")
	}
	rec.PostLastDone = time.Now().Add(-10 * time.Minute).Unix()
	if !m.Settled("h", 5*time.Minute) {
		t.Error("静默期已过应收敛")
	}
}

func TestSettled_FlattenPendingBlocks(t *testing.T) {
	rec := &cleanRecord{Done: true, PostDone: true, FlattenDir: "/data/old"}
	if m := settledMgr(t, true, true, rec); m.Settled("h", 0) {
		t.Error("归档移动未落地时不该收敛")
	}
}

func TestSettled_NoRecordIsNotSettled(t *testing.T) {
	m := settledMgr(t, true, true, nil)
	if m.Settled("h", 0) {
		t.Error("filemgr 还没处理过的任务不该判收敛（刚完成要等清理/转码）")
	}
}

func TestIdle_NoRecordIsIdle(t *testing.T) {
	m := settledMgr(t, true, true, nil)
	if !m.Idle("h") {
		t.Error("没有记录说明 filemgr 从没碰过它，应视为空闲")
	}
}

func TestIdle_BlocksOnInFlightWork(t *testing.T) {
	m := settledMgr(t, false, true, &cleanRecord{
		PostDispatched: []string{"/a.mov", "/b.mov"}, PostFinished: 1,
	})
	if m.Idle("h") {
		t.Error("还有命令在跑时不该判空闲")
	}
	m.cleanState["h"].PostFinished = 2
	if !m.Idle("h") {
		t.Error("命令收尾后应判空闲")
	}
	m.cleanState["h"].FlattenDir = "/data/old"
	if m.Idle("h") {
		t.Error("归档移动未落地时不该判空闲")
	}
}

// 存量状态文件没有 postFinished 键：不追平计数，历史任务一条都退不掉。
// 去掉 loadCleanState 里的归一循环，这条测试就会红。
func TestLoadCleanState_NormalizesLegacyPostFinished(t *testing.T) {
	m := setupPostProcess(t, models.PostProcessConfig{Enabled: true, Command: "echo hi"})
	legacy := `{"h":{"done":true,"postDone":true,"postDispatched":["/a.mov","/b.mov"]}}`
	if err := os.WriteFile(m.statePath, []byte(legacy), 0o644); err != nil {
		t.Fatalf("写入存量状态: %v", err)
	}
	m.loadCleanState()

	rec := m.cleanState["h"]
	if rec == nil {
		t.Fatal("存量记录未加载")
	}
	if rec.PostFinished != 2 {
		t.Fatalf("PostFinished 应追平已派发数 2，got %d", rec.PostFinished)
	}
	cfg := m.cfg.Get()
	cfg.FileManager.Enabled = true
	cfg.FileManager.CleanEnabled = true
	m.cfg.Set(cfg)
	if !m.Settled("h", 5*time.Minute) {
		t.Error("存量任务归一后应能收敛（PostLastDone 为 0 表示不是在跑的命令）")
	}
	if !m.Idle("h") {
		t.Error("存量任务归一后应判空闲")
	}
}

func TestMarkPostFinished(t *testing.T) {
	m := setupPostProcess(t, models.PostProcessConfig{Enabled: true, Command: "echo hi"})
	m.cleanState["h"] = &cleanRecord{PostDispatched: []string{"/a.mov"}}
	before := time.Now().Unix()
	m.markPostFinished("h")

	rec := m.cleanState["h"]
	if rec.PostFinished != 1 {
		t.Errorf("PostFinished=%d want 1", rec.PostFinished)
	}
	if rec.PostLastDone < before {
		t.Errorf("PostLastDone=%d 应不早于 %d", rec.PostLastDone, before)
	}
	// 落盘：重启后计数不能丢
	reloaded := New(m.cfg, m.client)
	if got := reloaded.cleanState["h"]; got == nil || got.PostFinished != 1 {
		t.Errorf("重启后 PostFinished 应为 1，got %+v", got)
	}
}

// 命令失败放弃也算收尾：退休等的是「没有在跑的命令」，不是「命令成功」
func TestRunPostProcess_FailureStillCountsFinished(t *testing.T) {
	// 不动 postRetryDelay：那是包级变量，别的用例的后台 goroutine 还在读它。
	// 用 1s 总预算裁掉退避等待即可
	m := setupPostProcess(t, models.PostProcessConfig{
		Enabled: true, Command: "exit 1", Timeout: 1,
	})
	m.cleanState["h"] = &cleanRecord{PostDispatched: []string{"/tmp/x.mov"}}

	m.runPostProcess(models.QBTorrent{Name: "n", Hash: "h"}, "/tmp/x.mov",
		m.cfg.Get().FileManager.PostProcess)

	if rec := m.cleanState["h"]; rec.PostFinished != 1 {
		t.Errorf("重试耗尽也应记收尾，PostFinished=%d", rec.PostFinished)
	}
}

func TestMatchLists(t *testing.T) {
	cases := []struct {
		name                           string
		category, tags                 string
		catInc, catExc, tagInc, tagExc string
		wantSkip                       bool
	}{
		{"全空不过滤", "电影", "转码", "", "", "", "", false},
		{"只有分隔符等于没配", "电影", "", "，、 ", "", "", "", false},
		{"分类命中包含", "电影", "", "动漫,电影", "", "", "", false},
		{"分类不在包含", "剧集", "", "动漫,电影", "", "", "", true},
		{"未分类不命中包含", "", "", "电影", "", "", "", true},
		{"分类命中排除", "电影", "", "", "电影", "", "", true},
		{"排除优先于包含", "电影", "", "电影", "电影", "", "", true},
		{"标签命中包含", "", "转码,4k", "", "", "4k", "", false},
		{"标签不在包含", "", "转码", "", "", "4k", "", true},
		{"标签命中排除", "", "转码,4k", "", "", "", "4k", true},
		{"无标签不被排除误伤", "", "", "", "", "", "4k", false},
		{"全角逗号分隔", "电影", "", "动漫，电影", "", "", "", false},
		{"顿号分隔", "电影", "", "动漫、电影", "", "", "", false},
		{"换行分隔", "电影", "", "动漫\n电影", "", "", "", false},
		{"大小写不敏感", "Movie", "", "movie", "", "", "", false},
	}
	for _, c := range cases {
		skip, why := MatchLists(c.category, c.tags, c.catInc, c.catExc, c.tagInc, c.tagExc)
		if skip != c.wantSkip {
			t.Errorf("%s: skip=%v want=%v（原因：%s）", c.name, skip, c.wantSkip, why)
		}
		if skip && why == "" {
			t.Errorf("%s: 跳过时必须给出原因", c.name)
		}
		if !skip && why != "" {
			t.Errorf("%s: 未跳过不该带原因，got %q", c.name, why)
		}
	}
}

// 转码过滤必须与退休名单判定同源：委托之后两者结论一致
func TestPostTaskFiltered_DelegatesToMatchLists(t *testing.T) {
	pc := models.PostProcessConfig{CategoryInclude: "动漫,电影", TagExclude: "跳过"}
	for _, tor := range []models.QBTorrent{
		{Category: "电影", Tags: "转码"},
		{Category: "剧集", Tags: ""},
		{Category: "电影", Tags: "跳过"},
		{Category: "", Tags: ""},
	} {
		a, aw := postTaskFiltered(tor, pc)
		b, bw := MatchLists(tor.Category, tor.Tags, pc.CategoryInclude, pc.CategoryExclude, pc.TagInclude, pc.TagExclude)
		if a != b || aw != bw {
			t.Errorf("%+v: 委托后结论不一致 (%v,%q) vs (%v,%q)", tor, a, aw, b, bw)
		}
	}
}
