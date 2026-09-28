package scheduler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Felix2yu/qbhive/internal/config"
	"github.com/Felix2yu/qbhive/internal/filemgr"
	"github.com/Felix2yu/qbhive/internal/limiter"
	"github.com/Felix2yu/qbhive/internal/models"
	"github.com/Felix2yu/qbhive/internal/notifier"
	qb "github.com/Felix2yu/qbhive/internal/qbittorrent"
	"github.com/Felix2yu/qbhive/internal/rss"
)

// ============ 本地 mock：qBittorrent WebUI + Apprise 接收端 ============

// qbMock 是可变的 mock qB 状态。
// HTTP handler 跑在 server goroutine 里，测试 goroutine 里读，
// 所以所有字段都必须经过 mu。
type qbMock struct {
	mu       sync.Mutex
	torrents string         // /api/v2/torrents/info 返回的 JSON
	fail     bool           // true 时 info 接口返回 500（模拟 qB 拉取失败）
	hits     map[string]int // 关键字 → 命中次数：info/completedInfo/limit/stop/rename/start
}

func newQBMock(torrentsJSON string) *qbMock {
	return &qbMock{torrents: torrentsJSON, hits: make(map[string]int)}
}

func (m *qbMock) setTorrents(js string) {
	m.mu.Lock()
	m.torrents = js
	m.mu.Unlock()
}

func (m *qbMock) setFail(v bool) {
	m.mu.Lock()
	m.fail = v
	m.mu.Unlock()
}

func (m *qbMock) count(key string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hits[key]
}

