package stalled

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Felix2yu/qbhive/internal/config"
	"github.com/Felix2yu/qbhive/internal/models"
	qb "github.com/Felix2yu/qbhive/internal/qbittorrent"
)

func testCfg(t *testing.T, c models.StalledConfig) (*config.Manager, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	data, _ := os.ReadFile(path)
	_ = data
	cm := config.New(path)
	full := cm.Get()
	full.Stalled = c
	cm.Set(full)
	return cm, dir
}

func mkTorrent(state string, dl, up, lastActivity int64) models.QBTorrent {
	return models.QBTorrent{
		Hash:          "h-" + state,
		Name:          "任务-" + state,
		State:         state,
		DownloadSpeed: dl,
		UploadSpeed:   up,
		LastActivity:  lastActivity,
		AddedOn:       lastActivity - 3600,
	}
}

// ---- 状态与速度判定 ----

func TestEvalState_FiltersStates(t *testing.T) {
	thr := int64(5 * 1024)
	now := int64(1_000_000)
	cases := []struct {
		state    string
		wantZero bool
	}{
		{"stalledDL", true},
		{"downloading", true},
		{"metaDL", true},
		{"forcedDL", false}, // 强制态绝不回收
		{"forcedMetaDL", false},
		{"checkingDL", false}, // 校验中的假性零速
		{"queuedDL", false},   // 排队中，没占名额
		{"stalledUP", false},  // 做种不碰
		{"uploading", false},
		{"stoppedDL", false},
		{"error", false},
	}
	for _, c := range cases {
		tt := mkTorrent(c.state, 0, 0, now-100)
		zero, _, _ := evalState(tt, thr, 0, now)
		if zero != c.wantZero {
			t.Errorf("state=%s zero=%v want=%v", c.state, zero, c.wantZero)
		}
	}
}

func TestEvalState_SpeedThreshold(t *testing.T) {
	now := int64(1_000_000)
	thr := int64(5 * 1024) // 5120 B/s
	cases := []struct {
		dl, up int64
		want   bool
	}{
		{0, 0, true},
		{100, 100, true},
		{5120, 5120, true}, // 等于阈值算命中
		{5121, 0, false},   // 超阈值
		{0, 6000, false},
	}
	for _, c := range cases {
		tt := mkTorrent("stalledDL", c.dl, c.up, now-100)
		zero, _, _ := evalState(tt, thr, 0, now)
		if zero != c.want {
			t.Errorf("dl=%d up=%d zero=%v want=%v", c.dl, c.up, zero, c.want)
		}
	}
}

// 双证据：qB 的 last_activity 与自己的观测起点，任一满足时长即可
func TestEvalState_DualEvidenceIdle(t *testing.T) {
	now := int64(1_000_000)
	thr := int64(0) // 严格零速
	// 只有 qB 证据（无 self 观测）：last_activity 很久以前
	tt := mkTorrent("stalledDL", 0, 0, now-3600)
	zero, idle, _ := evalState(tt, thr, 0, now)
	if !zero || idle != 3600 {
		t.Errorf("qb-only: zero=%v idle=%d want true/3600", zero, idle)
	}
	// 只有 self 证据（last_activity 被心跳刷成刚刚），self 起点很久以前
	tt2 := mkTorrent("stalledDL", 0, 0, now-5)
	zero, idle, _ = evalState(tt2, thr, now-7200, now)
	if !zero || idle != 7200 {
		t.Errorf("self-only: zero=%v idle=%d want true/7200", zero, idle)
	}
	// 两个证据都近：时长很短
	tt3 := mkTorrent("stalledDL", 0, 0, now-30)
	zero, idle, _ = evalState(tt3, thr, now-20, now)
	if !zero || idle != 30 {
		t.Errorf("both-recent: zero=%v idle=%d want true/30", zero, idle)
	}
}

func TestSpeedThreshold(t *testing.T) {
	if got := speedThreshold(models.StalledConfig{SpeedKiB: 5}); got != 5120 {
		t.Errorf("speedThreshold(5)=%d want 5120", got)
	}
	if got := speedThreshold(models.StalledConfig{SpeedKiB: 0}); got != 0 {
		t.Errorf("speedThreshold(0)=%d want 0", got)
	}
	if got := speedThreshold(models.StalledConfig{SpeedKiB: -3}); got != 5*1024 {
		t.Errorf("speedThreshold(-3)=%d want 5120 (default)", got)
	}
}

// ---- 保护规则 ----

func TestProtected(t *testing.T) {
	base := models.StalledConfig{ExcludeTag: "保活"}
	cases := []struct {
		state string
		tags  string
		want  bool
	}{
		{"stalledDL", "", false},
		{"forcedDL", "", true}, // 强制态
		{"forcedMetaDL", "", true},
		{"stalledDL", "保活", true},           // 命中保护标签
		{"stalledDL", "其他,保活,xx", true},   // 多标签命中
		{"stalledDL", "其他", false},
	}
	for _, c := range cases {
		tt := mkTorrent(c.state, 0, 0, 0)
		tt.Tags = c.tags
		if got := protected(tt, base); got != c.want {
			t.Errorf("state=%s tags=%q protected=%v want=%v", c.state, c.tags, got, c.want)
		}
	}
}

