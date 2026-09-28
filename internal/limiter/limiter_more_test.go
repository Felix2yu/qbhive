package limiter

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Felix2yu/qbhive/internal/config"
	"github.com/Felix2yu/qbhive/internal/models"
	qb "github.com/Felix2yu/qbhive/internal/qbittorrent"
)

// limiterEnv 是可定制的 qB mock 环境：torrents 内容与 setUploadLimit 状态码都能在测试过程中改，
// 用于模拟「torrent 列表变化」「qB 接口报错」等动态场景。
type limiterEnv struct {
	l   *Limiter
	srv *httptest.Server

	mu           sync.Mutex
	torrentsJSON string
	limitStatus  int // setUploadLimit 返回的状态码，0 视为 200
	limitCalls   []string
	infoCalls    int
}

func newLimiterEnv(t *testing.T, rules []models.LimitRule, torrentsJSON string) *limiterEnv {
	t.Helper()
	env := &limiterEnv{torrentsJSON: torrentsJSON}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "SID=test")
		_, _ = w.Write([]byte("Ok."))
	})
	mux.HandleFunc("/api/v2/torrents/info", func(w http.ResponseWriter, r *http.Request) {
		env.mu.Lock()
		env.infoCalls++
		body := env.torrentsJSON
		env.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-bittorrent")
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("/api/v2/torrents/setUploadLimit", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		env.mu.Lock()
		env.limitCalls = append(env.limitCalls, r.FormValue("hashes")+":"+r.FormValue("limit"))
		status := env.limitStatus
		env.mu.Unlock()
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
	})
	env.srv = httptest.NewServer(mux)
	t.Cleanup(env.srv.Close)

	cfg := config.New(filepath.Join(t.TempDir(), "config.json"))
	cfg.Set(models.AppConfig{
		Limiter: models.LimiterConfig{Enabled: true, Interval: 10, Rules: rules},
	})
	env.l = New(cfg, qb.New(env.srv.URL, "u", "p", ""))
	return env
}

// setTorrents 运行期替换 torrent 列表
func (e *limiterEnv) setTorrents(json string) {
	e.mu.Lock()
	e.torrentsJSON = json
	e.mu.Unlock()
}

func (e *limiterEnv) calls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.limitCalls))
	copy(out, e.limitCalls)
	return out
}

// defaultRules 与现有测试风格保持一致：4k / remux 两条启用规则 + 一条禁用规则
func defaultRules() []models.LimitRule {
	return []models.LimitRule{
		{ID: "r1", Name: "4k", Enabled: true, Match: `(?i)4k`, UploadLimit: 10240},
		{ID: "r2", Name: "remux", Enabled: true, Match: `(?i)remux`, UploadLimit: 0},
		{ID: "r3", Name: "off", Enabled: false, Match: `test`, UploadLimit: 999},
	}
}

// waitFor 轮询直到 fn 返回 true；上限 800ms，避免长时间阻塞
func waitFor(t *testing.T, fn func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(800 * time.Millisecond)
	for time.Now().Before(deadline) {
		if fn() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return fn()
}

// settle 给后台 ticker goroutine 一点退出时间，避免与后续断言产生数据竞争
func settle() { time.Sleep(80 * time.Millisecond) }

// ============ Start / Stop / Reload 生命周期 ============

// TestLimiter_Reload_WithNewClient 覆盖 Reload 里 c != nil 热替换 client 的分支
func TestLimiter_Reload_WithNewClient(t *testing.T) {
	env := newLimiterEnv(t, defaultRules(), `[]`)
	// 未启动任何 ticker，替换 client 无并发风险
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "SID=test")
		_, _ = w.Write([]byte("Ok."))
	}))
	defer srv2.Close()

	newCli := qb.New(srv2.URL, "u2", "p2", "")
	env.l.Reload(newCli)
	if env.l.client != newCli {
		t.Error("Reload 应替换 client 指针")
	}
	env.l.Stop()
	settle()
}

func TestLimiter_Start_EnabledSpawnsTicker(t *testing.T) {
	env := newLimiterEnv(t, defaultRules(), `[]`)
	env.l.Start()

	if env.l.tickerStop == nil {
		t.Fatal("Start 后应创建 tickerStop")
	}
	if !env.l.running {
		t.Error("Start 后 running 应为 true")
	}
	env.l.Stop()
	settle()
}

func TestLimiter_Start_DisabledNoTicker(t *testing.T) {
	env := newLimiterEnv(t, defaultRules(), `[]`)
	cfg := env.l.cfg.Get()
	cfg.Limiter.Enabled = false
	env.l.cfg.Set(cfg)

	env.l.Start()
	if env.l.tickerStop != nil {
		t.Error("停用时 Start 不应创建 ticker")
	}
	if env.l.running {
		t.Error("停用时 running 应为 false")
	}
}

