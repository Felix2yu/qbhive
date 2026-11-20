package stalled

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Felix2yu/qbhive/internal/models"
	qb "github.com/Felix2yu/qbhive/internal/qbittorrent"
)

// schedCalls 记录 mock qB 收到的处置请求，按提交顺序保存 hashes 参数
type schedCalls struct {
	mu     sync.Mutex
	stop   []string
	tags   []string
	bottom []string
}

func (c *schedCalls) snapshot() (stop, tags, bottom []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.stop...), append([]string(nil), c.tags...), append([]string(nil), c.bottom...)
}

func schedSetup(t *testing.T, list []models.QBTorrent, c models.StalledConfig) (*Manager, *schedCalls) {
	t.Helper()
	calls := &schedCalls{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		calls.mu.Lock()
		switch r.URL.Path {
		case "/api/v2/torrents/stop":
			calls.stop = append(calls.stop, r.Form.Get("hashes"))
		case "/api/v2/torrents/addTags":
			calls.tags = append(calls.tags, r.Form.Get("hashes"))
		case "/api/v2/torrents/bottomPrio":
			calls.bottom = append(calls.bottom, r.Form.Get("hashes"))
		}
		calls.mu.Unlock()
		switch r.URL.Path {
		case "/api/v2/auth/login":
			_, _ = w.Write([]byte("Ok."))
		case "/api/v2/torrents/info":
			_ = json.NewEncoder(w).Encode(list)
		default:
			_, _ = w.Write([]byte(""))
		}
	}))
	t.Cleanup(srv.Close)

	cm, dir := testCfg(t, c)
	t.Setenv("QBHIVE_CONFIG", filepath.Join(dir, "config.json"))
	return New(cm, qb.New(srv.URL, "u", "p", "")), calls
}

// schedTorrents 生成 n 条 stalledDL，下标越大 last_activity 越早 → 越卡。
// 另附 n 条 queuedDL 撑开名额闸门。
func schedTorrents(n int, now int64) (stalled, queued []models.QBTorrent) {
	for i := 0; i < n; i++ {
		stalled = append(stalled, models.QBTorrent{
			Hash:         fmt.Sprintf("h%02d", i),
			Name:         fmt.Sprintf("t%02d", i),
			State:        "stalledDL",
			LastActivity: now - int64(3600+i*60),
		})
		queued = append(queued, models.QBTorrent{
			Hash:  fmt.Sprintf("q%02d", i),
			Name:  fmt.Sprintf("queued%02d", i),
			State: "queuedDL",
		})
	}
	return stalled, queued
}

// seedObserved 模拟「已经盯着这些任务看了两小时」，跳过首轮观测封顶
func seedObserved(m *Manager, list []models.QBTorrent, now int64) {
	for _, t := range list {
		m.activeSince[t.Hash] = now - 7200
	}
}

func schedCfg(mode string, capN int) models.StalledConfig {
	return models.StalledConfig{
		Enabled: true, Mode: mode, Interval: 60, IdleMinutes: 30,
		SpeedKiB: 5, MaxPerRound: capN, Tag: "零速回收", NotifyCooldownMinutes: 60,
	}
}

// 刚被 start 的老任务：last_activity 是 3 天前，但这是我们第一次观测到它。
// 不能凭一个无法佐证的旧时间戳就判它「已零速 4320 分钟」并立刻重新暂停。
func TestObservationCap(t *testing.T) {
	now := time.Now().Unix()
	list := []models.QBTorrent{
		{Hash: "old", Name: "old", State: "downloading", LastActivity: now - 3*24*3600},
		{Hash: "q", Name: "q", State: "queuedDL"},
	}
	m, calls := schedSetup(t, list, schedCfg(models.StalledModePause, 20))

	m.ScanNow()
	if stop, _, _ := calls.snapshot(); len(stop) != 0 {
		t.Errorf("首轮观测就动手了：stop=%v", stop)
	}
	if m.activeSince["old"] == 0 {
		t.Fatal("首轮应记录 activeSince")
	}

	// 连续观测满 31 分钟后才允许处置
	m.activeSince["old"] = now - 31*60
	m.ScanNow()
	stop, _, _ := calls.snapshot()
	if len(stop) != 1 || stop[0] != "old" {
		t.Errorf("观测满门槛后应暂停 old，stop=%v", stop)
	}
}