// ---- collectCandidates：首次观测计时 + 门槛 + 排序 + 状态清理 ----

func TestCollectCandidates(t *testing.T) {
	cm, _ := testCfg(t, models.StalledConfig{Enabled: true, Mode: models.StalledModePause, IdleMinutes: 30, SpeedKiB: 5})
	dir := filepath.Join(t.TempDir(), "state")
	_ = os.MkdirAll(dir, 0o755)
	m := &Manager{
		cfg:         cm,
		firstZero:   make(map[string]int64),
		activeSince: make(map[string]int64),
		lastNotify:  make(map[string]int64),
		statePath:   filepath.Join(dir, "state.json"),
		auditPath:   filepath.Join(dir, "audit.jsonl"),
	}
	now := int64(1_000_000)
	idleSec := int64(30 * 60)

	old := mkTorrent("stalledDL", 0, 0, now-int64(40*60)) // 零速 40 分钟，达标
	old.Hash = "old"
	fresh := mkTorrent("downloading", 0, 0, now-10) // 刚零速，未达标
	fresh.Hash = "fresh"
	forced := mkTorrent("forcedDL", 0, 0, now-int64(50*60)) // 强制态受保护
	forced.Hash = "forced"

	// 已经盯着这三条看了 50 分钟，零速时长才允许采信到 40 分钟前。
	// 首轮观测（activeSince=now）会把时长封顶到 0，见 TestObservationCap。
	for _, h := range []string{"old", "fresh", "forced"} {
		m.activeSince[h] = now - 50*60
	}

	list := []models.QBTorrent{old, fresh, forced}
	cands, waiting := m.collectCandidates(list, cm.Get().Stalled, now)
	if len(cands) != 1 || cands[0].t.Hash != "old" {
		t.Errorf("cands=%+v want only 'old'", cands)
	}
	if waiting != 1 {
		t.Errorf("waiting=%d want 1 (fresh)", waiting)
	}
	// fresh 的观测起点应被记录
	if m.firstZero["fresh"] != now {
		t.Errorf("firstZero[fresh]=%d want %d", m.firstZero["fresh"], now)
	}
	_ = idleSec

	// 第二轮：时间过去 31 分钟，fresh 现在靠 self 证据达标
	now2 := now + 31*60
	cands2, _ := m.collectCandidates(list, cm.Get().Stalled, now2)
	hit := map[string]bool{}
	for _, c := range cands2 {
		hit[c.t.Hash] = true
	}
	if !hit["fresh"] {
		t.Errorf("fresh should pass on second round via self-evidence, cands=%+v", cands2)
	}
}

// ---- 状态持久化往返 ----

func TestSaveLoadState(t *testing.T) {
	cm, dir := testCfg(t, models.StalledConfig{Enabled: true, Mode: models.StalledModePause})
	_ = dir
	m := &Manager{
		cfg:         cm,
		firstZero:   map[string]int64{"a": 100, "b": 200},
		activeSince: map[string]int64{"a": 50},
		lastNotify:  map[string]int64{"a": 300},
		statePath:   filepath.Join(t.TempDir(), "state.json"),
		auditPath:   filepath.Join(t.TempDir(), "audit.jsonl"),
	}
	m.save()
	m.firstZero = make(map[string]int64)
	m.activeSince = make(map[string]int64)
	m.lastNotify = make(map[string]int64)
	m.load()
	if m.firstZero["a"] != 100 || m.firstZero["b"] != 200 {
		t.Errorf("firstZero after load=%+v", m.firstZero)
	}
	if m.activeSince["a"] != 50 {
		t.Errorf("activeSince after load=%+v", m.activeSince)
	}
	if m.lastNotify["a"] != 300 {
		t.Errorf("lastNotify after load=%+v", m.lastNotify)
	}
}