func TestLimiter_Start_NonPositiveIntervalUsesDefault(t *testing.T) {
	env := newLimiterEnv(t, defaultRules(), `[]`)
	cfg := env.l.cfg.Get()
	cfg.Limiter.Interval = 0
	env.l.cfg.Set(cfg)

	// Interval<=0 走默认 10s，不应 panic；立刻 Stop 结束后台 goroutine
	env.l.Start()
	if env.l.tickerStop == nil {
		t.Fatal("即使 Interval=0 也应按默认间隔启动 ticker")
	}
	env.l.Stop()
	settle()
}

func TestLimiter_Reload_RestartsRunningTicker(t *testing.T) {
	env := newLimiterEnv(t, defaultRules(), `[]`)
	env.l.Start()
	old := env.l.tickerStop

	cfg := env.l.cfg.Get()
	cfg.Limiter.Interval = -1 // 触发 Reload 里的默认间隔分支
	env.l.cfg.Set(cfg)
	env.l.Reload(nil)

	if env.l.tickerStop == nil {
		t.Fatal("Reload 后应重建 tickerStop")
	}
	if env.l.tickerStop == old {
		t.Error("Reload 应换掉旧的 tickerStop")
	}
	if !env.l.running {
		t.Error("Reload 启用后 running 应为 true")
	}
	// 旧 ticker 走 <-stop 分支退出、新 ticker 走 <-l.stop 分支退出
	env.l.Stop()
	settle()
}

// TestLimiter_RunTicker_TickAppliesRules 覆盖 runTicker 的 ticker.C 分支：
// ticker 触发 apply 后由 Stop 收尾；此处刻意不做 Reload（Reload 会写 lastLimits，
// 与后台 apply 的读没有 happens-before 关系，-race 会误判为竞争）。
func TestLimiter_RunTicker_TickAppliesRules(t *testing.T) {
	env := newLimiterEnv(t, defaultRules(), `[{"hash":"h1","name":"Movie.4k.mkv"}]`)

	// 直接用小间隔驱动 runTicker，覆盖 ticker.C → apply 分支（不依赖秒级 Interval）
	env.l.runTicker(50 * time.Millisecond)
	if !waitFor(t, func() bool { return len(env.calls()) > 0 }) {
		t.Fatalf("ticker 应触发 apply 并下发限速: %v", env.calls())
	}
	if got := env.calls()[0]; got != "h1:10485760" {
		t.Errorf("限速值错误: got %q want h1:10485760", got)
	}

	// Stop 关闭 l.stop → goroutine 走 <-l.stop 分支退出
	env.l.Stop()
	settle()
}

// TestLimiter_RunTicker_ReloadStopsIdleTicker 覆盖 runTicker 的 <-stop 分支：
// ticker 间隔拉到 1 小时，保证 goroutine 从未执行过 apply（否则 Reload 与其读竞争），
// 再用 Reload 关闭 tickerStop 唤醒它退出。
func TestLimiter_RunTicker_ReloadStopsIdleTicker(t *testing.T) {
	env := newLimiterEnv(t, defaultRules(), `[]`)
	env.l.runTicker(time.Hour)
	if env.l.tickerStop == nil {
		t.Fatal("runTicker 应创建 tickerStop")
	}

	env.l.Reload(nil) // 关闭旧 tickerStop → 旧 goroutine 走 <-stop 分支返回
	if env.l.tickerStop == nil {
		t.Fatal("Reload 后应重建 tickerStop")
	}
	settle()

	env.l.Stop()
	settle()
}

// ============ ApplyToTorrent（立即下发） ============

func TestLimiter_ApplyToTorrent(t *testing.T) {
	cases := []struct {
		name      string
		rules     []models.LimitRule
		torrent   string // name
		wantLimit string // 期望 setUploadLimit 的 limit 表单值；"" 表示不应有调用
	}{
		{
			name:      "命中且配置了限速",
			rules:     defaultRules(),
			torrent:   "Movie.4k.HDR.mkv",
			wantLimit: "10485760",
		},
		{
			name:      "命中但限速为0表示不限制",
			rules:     defaultRules(),
			torrent:   "Movie.REMUX.mkv",
			wantLimit: "-1",
		},
		{
			name:      "规则被禁用则不下发",
			rules:     []models.LimitRule{{ID: "r", Name: "off", Enabled: false, Match: `test`, UploadLimit: 999}},
			torrent:   "test-name",
			wantLimit: "",
		},
		{
			name:      "正则非法则跳过",
			rules:     []models.LimitRule{{ID: "r", Name: "bad", Enabled: true, Match: `[`, UploadLimit: 999}},
			torrent:   "any-name",
			wantLimit: "",
		},
		{
			name:      "不匹配则不下发",
			rules:     defaultRules(),
			torrent:   "SomeOther.Name",
			wantLimit: "",
		},
		{
			name: "多条命中只用第一条",
			rules: []models.LimitRule{
				{ID: "a", Name: "first", Enabled: true, Match: `Movie`, UploadLimit: 100},
				{ID: "b", Name: "second", Enabled: true, Match: `4k`, UploadLimit: 200},
			},
			torrent:   "Movie.4k.mkv",
			wantLimit: "102400",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newLimiterEnv(t, tc.rules, `[]`)
			env.l.ApplyToTorrent("h1", tc.torrent)

			calls := env.calls()
			if tc.wantLimit == "" {
				if len(calls) != 0 {
					t.Fatalf("不应下发限速，却收到 %v", calls)
				}
				return
			}
			if len(calls) != 1 {
				t.Fatalf("应恰好下发一次，收到 %v", calls)
			}
			if calls[0] != "h1:"+tc.wantLimit {
				t.Errorf("限速错误: got %q want h1:%s", calls[0], tc.wantLimit)
			}
		})
	}
}