// 没有 queuedDL 在等名额时，暂停/下沉都是空操作，不该动手；notify 档不受影响
func TestQueueGate(t *testing.T) {
	now := time.Now().Unix()
	stalled, _ := schedTorrents(3, now)

	for _, mode := range []string{models.StalledModePause, models.StalledModeDemote} {
		t.Run(mode, func(t *testing.T) {
			m, calls := schedSetup(t, stalled, schedCfg(mode, 20))
			seedObserved(m, stalled, now)
			m.ScanNow()
			stop, tags, bottom := calls.snapshot()
			if len(stop)+len(tags)+len(bottom) != 0 {
				t.Errorf("queuedDL=0 仍动手：stop=%v tags=%v bottom=%v", stop, tags, bottom)
			}
		})
	}

	t.Run("notify", func(t *testing.T) {
		m, _ := schedSetup(t, stalled, schedCfg(models.StalledModeNotify, 20))
		seedObserved(m, stalled, now)
		m.ScanNow()
		if len(m.AuditList(50)) != 3 {
			t.Errorf("notify 档应照常告警 3 条，审计=%d", len(m.AuditList(50)))
		}
	})
}

// 要腾几个名额就处置几条：5 条停滞但只有 2 条在排队，多停的没人接手
func TestQueueRightSize(t *testing.T) {
	now := time.Now().Unix()
	stalled, queued := schedTorrents(5, now)
	list := append(append([]models.QBTorrent{}, stalled...), queued[:2]...)
	m, calls := schedSetup(t, list, schedCfg(models.StalledModePause, 20))
	seedObserved(m, stalled, now)
	m.ScanNow()

	stop, _, _ := calls.snapshot()
	if len(stop) != 1 {
		t.Fatalf("stop 请求数=%d want 1", len(stop))
	}
	if got := len(strings.Split(stop[0], "|")); got != 2 {
		t.Errorf("本轮处置 %d 条，want 2（只有 2 条 queuedDL 在等）", got)
	}
}

// demote 必须按 idle 升序逐条提交：每次 bottomPrio 都把该条挪到队尾，
// 最后提交的才真正沉底。降序提交会让最卡的反而最先被 qB 提回下载。
func TestDemoteOrderAscending(t *testing.T) {
	now := time.Now().Unix()
	stalled, queued := schedTorrents(5, now)
	list := append(append([]models.QBTorrent{}, stalled...), queued...)
	m, calls := schedSetup(t, list, schedCfg(models.StalledModeDemote, 20))
	seedObserved(m, stalled, now)
	m.ScanNow()

	_, tags, bottom := calls.snapshot()
	// 打标批量化（1 次），bottomPrio 刻意逐条（顺序有语义）
	if len(tags) != 1 {
		t.Errorf("addTags 次数=%d want 1（应批量打标）", len(tags))
	}
	want := []string{"h00", "h01", "h02", "h03", "h04"} // h04 最卡，必须最后提交
	if len(bottom) != len(want) {
		t.Fatalf("bottomPrio 次数=%d want %d", len(bottom), len(want))
	}
	for i := range want {
		if bottom[i] != want[i] {
			t.Errorf("提交顺序=%v want %v（最卡的 h04 应落在队尾）", bottom, want)
			break
		}
	}
}