// newQBServer 起一个模拟 qBittorrent WebUI 的 httptest server（登录 + 常用写接口）
func newQBServer(t *testing.T, m *qbMock) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "SID=test")
		_, _ = w.Write([]byte("Ok."))
	})
	mux.HandleFunc("/api/v2/torrents/info", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.hits["info"]++
		if r.URL.Query().Get("filter") == "completed" {
			m.hits["completedInfo"]++
		}
		fail, js := m.fail, m.torrents
		m.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(js))
	})
	mux.HandleFunc("/api/v2/torrents/setUploadLimit", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.hits["limit"]++
		m.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	// qBittorrent 5.x：4.x 的 pause/resume 已移除，改为 stop/start
	mux.HandleFunc("/api/v2/torrents/stop", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.hits["stop"]++
		m.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/v2/torrents/start", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.hits["start"]++
		m.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/v2/torrents/renameFile", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.hits["rename"]++
		m.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// notifyRecorder 是本地假 Apprise 接收端（json:// URL 会 POST 到这里）
type notifyRecorder struct {
	mu     sync.Mutex
	titles []string
	status int
}

func newNotifySink(t *testing.T, status int) (*httptest.Server, *notifyRecorder) {
	t.Helper()
	rec := &notifyRecorder{status: status}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Title   string `json:"title"`
			Message string `json:"message"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		rec.mu.Lock()
		rec.titles = append(rec.titles, payload.Title)
		rec.mu.Unlock()
		w.WriteHeader(rec.status)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, rec
}

func appriseURL(srv *httptest.Server) string {
	return "json://" + strings.TrimPrefix(srv.URL, "http://") + "/notify"
}

func (r *notifyRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.titles)
}

func (r *notifyRecorder) last() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.titles) == 0 {
		return ""
	}
	return r.titles[len(r.titles)-1]
}

// ============ 完整 Scheduler 组装 ============

type fullOpts struct {
	torrents     string // qB 返回的任务列表 JSON
	notifier     models.NotifierConfig
	limiterOn    bool // 是否启用限速器
	limiterRules []models.LimitRule
	filemgrOn    bool   // 是否启用文件管理
	rssOn        bool   // 是否启用 RSS
	feedURL      string // RSS 订阅源地址（本地 httptest）
}

// fullEnv 是一个各子系统齐全、qB/通知/RSS 全部指向本地 mock 的 Scheduler
type fullEnv struct {
	s     *Scheduler
	cfg   *config.Manager
	qbM   *qbMock
	qbSrv *httptest.Server
	dir   string
}

// setupFull 组装完整 Scheduler：状态文件（finished.json / rss_seen.json）
// 通过 QBHIVE_CONFIG 落到临时目录，不会污染仓库。
func setupFull(t *testing.T, o fullOpts) *fullEnv {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	t.Setenv("QBHIVE_CONFIG", cfgPath)

	qbM := newQBMock(o.torrents)
	qbSrv := newQBServer(t, qbM)

	cfg := config.New(cfgPath)
	cfg.Set(models.AppConfig{
		Notifier: o.notifier,
		Limiter: models.LimiterConfig{
			Enabled: o.limiterOn, Interval: 1, Rules: o.limiterRules,
		},
		FileManager: models.FileManagerConfig{Enabled: o.filemgrOn, ScanInterval: 1},
		RSS: models.RSSConfig{
			Enabled: o.rssOn, Interval: 1,
			Feeds: []models.RSSFeed{{ID: "f1", Name: "feed", URL: o.feedURL, Enabled: o.rssOn}},
		},
	})

	// 用 API Key 模式：qb.Client 的 cookie 字段没有加锁，多个 goroutine
	// 共用同一个 client 时并发首次登录会在 -race 下撞车（源码缺陷，本次不可改）。
	// API Key 路径完全不碰 cookie，可避开该并发写。
	client := qb.New(qbSrv.URL, "u", "p", "qbt_test_key")
	nt := notifier.New(o.notifier)
	lim := limiter.New(cfg, client)
	fm := filemgr.New(cfg, client)
	eng := rss.New(cfg, client)

	return &fullEnv{
		s:     New(cfg, client, nt, lim, fm, eng),
		cfg:   cfg,
		qbM:   qbM,
		qbSrv: qbSrv,
		dir:   dir,
	}
}

// mkTorrents 用 models.QBTorrent 的 JSON tag 直接拼任务列表
func mkTorrents(ts ...models.QBTorrent) string {
	b, err := json.Marshal(ts)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// doneTorrent 构造一条"刚完成"的任务
func doneTorrent(hash, name string, completedOn int64) models.QBTorrent {
	return models.QBTorrent{
		Hash: hash, Name: name, State: "uploading", Progress: 1,
		Size: 1 << 30, AddedOn: completedOn - 3600, CompletedOn: completedOn,
		Category: "movies", SavePath: "/downloads",
	}
}

// waitUntil 轮询等待条件成立（带超时，避免固定长 sleep）
func waitUntil(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待超时（%s）：%s", timeout, desc)
}

// ---- Start/Stop 生命周期观察工具 ----

// goroutineStacks 抓取当前进程所有 goroutine 的栈（不够大就自动扩容）
func goroutineStacks() string {
	buf := make([]byte, 4<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return string(buf[:n])
		}
		buf = make([]byte, 2*len(buf))
	}
}

// schedulerTickerSyms 是 Start 拉起的常驻 ticker goroutine 符号：
// func1 = 完成通知扫描，func2 = finished 持久化；后三个分别是限速/文件管理/RSS 的轮询
var schedulerTickerSyms = []string{
	"scheduler.(*Scheduler).Start.func",
	"limiter.(*Limiter).runTicker.func",
	"filemgr.(*Manager).runTicker.func",
	"rss.(*Engine).runPoller.func",
}

// missingGoroutines 返回在当前 goroutine 栈里找不到的符号
func missingGoroutines(syms []string) []string {
	stack := goroutineStacks()
	var missing []string
	for _, sym := range syms {
		if !strings.Contains(stack, sym) {
			missing = append(missing, sym)
		}
	}
	return missing
}

// waitNoGoroutines 轮询等待指定符号的 goroutine 全部退出（带超时，不做固定长 sleep）
func waitNoGoroutines(t *testing.T, timeout time.Duration, desc string, syms []string) {
	t.Helper()
	waitUntil(t, timeout, desc, func() bool { return len(missingGoroutines(syms)) == len(syms) })
}

// ============ scanCompleted ============

// TestScanCompleted_NewCompletionNotifies 新完成（progress=1、完成态）的任务：
// 发出通知 + 写入 finished 集合 + 落盘状态文件；
// "已通知的任务不再重复通知"的语义由 TestScanCompleted_PrefilledNoRenotify 覆盖。
func TestScanCompleted_NewCompletionNotifies(t *testing.T) {
	sink, rec := newNotifySink(t, http.StatusOK)
	env := setupFull(t, fullOpts{
		torrents: mkTorrents(doneTorrent("hash-aaaa-1111", "My.Movie.2160p.mkv", 1700000000)),
		notifier: models.NotifierConfig{Enabled: true, AppriseURLs: []string{appriseURL(sink)}},
	})

	env.s.scanCompleted()

	if rec.count() != 1 {
		t.Fatalf("首次扫描应发 1 条通知，got %d", rec.count())
	}
	if title := rec.last(); title != "✅ 下载完成 · My.Movie.2160p.mkv" {
		t.Errorf("通知标题不符，got %q", title)
	}
	if !env.s.finished["hash-aaaa-1111"] {
		t.Errorf("新完成任务应写入 finished 集合，got %v", env.s.finished)
	}
	// saveFinished 必须在释放 s.mu 之后调用，否则会自锁；这里断言状态文件已落盘
	if _, err := os.Stat(env.s.stateFile); err != nil {
		t.Errorf("扫描后应写入状态文件：%v", err)
	}

	// 二次扫描不重复通知
	env.s.scanCompleted()
	if rec.count() != 1 {
		t.Errorf("二次扫描不应重复通知，got %d 条", rec.count())
	}
}

// TestScanCompleted_NoNewDoneDoesNotPersist 本轮没有新完成任务时不调用 saveFinished
// （状态文件不产生）
func TestScanCompleted_NoNewDoneDoesNotPersist(t *testing.T) {
	sink, rec := newNotifySink(t, http.StatusOK)
	env := setupFull(t, fullOpts{
		torrents: mkTorrents(models.QBTorrent{
			Hash: "hash-active-1", Name: "Downloading.mkv",
			State: "downloading", Progress: 0.5,
		}),
		notifier: models.NotifierConfig{Enabled: true, AppriseURLs: []string{appriseURL(sink)}},
	})

	env.s.scanCompleted()

	if rec.count() != 0 {
		t.Errorf("未完成任务不应发通知，got %d 条", rec.count())
	}
	if _, err := os.Stat(env.s.stateFile); err == nil {
		t.Error("没有新完成任务时不应写状态文件")
	}
}

// TestScanCompleted_PrefilledNoRenotify 已在 finished 集合里的任务
// （prefillFinished 预填充 / 上一轮通知过）再次扫描不重复通知
func TestScanCompleted_PrefilledNoRenotify(t *testing.T) {
	sink, rec := newNotifySink(t, http.StatusOK)
	env := setupFull(t, fullOpts{
		torrents: mkTorrents(doneTorrent("hash-prefill-01", "Sequel.mkv", 1700000000)),
		notifier: models.NotifierConfig{Enabled: true, AppriseURLs: []string{appriseURL(sink)}},
	})

	// 与生产流程一致：Start 后先做一次历史预填充
	env.s.prefillFinished()
	if !env.s.finished["hash-prefill-01"] {
		t.Fatalf("预填充后应标记为已通知，got %v", env.s.finished)
	}

	env.s.scanCompleted()

	if rec.count() != 0 {
		t.Errorf("已通知过的任务不应重复通知，got %d 条", rec.count())
	}
	if !env.s.finished["hash-prefill-01"] {
		t.Error("预填充的标记不应被扫描清掉")
	}
	if _, err := os.Stat(env.s.stateFile); err != nil {
		t.Errorf("预填充后应写入状态文件：%v", err)
	}
}

// TestScanCompleted_CompletedOnZeroNotifies completion_on=0（老任务/迁移后丢时间戳）
// 只要 progress+state 满足就算完成，照常通知——源码刻意不依赖 CompletedOn，
// finished 集合负责去重；通知后应把状态文件落盘
func TestScanCompleted_CompletedOnZeroNotifies(t *testing.T) {
	sink, rec := newNotifySink(t, http.StatusOK)
	env := setupFull(t, fullOpts{
		torrents: mkTorrents(doneTorrent("hash-bbbb-2222", "Old.Torrent.mkv", 0)),
		notifier: models.NotifierConfig{Enabled: true, AppriseURLs: []string{appriseURL(sink)}},
	})

	env.s.scanCompleted()

	if !env.s.finished["hash-bbbb-2222"] {
		t.Errorf("completion_on=0 但进度/状态满足，应写入 finished 集合，got %v", env.s.finished)
	}
	if rec.count() != 1 {
		t.Errorf("completion_on=0 应发 1 条通知，got %d 条", rec.count())
	}
	// 通知里完成时间应被兜底成当前时刻（源码对 CompletedOn==0 补时间戳）
	if !strings.Contains(rec.last(), "✅ 下载完成 · Old.Torrent.mkv") {
		t.Errorf("通知标题不符，got %q", rec.last())
	}
}

// TestScanCompleted_QBFetchErrorIsNoOp qB 拉取失败分支：直接返回，不写集合不发通知
func TestScanCompleted_QBFetchErrorIsNoOp(t *testing.T) {
	sink, rec := newNotifySink(t, http.StatusOK)
	env := setupFull(t, fullOpts{
		torrents: mkTorrents(doneTorrent("hash-cccc-3333", "Movie.mkv", 1700000000)),
		notifier: models.NotifierConfig{Enabled: true, AppriseURLs: []string{appriseURL(sink)}},
	})
	env.qbM.setFail(true)

	env.s.scanCompleted() // 不 panic 即为通过

	if len(env.s.finished) != 0 {
		t.Errorf("拉取失败不应写入 finished，got %v", env.s.finished)
	}
	if rec.count() != 0 {
		t.Errorf("拉取失败不应发通知，got %d 条", rec.count())
	}
}

// TestScanCompleted_SkipReasons 覆盖两种跳过原因（switch 分支）：
// 状态不匹配 / 已通知 —— 一条都不该通知（completion_on=0 不再是跳过原因，
// 由 TestScanCompleted_CompletedOnZeroNotifies 单独覆盖）
func TestScanCompleted_SkipReasons(t *testing.T) {
	sink, rec := newNotifySink(t, http.StatusOK)

	list := mkTorrents(
		// 状态不匹配：progress>=0.98 但还在下载态
		models.QBTorrent{Hash: "hash-mismatch-01", Name: "Almost.Done.mkv", State: "downloading", Progress: 0.99},
		// 已通知过：进度/状态都满足，但已在 finished 里
		doneTorrent("hash-notified-1", "Already.Notified.mkv", 1700000000),
	)

	env := setupFull(t, fullOpts{
		torrents: list,
		notifier: models.NotifierConfig{Enabled: true, AppriseURLs: []string{appriseURL(sink)}},
	})
	env.s.finished["hash-notified-1"] = true

	env.s.scanCompleted()

	if rec.count() != 0 {
		t.Errorf("两种跳过原因都不应发通知，got %d 条", rec.count())
	}
	if env.s.finished["hash-mismatch-01"] {
		t.Error("状态不匹配的任务不应写入 finished")
	}
	if !env.s.finished["hash-notified-1"] {
		t.Error("已通知的标记不应被扫描清掉")
	}
}

// TestScanCompleted_StateMismatchDiagnosticDump 接近完成但状态不匹配、
// 且本轮没有新完成时，走诊断 dump 分支（数量超过 dumpLimit 时截断）
func TestScanCompleted_StateMismatchDiagnosticDump(t *testing.T) {
	sink, rec := newNotifySink(t, http.StatusOK)
	var list []models.QBTorrent
	for i := 0; i < 25; i++ {
		list = append(list, models.QBTorrent{
			Hash:     fmt.Sprintf("hash%06d", i), // 长度 ≥10，诊断日志会截取前 10 位
			Name:     fmt.Sprintf("Almost.Done.%d.mkv", i),
			State:    "downloading",
			Progress: 0.99,
		})
	}

	env := setupFull(t, fullOpts{
		torrents: mkTorrents(list...),
		notifier: models.NotifierConfig{Enabled: true, AppriseURLs: []string{appriseURL(sink)}},
	})

	env.s.scanCompleted()

	if rec.count() != 0 {
		t.Errorf("状态不匹配不应发通知，got %d 条", rec.count())
	}
	if len(env.s.finished) != 0 {
		t.Errorf("状态不匹配不应写入 finished，got %v", env.s.finished)
	}
}

// TestScanCompleted_NotifierSkipped 表驱动：通知器未启用 / 启用但没配 URL 时跳过外发，
// 但仍要把任务标记为已通知（避免配置好之后重发历史任务）
func TestScanCompleted_NotifierSkipped(t *testing.T) {
	cases := []struct {
		name string
		cfg  models.NotifierConfig
	}{
		{"未启用", models.NotifierConfig{Enabled: false, AppriseURLs: []string{"json://127.0.0.1:1/notify"}}},
		{"启用但无URL", models.NotifierConfig{Enabled: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := setupFull(t, fullOpts{
				torrents: mkTorrents(doneTorrent("hash-skip-0001", "Skipped.mkv", 1700000000)),
				notifier: c.cfg,
			})
			// 扫到新完成任务 → 标记 + 落盘（saveFinished 不能在锁内调用）
			env.s.scanCompleted()
			if !env.s.finished["hash-skip-0001"] {
				t.Error("即使不外发也应标记为已通知")
			}
			if _, err := os.Stat(env.s.stateFile); err != nil {
				t.Errorf("标记后应写入状态文件：%v", err)
			}
		})
	}
}

// TestScanCompleted_NotifyFailureStillMarks 通知发送失败（对端 500）时记告警并继续，
// 任务依然标记为已通知
func TestScanCompleted_NotifyFailureStillMarks(t *testing.T) {
	sink, rec := newNotifySink(t, http.StatusInternalServerError)
	env := setupFull(t, fullOpts{
		torrents: mkTorrents(doneTorrent("hash-failed-01", "Notify.Fail.mkv", 1700000000)),
		notifier: models.NotifierConfig{Enabled: true, AppriseURLs: []string{appriseURL(sink)}},
	})

	env.s.scanCompleted()

	if rec.count() != 1 {
		t.Errorf("应尝试发送 1 次，got %d", rec.count())
	}
	if !env.s.finished["hash-failed-01"] {
		t.Error("发送失败也应标记已通知（不重复轰炸）")
	}
}

// ============ prefillFinished ============

// TestPrefillFinished_FillsHistory 启动时把历史已完成任务（只看 Progress+State，
// 不依赖 CompletedOn）预填充进集合，重启后不会给老任务重发；
// 进度未满的任务不预填充
func TestPrefillFinished_FillsHistory(t *testing.T) {
	env := setupFull(t, fullOpts{
		torrents: mkTorrents(
			doneTorrent("hash-old-000001", "Historical.mkv", 1690000000),
			// 老任务/迁移后丢时间戳：Progress+State 满足同样要预填充
			doneTorrent("hash-no-ts-0002", "No.Timestamp.mkv", 0),
			// 进度未满：不预填充
			models.QBTorrent{
				Hash: "hash-partial-003", Name: "Partial.mkv",
				State: "uploading", Progress: 0.5, CompletedOn: 1700000000,
			},
		),
	})

	env.s.prefillFinished()

	if !env.s.finished["hash-old-000001"] {
		t.Errorf("历史已完成任务应被预填充，got %v", env.s.finished)
	}
	if !env.s.finished["hash-no-ts-0002"] {
		t.Errorf("Progress+State 满足的任务应预填充（不依赖 CompletedOn），got %v", env.s.finished)
	}
	if env.s.finished["hash-partial-003"] {
		t.Errorf("进度未满的任务不应预填充，got %v", env.s.finished)
	}
	// n>0 时应落盘（二次调用时已全部存在 → n=0，不重复写也无副作用）
	if _, err := os.Stat(env.s.stateFile); err != nil {
		t.Errorf("预填充后应写入状态文件：%v", err)
	}
	env.s.prefillFinished()
	if len(env.s.finished) != 2 {
		t.Errorf("重复预填充不应改变集合，got %v", env.s.finished)
	}
}

// TestPrefillFinished_QBErrorIsNoOp qB 拉取失败分支：直接返回，不写集合
func TestPrefillFinished_QBErrorIsNoOp(t *testing.T) {
	env := setupFull(t, fullOpts{
		torrents: mkTorrents(doneTorrent("hash-err-000001", "Err.mkv", 1700000000)),
	})
	env.qbM.setFail(true)

	env.s.prefillFinished()

	if len(env.s.finished) != 0 {
		t.Errorf("拉取失败不应预填充，got %v", env.s.finished)
	}
}

// ============ Start / Stop 生命周期 ============

// TestStartStop_Lifecycle Start 后限速器/文件管理/RSS 各自真的跑起来，
// Stop 后所有常驻 goroutine 退出且不再产生新的 qB 请求。
func TestStartStop_Lifecycle(t *testing.T) {
	// 1) 本地假 RSS 订阅源（验证 rss 引擎启动后会去拉）
	feedHits := 0
	var feedMu sync.Mutex
	feedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		feedMu.Lock()
		feedHits++
		feedMu.Unlock()
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<?xml version="1.0"?><rss version="2.0"><channel><title>t</title><item><title>ep1</title><link>http://x/1.torrent</link><guid>g1</guid></item></channel></rss>`))
	}))
	t.Cleanup(feedSrv.Close)

	// 2) 造一个"单文件"任务目录，让文件管理扫描时走 stop/rename/start 分支
	saveDir := t.TempDir()
	torrentDir := filepath.Join(saveDir, "Test.Single.mkv")
	if err := os.MkdirAll(torrentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(torrentDir, "video.mkv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	env := setupFull(t, fullOpts{
		torrents: mkTorrents(models.QBTorrent{
			Hash: "hash-life-0001", Name: "Test.Single.mkv",
			State: "stoppedUP", Progress: 1, Size: 1024,
			CompletedOn: time.Now().Unix(), SavePath: saveDir,
		}),
		limiterOn: true,
		limiterRules: []models.LimitRule{
			{ID: "r1", Name: "test", Enabled: true, Match: `(?i)test`, UploadLimit: 512},
		},
		filemgrOn: true,
		rssOn:     true,
		feedURL:   feedSrv.URL,
	})

	env.s.Start()

	// 子模块全部启动：限速器下发限速、文件管理做单文件移动、RSS 拉订阅源
	waitUntil(t, 6*time.Second, "限速器下发 setUploadLimit",
		func() bool { return env.qbM.count("limit") >= 1 })
	waitUntil(t, 6*time.Second, "文件管理触发 renameFile",
		func() bool { return env.qbM.count("rename") >= 1 })
	waitUntil(t, 6*time.Second, "RSS 拉取订阅源",
		func() bool {
			feedMu.Lock()
			defer feedMu.Unlock()
			return feedHits >= 1
		})

	// 调度器自身 + 三个子模块的常驻 ticker goroutine 都应该在跑
	if missing := missingGoroutines(schedulerTickerSyms); len(missing) > 0 {
		t.Errorf("Start 后应存在这些 goroutine，缺失：%v", missing)
	}

	env.s.Stop()

	// Stop 后所有常驻 ticker goroutine 应退出（带超时轮询，不做固定长 sleep）
	waitNoGoroutines(t, 5*time.Second, "Stop 后 ticker goroutine 退出", schedulerTickerSyms)

	// Stop 会把 finished 落盘
	waitUntil(t, 2*time.Second, "Stop 后状态文件写入",
		func() bool {
			_, err := os.Stat(env.s.stateFile)
			return err == nil
		})

	// 已退出的 ticker 不应再产生新的限速请求（等超过一个轮询周期 1s）
	before := env.qbM.count("limit")
	time.Sleep(1300 * time.Millisecond)
	if after := env.qbM.count("limit"); after != before {
		t.Errorf("Stop 后不应再有新的限速请求：%d -> %d", before, after)
	}
}