// save 会被 ticker 的 ScanNow、API 触发的 ScanNow、Stop 并发调用，共用同一个 .tmp 路径。
// mu 只护内存快照，放开后 A 写到一半被 B 截断，A 再 rename 出去的就是半截 JSON，
// 重启时观测时钟与审计整份丢失。内容必须递变、且要在写的过程中读回，否则抓不到瞬时损坏。
func TestSaveConcurrentAlwaysValidJSON(t *testing.T) {
	cm, dir := testCfg(t, models.StalledConfig{Enabled: true, Mode: models.StalledModePause})
	m := &Manager{
		cfg:         cm,
		firstZero:   make(map[string]int64),
		activeSince: make(map[string]int64),
		lastNotify:  make(map[string]int64),
		statePath:   filepath.Join(dir, "state.json"),
		auditPath:   filepath.Join(dir, "audit.jsonl"),
	}

	var bad atomic.Int64
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				h := fmt.Sprintf("g%d-%d", g, i)
				m.mu.Lock()
				m.firstZero[h] = int64(i)
				m.mu.Unlock()
				m.addAudit(auditEntry{TS: int64(i), Hash: h, Name: h, Action: "paused", OK: true})
				m.save()

				if data, err := os.ReadFile(m.statePath); err == nil {
					var p persisted
					if json.Unmarshal(data, &p) != nil {
						bad.Add(1)
					}
				}
				if data, err := os.ReadFile(m.auditPath); err == nil {
					for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
						if line == "" {
							continue
						}
						var e auditEntry
						if json.Unmarshal([]byte(line), &e) != nil {
							bad.Add(1)
							break
						}
					}
				}
			}
		}(g)
	}
	wg.Wait()

	if n := bad.Load(); n > 0 {
		t.Errorf("并发落盘期间读到 %d 次坏 JSON（重启时观测时钟与审计会整份丢失）", n)
	}
}

// ---- qB 客户端 AddTags 走 addTags 端点、不调用 removeTags ----

func TestClientAddTags(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.WriteHeader(200)
	}))
	defer srv.Close()
	cli := qb.New(srv.URL, "u", "p", "")
	if err := cli.AddTags([]string{"h1", "h2"}, []string{"零速回收"}); err != nil {
		t.Fatalf("AddTags: %v", err)
	}
	// 首次调用会先登录，再打标签；关键是绝不调用 removeTags（那会抹掉用户标签）
	var addCount, removeCount int
	for _, p := range paths {
		if p == "/api/v2/torrents/addTags" {
			addCount++
		}
		if p == "/api/v2/torrents/removeTags" {
			removeCount++
		}
	}
	if addCount != 1 || removeCount != 0 {
		t.Errorf("paths=%v want exactly 1 addTags and 0 removeTags", paths)
	}
}

// ---- qB 客户端 BottomPrio 走 bottomPrio 端点（下沉档位的动作基础）----

func TestClientBottomPrio(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.WriteHeader(200)
	}))
	defer srv.Close()
	cli := qb.New(srv.URL, "u", "p", "")
	if err := cli.BottomPrio("h1", "h2"); err != nil {
		t.Fatalf("BottomPrio: %v", err)
	}
	var bottomCount int
	for _, p := range paths {
		if p == "/api/v2/torrents/bottomPrio" {
			bottomCount++
		}
	}
	if bottomCount != 1 {
		t.Errorf("paths=%v want exactly 1 bottomPrio", paths)
	}
}

// ---- 集成：demote 档位命中后确实调用 bottomPrio 并落审计 ----

func TestDemoteRound(t *testing.T) {
	var mu sync.Mutex
	var bottomHashes string
	var auditActions []string

	now := time.Now().Unix()
	stalled := mkTorrent("stalledDL", 0, 0, now-7200) // 零速已 2 小时，超过 30 分钟门槛
	// 名额闸门：必须真有任务在排队等名额，下沉才有收益
	queued := mkTorrent("queuedDL", 0, 0, now-60)
	queued.Hash = "h-queued"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v2/auth/login":
			w.Write([]byte("Ok."))
		case "/api/v2/torrents/info":
			json.NewEncoder(w).Encode([]models.QBTorrent{stalled, queued})
		case "/api/v2/torrents/bottomPrio":
			r.ParseForm()
			mu.Lock()
			bottomHashes = r.Form.Get("hashes")
			mu.Unlock()
			w.Write([]byte(""))
		case "/api/v2/torrents/addTags":
			w.Write([]byte(""))
		default:
			t.Logf("未预期端点: %s", r.URL.Path)
			w.WriteHeader(200)
		}
	}))
	defer srv.Close()

	cm, dir := testCfg(t, models.StalledConfig{
		Enabled: true, Mode: models.StalledModeDemote, IdleMinutes: 30,
		SpeedKiB: 5, MaxPerRound: 10, Tag: "零速回收",
	})
	t.Setenv("QBHIVE_CONFIG", filepath.Join(dir, "config.json"))

	cli := qb.New(srv.URL, "u", "p", "")
	m := New(cm, cli)
	// 已经盯着它看了 2 小时。首轮观测会把零速时长封顶到 0，见 TestObservationCap。
	m.activeSince[stalled.Hash] = now - 7200
	m.ScanNow()

	mu.Lock()
	got := bottomHashes
	mu.Unlock()
	if got != stalled.Hash {
		t.Errorf("bottomPrio hashes=%q want %q", got, stalled.Hash)
	}
	for _, e := range m.AuditList(50) {
		auditActions = append(auditActions, e.Action)
	}
	found := false
	for _, a := range auditActions {
		if a == "demoted" {
			found = true
		}
	}
	if !found {
		t.Errorf("审计中未找到 demoted 记录: %v", auditActions)
	}
}