func TestLimiter_ApplyToTorrent_APIErrorIsIgnored(t *testing.T) {
	env := newLimiterEnv(t, defaultRules(), `[]`)
	env.mu.Lock()
	env.limitStatus = http.StatusInternalServerError
	env.mu.Unlock()

	// 下发失败不应 panic（错误被忽略）
	env.l.ApplyToTorrent("h1", "Movie.4k.mkv")
	if len(env.calls()) != 1 {
		t.Errorf("应尝试下发一次，收到 %v", env.calls())
	}
}

// ============ apply 的剩余分支 ============

func TestLimiter_apply_InvalidRegexRuleSkipped(t *testing.T) {
	rules := []models.LimitRule{
		{ID: "bad", Name: "bad", Enabled: true, Match: `[unclosed`, UploadLimit: 500},
		{ID: "ok", Name: "ok", Enabled: true, Match: `4k`, UploadLimit: 1024},
	}
	env := newLimiterEnv(t, rules, `[{"hash":"h1","name":"Movie.4k.mkv"}]`)
	env.l.apply()

	calls := env.calls()
	if len(calls) != 1 || calls[0] != "h1:1048576" {
		t.Errorf("非法正则应被跳过并命中下一条规则: got %v", calls)
	}
}

func TestLimiter_apply_SetUploadLimitErrorRetriesNextRound(t *testing.T) {
	env := newLimiterEnv(t, defaultRules(), `[{"hash":"h1","name":"Movie.4k.mkv"}]`)
	env.mu.Lock()
	env.limitStatus = http.StatusInternalServerError
	env.mu.Unlock()

	env.l.apply()
	// 失败时不应记录 lastLimits
	if _, ok := env.l.lastLimits["h1"]; ok {
		t.Error("下发失败不应写入 lastLimits")
	}

	// 恢复后下一轮应重试
	env.mu.Lock()
	env.limitStatus = 0
	env.mu.Unlock()
	env.l.apply()
	if _, ok := env.l.lastLimits["h1"]; !ok {
		t.Error("恢复后应记录 lastLimits")
	}
	if got := len(env.calls()); got != 2 {
		t.Errorf("应重试下发，调用次数=%d want 2", got)
	}
}

func TestLimiter_apply_CleansVanishedHashes(t *testing.T) {
	env := newLimiterEnv(t, defaultRules(), `[{"hash":"h1","name":"Movie.4k.mkv"},{"hash":"h2","name":"Other.REMUX"}]`)
	env.l.apply()
	if len(env.l.lastLimits) != 2 {
		t.Fatalf("首轮应记录 2 条 lastLimits: %v", env.l.lastLimits)
	}

	// h2 从 qB 里消失 → 下一轮清理
	env.setTorrents(`[{"hash":"h1","name":"Movie.4k.mkv"}]`)
	env.l.apply()
	if _, ok := env.l.lastLimits["h2"]; ok {
		t.Errorf("消失的 hash 应被清理: %v", env.l.lastLimits)
	}
	if _, ok := env.l.lastLimits["h1"]; !ok {
		t.Errorf("仍存在的 hash 不应被清理: %v", env.l.lastLimits)
	}
}

func TestLimiter_apply_UnmatchedDoesNotTouchLastLimits(t *testing.T) {
	// 未命中规则的 torrent 不下发 -1，也不进 lastLimits
	env := newLimiterEnv(t, defaultRules(), `[{"hash":"h9","name":"NoRuleMatch.mkv"}]`)
	env.l.apply()
	if len(env.calls()) != 0 {
		t.Errorf("未命中规则不应下发: %v", env.calls())
	}
	if len(env.l.lastLimits) != 0 {
		t.Errorf("未命中规则不应记录: %v", env.l.lastLimits)
	}
}