// TestStartStop_SubSystemsDisabled 子系统全部禁用时也能正常 Start/Stop（不 panic）：
// 只拉起调度器自身的扫描/持久化 goroutine，Stop 后同样全部退出
func TestStartStop_SubSystemsDisabled(t *testing.T) {
	env := setupFull(t, fullOpts{
		torrents: mkTorrents(doneTorrent("hash-dis-000001", "Idle.mkv", 1700000000)),
	})
	env.s.Start()

	selfSyms := []string{"scheduler.(*Scheduler).Start.func"}
	if missing := missingGoroutines(selfSyms); len(missing) > 0 {
		t.Errorf("Start 后应存在调度器自身的 goroutine，缺失：%v", missing)
	}
	// 子模块禁用时不应启动它们的轮询 goroutine
	others := schedulerTickerSyms[1:]
	if alive := missingGoroutines(others); len(alive) != len(others) {
		t.Errorf("子模块禁用时不应启动其 goroutine，仍在运行：%v", alive)
	}

	env.s.Stop()
	waitNoGoroutines(t, 5*time.Second, "Stop 后 goroutine 退出", selfSyms)
}

// ============ ReloadAll ============

// TestReloadAll_HotSwapsQBClient 配置变更后热替换 qb 客户端：
// 同一个 *Client 指针改完凭据后，请求全部打到新 qB；
// 各子系统按新配置重启 ticker（这里用限速器 1s 轮询验证）
func TestReloadAll_HotSwapsQBClient(t *testing.T) {
	// 新的 qB：返回带标记的任务列表
	newQB := newQBMock(mkTorrents(models.QBTorrent{
		Hash: "hash-new-qb-001", Name: "From.New.QB.mkv",
		State: "downloading", Progress: 0.5,
	}))
	newSrv := newQBServer(t, newQB)

	env := setupFull(t, fullOpts{
		torrents: mkTorrents(models.QBTorrent{
			Hash: "hash-old-qb-001", Name: "From.Old.QB.mkv",
			State: "downloading", Progress: 0.5,
		}),
		limiterOn: false, // 先不启动 ticker，ReloadAll 时按新配置拉起
	})

	// 热替换前：打到旧 qB
	if list, err := env.s.client.GetTorrents("all", "", ""); err != nil || len(list) == 0 || list[0].Name != "From.Old.QB.mkv" {
		t.Fatalf("热替换前应读到旧 qB 数据：list=%v err=%v", list, err)
	}
	oldHits := env.qbM.count("info")

	// 打开限速器（1s 轮询 + 命中规则），ReloadAll 应按新配置把 ticker 拉起来
	cfg := env.cfg.Get()
	cfg.Limiter.Enabled = true
	cfg.Limiter.Interval = 1
	cfg.Limiter.Rules = []models.LimitRule{
		{ID: "r1", Name: "new", Enabled: true, Match: `(?i)from`, UploadLimit: 256},
	}
	env.cfg.Set(cfg)

	env.s.ReloadAll(models.QBConfig{URL: newSrv.URL, Username: "u", Password: "p", APIKey: "qbt_new_key"})
	defer env.s.Stop()

	// 同一个 client 指针，凭据已热换 → 请求全部打到新 qB
	list, err := env.s.client.GetTorrents("all", "", "")
	if err != nil {
		t.Fatalf("热替换后拉取失败：%v", err)
	}
	if len(list) == 0 || list[0].Name != "From.New.QB.mkv" {
		t.Errorf("热替换后应读到新 qB 数据，got %+v", list)
	}
	if newQB.count("info") < 1 {
		t.Error("新 qB 应至少收到一次请求")
	}
	if env.qbM.count("info") != oldHits {
		t.Errorf("旧 qB 不应再收到请求：%d -> %d", oldHits, env.qbM.count("info"))
	}

	// 子系统按新配置重启：限速器 ticker 开始对新 qB 下发限速
	waitUntil(t, 4*time.Second, "ReloadAll 后限速器 ticker 重启",
		func() bool { return newQB.count("limit") >= 1 })
}