// 配额只计真正告警的条数：冷却中的候选不占名额，
// 否则最卡的一批刚告警过就把配额吃光，从未告警的一条都轮不上。
func TestNotifyQuotaIgnoresCooldown(t *testing.T) {
	now := time.Now().Unix()
	stalled, _ := schedTorrents(40, now)
	m, _ := schedSetup(t, stalled, schedCfg(models.StalledModeNotify, 20))
	seedObserved(m, stalled, now)
	// 最卡的 20 条（h20…h39）刚告警过，正处冷却
	for i := 20; i < 40; i++ {
		m.lastNotify[stalled[i].Hash] = now - 60
	}
	m.ScanNow()

	entries := m.AuditList(100)
	if len(entries) != 20 {
		t.Fatalf("本轮告警=%d want 20（冷却的 20 条不该占配额）", len(entries))
	}
	for _, e := range entries {
		if e.Hash >= "h20" {
			t.Errorf("冷却中的 %s 被告警了", e.Hash)
		}
	}
}

// 20 条候选应压成 2 次 HTTP（批量打标 + 批量暂停），而不是 40 次串行请求
func TestPauseBatchRequests(t *testing.T) {
	now := time.Now().Unix()
	stalled, queued := schedTorrents(20, now)
	list := append(append([]models.QBTorrent{}, stalled...), queued...)
	m, calls := schedSetup(t, list, schedCfg(models.StalledModePause, 20))
	seedObserved(m, stalled, now)
	m.ScanNow()

	stop, tags, _ := calls.snapshot()
	if len(tags) != 1 || len(stop) != 1 {
		t.Fatalf("HTTP 次数：addTags=%d stop=%d，want 各 1 次", len(tags), len(stop))
	}
	if got := len(strings.Split(stop[0], "|")); got != 20 {
		t.Errorf("批量暂停只带上 %d 条，want 20", got)
	}
	if got := len(strings.Split(tags[0], "|")); got != 20 {
		t.Errorf("批量打标只带上 %d 条，want 20", got)
	}
	if len(m.AuditList(100)) != 20 {
		t.Errorf("审计条数=%d want 20", len(m.AuditList(100)))
	}
}

// 超限截断要发告警让人看见，且受冷却约束不每轮刷屏
func TestOverCapNotify(t *testing.T) {
	now := time.Now().Unix()
	stalled, queued := schedTorrents(30, now)
	list := append(append([]models.QBTorrent{}, stalled...), queued...)
	m, calls := schedSetup(t, list, schedCfg(models.StalledModePause, 20))
	seedObserved(m, stalled, now)

	var mu sync.Mutex
	var titles []string
	m.SetNotifier(func(title, body string) {
		mu.Lock()
		titles = append(titles, title)
		mu.Unlock()
	})

	m.ScanNow()
	stop, _, _ := calls.snapshot()
	if len(stop) != 1 || len(strings.Split(stop[0], "|")) != 20 {
		t.Fatalf("本轮应处置 20 条，stop=%v", stop)
	}
	mu.Lock()
	got := append([]string(nil), titles...)
	mu.Unlock()
	if len(got) != 1 || !strings.Contains(got[0], "超限") {
		t.Fatalf("超限告警=%v want 恰好 1 条含「超限」", got)
	}

	// 处置过的任务时钟会被清空，重新喂一遍观测起点，让第二轮候选依旧超限，
	// 这样「没有第二条告警」才真的是冷却压下来的，而不是候选不够多。
	seedObserved(m, stalled, now)
	m.ScanNow()
	stop2, _, _ := calls.snapshot()
	if len(stop2) != 2 || len(strings.Split(stop2[1], "|")) != 20 {
		t.Fatalf("第二轮应仍截断处置 20 条，stop=%v", stop2)
	}
	mu.Lock()
	after := len(titles)
	mu.Unlock()
	if after != 1 {
		t.Errorf("冷却期内重复告警，titles=%d want 1", after)
	}

	// 冷却过期后同样超限 → 必须再告警一次，否则阈值配错就永远看不见了
	seedObserved(m, stalled, now)
	m.lastBurst = now - 7200
	m.ScanNow()
	mu.Lock()
	expired := append([]string(nil), titles...)
	mu.Unlock()
	if len(expired) != 2 || !strings.Contains(expired[1], "超限") {
		t.Errorf("冷却过期后未再告警，titles=%v", expired)
	}
}