// ============ New / loadFinished / saveFinished 边界分支 ============

// TestNew_LoadsFinishedFromDisk New 会把同目录下已有的 finished.json 预加载
func TestNew_LoadsFinishedFromDisk(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	t.Setenv("QBHIVE_CONFIG", cfgPath)

	// 预置历史状态文件（New 会加载它）
	if err := os.WriteFile(filepath.Join(dir, finishedFile), []byte(`["hash-a","hash-b"]`), 0o644); err != nil {
		t.Fatal(err)
	}

	s := New(nil, nil, nil, nil, nil, nil)
	if s.stateFile != filepath.Join(dir, finishedFile) {
		t.Errorf("stateFile = %q, want %q", s.stateFile, filepath.Join(dir, finishedFile))
	}
	if !s.finished["hash-a"] || !s.finished["hash-b"] {
		t.Errorf("New 应加载历史已通知集合，got %v", s.finished)
	}
}

// TestLoadFinished_ReadErrorIsNoOp 状态文件本身是个目录 → 读取失败（非"不存在"），
// 走告警分支并安全返回
func TestLoadFinished_ReadErrorIsNoOp(t *testing.T) {
	s := setupScheduler(t)
	s.stateFile = t.TempDir() // 目录：ReadFile 会报错但不是 os.IsNotExist
	s.finished = make(map[string]bool)
	s.loadFinished()
	if len(s.finished) != 0 {
		t.Errorf("读取失败时集合应保持为空，got %v", s.finished)
	}
}

// TestSaveFinished_WriteErrorIsNoOp 父路径被一个普通文件占着 → 写入失败，
// 走告警分支并安全返回（不 panic、不覆盖）
func TestSaveFinished_WriteErrorIsNoOp(t *testing.T) {
	s := setupScheduler(t)
	tmp := t.TempDir()
	blocker := filepath.Join(tmp, "blocker")
	if err := os.WriteFile(blocker, []byte("file"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.stateFile = filepath.Join(blocker, "finished.json")
	s.finished["x"] = true
	s.saveFinished() // 不 panic 即为通过
	if _, err := os.Stat(s.stateFile); err == nil {
		t.Error("写入路径被文件占用时不应产出状态文件")
	}
}
