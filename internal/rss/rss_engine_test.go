package rss

// 本文件补充 Engine 生命周期 / 拉取 / 提交 / 状态 等单元测试。
// 全部基于 net/http/httptest 模拟 RSS 源与 qBittorrent WebUI API，
// 状态文件用 t.TempDir() + QBHIVE_CONFIG 隔离，禁止访问外网。

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Felix2yu/qbhive/internal/config"
	"github.com/Felix2yu/qbhive/internal/models"
	qb "github.com/Felix2yu/qbhive/internal/qbittorrent"
)

// ==================== 测试基础设施 ====================

// rssItemXML 构造一条 <item>（空字段自动省略）
func rssItemXML(title, link, guid, enc string) string {
	var b strings.Builder
	b.WriteString("<item><title>")
	_ = xml.EscapeText(&b, []byte(title))
	b.WriteString("</title>")
	if link != "" {
		b.WriteString("<link>")
		_ = xml.EscapeText(&b, []byte(link))
		b.WriteString("</link>")
	}
	if guid != "" {
		b.WriteString("<guid>")
		_ = xml.EscapeText(&b, []byte(guid))
		b.WriteString("</guid>")
	}
	if enc != "" {
		b.WriteString(`<enclosure url="`)
		_ = xml.EscapeText(&b, []byte(enc))
		b.WriteString(`" type="application/x-bittorrent"/>`)
	}
	b.WriteString("</item>")
	return b.String()
}

// rssDoc 把若干 <item> 拼成最小合法 RSS 文档
func rssDoc(items ...string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>` +
		`<rss version="2.0"><channel><title>测试订阅源</title>` +
		strings.Join(items, "") + `</channel></rss>`
}

// mockFeed 可编程的 RSS 源测试服务器（可动态改 body / 状态码）
type mockFeed struct {
	srv    *httptest.Server
	mu     sync.Mutex
	body   string
	status int
	hits   int
}

// newMockFeed 启动一个固定 body、200 状态码的 RSS 源
func newMockFeed(body string) *mockFeed {
	m := &mockFeed{body: body, status: http.StatusOK}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.hits++
		body, status := m.body, m.status
		m.mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	return m
}

// set 动态修改返回内容与状态码
func (m *mockFeed) set(body string, status int) {
	m.mu.Lock()
	m.body, m.status = body, status
	m.mu.Unlock()
}

// hitCount 返回被请求次数
func (m *mockFeed) hitCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hits
}

// URL 返回服务地址
func (m *mockFeed) URL() string { return m.srv.URL }

// Close 关闭服务
func (m *mockFeed) Close() { m.srv.Close() }

// newStaticServer 返回一个固定响应体的测试服务器（用于种子文件等）
func newStaticServer(body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
}

// addRecord 记录一次 /api/v2/torrents/add 的表单内容
type addRecord struct {
	SavePath     string
	Category     string
	Tags         string
	SkipChecking string
	Stopped      string
	Torrent      []byte
}

// mockQB 模拟 qBittorrent WebUI API（登录 / add / info / setUploadLimit）
type mockQB struct {
	srv      *httptest.Server
	mu       sync.Mutex
	adds     []addRecord
	limits   []int64 // setUploadLimit 收到的 limit（字节/秒）
	infoJSON string  // torrents/info 的响应体
}

// newMockQB 启动模拟 qB WebUI
func newMockQB() *mockQB {
	m := &mockQB{infoJSON: `[{"hash":"abc123","name":"t","state":"downloading"}]`}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		switch r.URL.Path {
		case "/api/v2/auth/login":
			w.Header().Add("Set-Cookie", "SID=sid1; Path=/")
			_, _ = io.WriteString(w, "Ok.")
		case "/api/v2/torrents/add":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			rec := addRecord{}
			if v := r.MultipartForm.Value["savepath"]; len(v) > 0 {
				rec.SavePath = v[0]
			}
			if v := r.MultipartForm.Value["category"]; len(v) > 0 {
				rec.Category = v[0]
			}
			if v := r.MultipartForm.Value["tags"]; len(v) > 0 {
				rec.Tags = v[0]
			}
			if v := r.MultipartForm.Value["skip_checking"]; len(v) > 0 {
				rec.SkipChecking = v[0]
			}
			// qBittorrent 5.x 只认 stopped（4.x 的 paused 已被静默忽略）
			if v := r.MultipartForm.Value["stopped"]; len(v) > 0 {
				rec.Stopped = v[0]
			}
			if fhs := r.MultipartForm.File["torrents"]; len(fhs) > 0 {
				if f, err := fhs[0].Open(); err == nil {
					data, _ := io.ReadAll(f)
					_ = f.Close()
					rec.Torrent = data
				}
			}
			m.adds = append(m.adds, rec)
			_, _ = io.WriteString(w, "Ok.")
		case "/api/v2/torrents/info":
			_, _ = io.WriteString(w, m.infoJSON)
		case "/api/v2/torrents/setUploadLimit":
			_ = r.ParseForm()
			if lim, err := strconv.ParseInt(r.FormValue("limit"), 10, 64); err == nil {
				m.limits = append(m.limits, lim)
			}
			_, _ = io.WriteString(w, "Ok.")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return m
}

// addList 返回 add 请求的副本
func (m *mockQB) addList() []addRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]addRecord, len(m.adds))
	copy(out, m.adds)
	return out
}

// addCount 返回 add 请求次数
func (m *mockQB) addCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.adds)
}

// limitList 返回收到的限速值副本
func (m *mockQB) limitList() []int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]int64, len(m.limits))
	copy(out, m.limits)
	return out
}

// newClient 用 API key 模式构造 qb 客户端（跳过 cookie 登录）
func (m *mockQB) newClient() *qb.Client {
	return qb.New(m.srv.URL, "admin", "admin", "test-api-key")
}

// Close 关闭服务
func (m *mockQB) Close() { m.srv.Close() }

// newTestEngine 用临时目录里的 config.json 构造 Engine（QBHIVE_CONFIG 指向它）
func newTestEngine(t *testing.T, rssCfg models.RSSConfig, cli *qb.Client) *Engine {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	t.Setenv("QBHIVE_CONFIG", cfgPath)
	m := config.New(cfgPath)
	full := m.Get()
	full.RSS = rssCfg
	m.Set(full)
	return New(m, cli)
}

// waitFor 轮询等待条件成立（每 5ms 一次），避免固定长 sleep
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待超时：%s", msg)
}

// stopEngine 返回只会执行一次的 Stop，避免重复 close channel panic
func stopEngine(e *Engine) func() {
	var once sync.Once
	return func() { once.Do(e.Stop) }
}

// ==================== feedState ====================

// TestNewFeedState_PushAndCollectRecent 覆盖 newFeedState / pushRecent / collectRecent
func TestNewFeedState_PushAndCollectRecent(t *testing.T) {
	s := newFeedState()
	if s.recent == nil {
		t.Fatal("newFeedState 应初始化 recent 列表")
	}
	if got := len(s.collectRecent()); got != 0 {
		t.Errorf("空状态 collectRecent 长度 = %d, 期望 0", got)
	}
	// 推 105 条，最多保留最新 100 条
	for i := 0; i < 105; i++ {
		s.pushRecent(RecentItem{Title: fmt.Sprintf("item-%d", i), Action: "downloaded"})
	}
	got := s.collectRecent()
	if len(got) != 100 {
		t.Fatalf("超过 100 条后长度 = %d, 期望 100", len(got))
	}
	if got[0].Title != "item-104" {
		t.Errorf("最新条目应在最前，got %q", got[0].Title)
	}
	if got[99].Title != "item-5" {
		t.Errorf("最旧保留条目应为 item-5, got %q", got[99].Title)
	}
	if got[0].Action != "downloaded" {
		t.Errorf("Action 应保留，got %q", got[0].Action)
	}
}

// TestGetOrCreateState 覆盖 getOrCreateState 的复用与新建分支
func TestGetOrCreateState(t *testing.T) {
	e := newTestEngine(t, models.RSSConfig{}, nil)
	s1 := e.getOrCreateState("f1")
	if s1 == nil {
		t.Fatal("getOrCreateState 不应返回 nil")
	}
	s2 := e.getOrCreateState("f1")
	if s1 != s2 {
		t.Error("同一 feed 应复用同一个 state")
	}
	s3 := e.getOrCreateState("f2")
	if s3 == s1 || s3 == s2 {
		t.Error("不同 feed 应各自持有 state")
	}
}

// ==================== httpGet ====================

// TestHttpGet 表驱动覆盖 httpGet 的成功 / 4xx / 连不上 / 非法 URL 分支
func TestHttpGet(t *testing.T) {
	okSrv := newStaticServer("hello-body")
	defer okSrv.Close()
	missing := newMockFeed("not found")
	missing.set("not found", http.StatusNotFound)
	defer missing.Close()

	cases := []struct {
		name     string
		target   string
		wantErr  bool
		wantBody string
	}{
		{name: "正常返回", target: okSrv.URL, wantErr: false, wantBody: "hello-body"},
		{name: "404 状态码", target: missing.URL(), wantErr: true},
		{name: "端口无人监听", target: "http://127.0.0.1:1/", wantErr: true},
		{name: "非法 URL", target: "://bad url", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, err := httpGet(tc.target)
			if tc.wantErr {
				if err == nil {
					t.Errorf("期望报错，实际拿到 body=%q", body)
				}
				return
			}
			if err != nil {
				t.Fatalf("httpGet 报错：%v", err)
			}
			if string(body) != tc.wantBody {
				t.Errorf("body = %q, 期望 %q", body, tc.wantBody)
			}
		})
	}
}

// ==================== processItem ====================

// TestProcessItem 表驱动覆盖规则禁用 / 无命中 / 无 URL / 下载失败 / 提交失败
func TestProcessItem(t *testing.T) {
	tor := newStaticServer("torrent-content")
	defer tor.Close()
	badFeed := newMockFeed("nope")
	badFeed.set("nope", http.StatusNotFound)
	defer badFeed.Close()
	qbm := newMockQB()
	defer qbm.Close()

	feed := models.RSSFeed{
		Name: "测试源",
		Rules: []models.RSSRule{
			{ID: "r-off", Name: "禁用规则", Enabled: false, Mode: "keyword", Include: "movie"},
			{ID: "r-hit", Name: "命中规则", Enabled: true, Mode: "keyword", Include: "1080p",
				SavePath: "/dl/movies", Category: "movies", Tags: "auto,rss"},
		},
	}
	good := newTestEngine(t, models.RSSConfig{}, qbm.newClient())
	// qB 完全连不上 → AddTorrent 报错
	badQB := newTestEngine(t, models.RSSConfig{}, qb.New("http://127.0.0.1:1", "u", "p", "k"))

	cases := []struct {
		name     string
		eng      *Engine
		item     rssItem
		wantRule string
		wantErr  string
	}{
		{
			name:     "命中并成功提交",
			eng:      good,
			item:     rssItem{Title: "Movie.1080p.mkv", Enclosure: rssEnclosure{URL: tor.URL}},
			wantRule: "命中规则",
		},
		{
			name:     "命中但没有下载地址",
			eng:      good,
			item:     rssItem{Title: "Movie.1080p.mkv"},
			wantRule: "命中规则",
		},
		{
			name:     "没有规则命中",
			eng:      good,
			item:     rssItem{Title: "Movie.720p.mkv", Enclosure: rssEnclosure{URL: tor.URL}},
			wantRule: "",
		},
		{
			name:     "下载种子失败",
			eng:      good,
			item:     rssItem{Title: "Movie.1080p.mkv", Enclosure: rssEnclosure{URL: badFeed.URL()}},
			wantRule: "命中规则",
			wantErr:  "下载 torrent 失败",
		},
		{
			name:     "添加到 qB 失败",
			eng:      badQB,
			item:     rssItem{Title: "Movie.1080p.mkv", Enclosure: rssEnclosure{URL: tor.URL}},
			wantRule: "命中规则",
			wantErr:  "添加到 qBittorrent 失败",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rule, err := tc.eng.processItem(feed, tc.item)
			if rule != tc.wantRule {
				t.Errorf("规则名 = %q, 期望 %q", rule, tc.wantRule)
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("期望无错，实际：%v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("期望错误包含 %q，实际无错", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("错误 %q 不包含 %q", err.Error(), tc.wantErr)
			}
		})
	}

	// 只有第一条命中用例会真正提交一次
	adds := qbm.addList()
	if len(adds) != 1 {
		t.Fatalf("qB 收到 %d 次 add，期望 1 次", len(adds))
	}
	if adds[0].SavePath != "/dl/movies" || adds[0].Category != "movies" || adds[0].Tags != "auto,rss" {
		t.Errorf("add 表单参数不对：%+v", adds[0])
	}
	if string(adds[0].Torrent) != "torrent-content" {
		t.Errorf("种子内容 = %q", adds[0].Torrent)
	}
}

// TestMatchRule_EmptyORGroup 表驱动：include/exclude 里的空 OR 组应被跳过
func TestMatchRule_EmptyORGroup(t *testing.T) {
	cases := []struct {
		name  string
		rule  models.RSSRule
		title string
		want  bool
	}{
		{
			name:  "include 含空 OR 组时跳过",
			rule:  models.RSSRule{Mode: "keyword", Include: "|movie|"},
			title: "A Movie From 2024",
			want:  true,
		},
		{
			name:  "include 空组不参与命中",
			rule:  models.RSSRule{Mode: "keyword", Include: "|zzz"},
			title: "A Movie From 2024",
			want:  false,
		},
		{
			name:  "exclude 含空 OR 组时跳过",
			rule:  models.RSSRule{Mode: "keyword", Include: "movie", Exclude: "|extras"},
			title: "A Movie From 2024",
			want:  true,
		},
		{
			name:  "exclude 末尾空组不影响排除",
			rule:  models.RSSRule{Mode: "keyword", Include: "movie", Exclude: "extras|"},
			title: "A Movie Extras 2024",
			want:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchRule(tc.rule, tc.title); got != tc.want {
				t.Errorf("matchRule(%+v, %q) = %v, 期望 %v", tc.rule, tc.title, got, tc.want)
			}
		})
	}
}

// ==================== fetchFeed：正常拉取 + 去重 + Status ====================

// TestFetchFeed_MatchSubmitAndStatus 正常模式：命中规则提交 qB，并断言 Status 各字段
func TestFetchFeed_MatchSubmitAndStatus(t *testing.T) {
	torA := newStaticServer("torrent-content-a")
	defer torA.Close()
	torB := newStaticServer("torrent-content-b")
	defer torB.Close()

	feedSrv := newMockFeed(rssDoc(
		rssItemXML("Demo.S01E01.1080p.WEB.mkv", "", "g1", torA.URL), // 命中（enclosure）
		rssItemXML("Demo.S01E02.720p.CAM.mkv", "", "g2", torB.URL),  // 被 exclude 排除
		rssItemXML("Totally.Unrelated.mkv", "", "g3", torA.URL),     // 无规则命中
		rssItemXML("Demo.S01E03.2160p.WEB.mkv", torB.URL, "g4", ""), // 命中（用 link 当种子地址）
		rssItemXML("NoKey.Item.mkv", "", "", ""),                    // 没有 guid/link → 直接跳过
	))
	defer feedSrv.Close()
	qbm := newMockQB()
	defer qbm.Close()

	feed := models.RSSFeed{
		ID: "f-main", Name: "主源", URL: feedSrv.URL(), Enabled: true,
		Rules: []models.RSSRule{
			{ID: "r0", Name: "禁用规则", Enabled: false, Mode: "keyword", Include: "1080p", SavePath: "/never"},
			{ID: "r1", Name: "高清规则", Enabled: true, Mode: "keyword",
				Include: "1080p|2160p", Exclude: "CAM",
				SavePath: "/downloads/anime", Category: "anime", Tags: "auto,rss"},
		},
	}
	offFeed := models.RSSFeed{ID: "f-off", Name: "停用源", URL: "http://127.0.0.1:1/", Enabled: false}

	e := newTestEngine(t, models.RSSConfig{
		Enabled: false, Interval: 15,
		Feeds: []models.RSSFeed{feed, offFeed},
	}, qbm.newClient())

	// 预置非空 seen，避免进入首次快照模式
	e.seen[feed.ID] = map[string]bool{"primed": true}

	e.fetchFeed(feed)

	// qB 应收到 2 次 add（g1 走 enclosure，g4 走 link）
	adds := qbm.addList()
	if len(adds) != 2 {
		t.Fatalf("qB 收到 %d 次 add，期望 2 次：%+v", len(adds), adds)
	}
	wantTorrents := []string{"torrent-content-a", "torrent-content-b"}
	for i, a := range adds {
		if a.SavePath != "/downloads/anime" {
			t.Errorf("add[%d].SavePath = %q, 期望 /downloads/anime", i, a.SavePath)
		}
		if a.Category != "anime" {
			t.Errorf("add[%d].Category = %q", i, a.Category)
		}
		if a.Tags != "auto,rss" {
			t.Errorf("add[%d].Tags = %q", i, a.Tags)
		}
		// 源码刻意不设 skip_checking（交给 qB 自己做 hash check），
		// 且 5.x 用 stopped 取代 4.x 的 paused
		if a.SkipChecking != "" || a.Stopped != "false" {
			t.Errorf("add[%d] 固定参数不对：%+v", i, a)
		}
		if string(a.Torrent) != wantTorrents[i] {
			t.Errorf("add[%d].Torrent = %q, 期望 %q", i, a.Torrent, wantTorrents[i])
		}
	}

	// Status 各字段
	sts := e.Status()
	if len(sts) != 2 {
		t.Fatalf("Status 长度 = %d, 期望 2", len(sts))
	}
	st := sts[0]
	if st.FeedID != "f-main" || st.FeedName != "主源" || st.URL != feedSrv.URL() || !st.Enabled {
		t.Errorf("Status 基本字段不对：%+v", st)
	}
	if st.LastOK == nil || !*st.LastOK {
		t.Errorf("LastOK 应为 true，got %v", st.LastOK)
	}
	if st.LastError != "" {
		t.Errorf("LastError 应为空，got %q", st.LastError)
	}
	if st.Fetching {
		t.Error("拉取结束后 Fetching 应为 false")
	}
	if st.LastFetchAt.IsZero() || time.Since(st.LastFetchAt) > time.Minute {
		t.Errorf("LastFetchAt 不合理：%v", st.LastFetchAt)
	}
	if st.ItemCount != 5 {
		t.Errorf("ItemCount = %d, 期望 5", st.ItemCount)
	}
	if st.Matched != 2 || st.Downloaded != 2 || st.Failed != 0 {
		t.Errorf("Matched/Downloaded/Failed = %d/%d/%d, 期望 2/2/0", st.Matched, st.Downloaded, st.Failed)
	}
	if st.Snapshot {
		t.Error("预置 seen 后不应是快照模式")
	}
	if st.SeenCount != 5 {
		t.Errorf("SeenCount = %d, 期望 5（primed + 4 条有 key 的条目）", st.SeenCount)
	}
	// recent：4 条（无 key 的条目不入列），最新在前
	if len(st.RecentItems) != 4 {
		t.Fatalf("RecentItems 长度 = %d, 期望 4", len(st.RecentItems))
	}
	wantActions := []string{"downloaded", "skipped_no_rule", "skipped_no_rule", "downloaded"}
	for i, ri := range st.RecentItems {
		if ri.Action != wantActions[i] {
			t.Errorf("RecentItems[%d].Action = %q, 期望 %q", i, ri.Action, wantActions[i])
		}
		if ri.ProcessedAt.IsZero() {
			t.Errorf("RecentItems[%d].ProcessedAt 应有值", i)
		}
	}
	// 没被拉取过的订阅源：state 为 nil，字段全零
	off := sts[1]
	if off.FeedID != "f-off" || off.Enabled || off.LastOK != nil || off.LastError != "" ||
		off.ItemCount != 0 || off.SeenCount != 0 || off.RecentItems != nil || !off.LastFetchAt.IsZero() {
		t.Errorf("未拉取源的 Status 应全零：%+v", off)
	}

	// 第二次拉取：同一 URL 的条目已 seen，不重复提交
	e.fetchFeed(feed)
	if got := qbm.addCount(); got != 2 {
		t.Errorf("重复拉取后 add 次数 = %d, 期望仍为 2", got)
	}
	st = e.Status()[0]
	if st.Matched != 0 || st.Downloaded != 0 || st.Failed != 0 {
		t.Errorf("重复拉取不应再处理，got Matched/Downloaded/Failed=%d/%d/%d", st.Matched, st.Downloaded, st.Failed)
	}
	if st.Snapshot {
		t.Error("seen 非空后再次拉取不应是快照")
	}
	if st.LastOK == nil || !*st.LastOK {
		t.Errorf("第二次拉取 LastOK 应为 true")
	}
}

// TestFetchFeed_StoppedRuleSubmitsStopped 覆盖 5.x 新增的 stopped 表单字段：
// 规则勾了"添加后保持停止" → add 请求带 stopped=true；未勾则为 false（见上面主用例）
func TestFetchFeed_StoppedRuleSubmitsStopped(t *testing.T) {
	tor := newStaticServer("torrent-content")
	defer tor.Close()
	feedSrv := newMockFeed(rssDoc(
		rssItemXML("Paused.Show.S01E01.mkv", "", "g1", tor.URL),
	))
	defer feedSrv.Close()
	qbm := newMockQB()
	defer qbm.Close()

	feed := models.RSSFeed{
		ID: "f-stopped", Name: "停止源", URL: feedSrv.URL(), Enabled: true,
		Rules: []models.RSSRule{
			{ID: "r1", Name: "添加后停止", Enabled: true, Mode: "keyword",
				Include: "Paused", SavePath: "/dl", Stopped: true},
		},
	}
	e := newTestEngine(t, models.RSSConfig{Feeds: []models.RSSFeed{feed}}, qbm.newClient())
	e.seen[feed.ID] = map[string]bool{"primed": true}

	e.fetchFeed(feed)

	adds := qbm.addList()
	if len(adds) != 1 {
		t.Fatalf("qB 收到 %d 次 add，期望 1 次", len(adds))
	}
	if adds[0].Stopped != "true" {
		t.Errorf("勾选保持停止时 stopped 应为 true，got %q", adds[0].Stopped)
	}
	if adds[0].SkipChecking != "" {
		t.Errorf("源码不应设置 skip_checking，got %q", adds[0].SkipChecking)
	}
}

// TestFetchFeed_RexExRule 正则模式：命中与排除
func TestFetchFeed_RegexRule(t *testing.T) {
	tor := newStaticServer("torrent-content")
	defer tor.Close()
	feedSrv := newMockFeed(rssDoc(
		rssItemXML("Movie.2160p.REMUX.mkv", "", "g1", tor.URL), // 被正则排除
		rssItemXML("Movie.2160p.WEB-DL.mkv", "", "g2", tor.URL),
		rssItemXML("Movie.BDRip.mkv", "", "g3", tor.URL), // 不匹配 include
	))
	defer feedSrv.Close()
	qbm := newMockQB()
	defer qbm.Close()

	feed := models.RSSFeed{
		ID: "f-regex", Name: "正则源", URL: feedSrv.URL(), Enabled: true,
		Rules: []models.RSSRule{
			{ID: "r1", Name: "4K 规则", Enabled: true, Mode: "regex",
				Include: `\b2160p\b`, Exclude: `remux`,
				SavePath: "/4k", Category: "4k", Tags: "regex"},
		},
	}
	e := newTestEngine(t, models.RSSConfig{Feeds: []models.RSSFeed{feed}}, qbm.newClient())
	e.seen[feed.ID] = map[string]bool{"primed": true}

	e.fetchFeed(feed)

	adds := qbm.addList()
	if len(adds) != 1 {
		t.Fatalf("qB 收到 %d 次 add，期望 1 次", len(adds))
	}
	if string(adds[0].Torrent) != "torrent-content" {
		t.Errorf("种子内容 = %q", adds[0].Torrent)
	}
	st := e.Status()[0]
	if st.Matched != 1 || st.Downloaded != 1 || st.Failed != 0 {
		t.Errorf("Matched/Downloaded/Failed = %d/%d/%d, 期望 1/1/0", st.Matched, st.Downloaded, st.Failed)
	}
	if st.ItemCount != 3 {
		t.Errorf("ItemCount = %d, 期望 3", st.ItemCount)
	}
}

// TestFetchFeed_SnapshotFirstFetch 首次接入：只 mark seen 不提交
func TestFetchFeed_SnapshotFirstFetch(t *testing.T) {
	tor := newStaticServer("torrent-content")
	defer tor.Close()
	feedSrv := newMockFeed(rssDoc(
		rssItemXML("Show.S01E01.1080p.mkv", "", "g1", tor.URL),
		rssItemXML("Show.S01E02.1080p.mkv", "", "g2", tor.URL),
		rssItemXML("Show.S01E03.1080p.mkv", "", "g3", tor.URL),
	))
	defer feedSrv.Close()
	qbm := newMockQB()
	defer qbm.Close()

	feed := models.RSSFeed{
		ID: "f-snap", Name: "快照源", URL: feedSrv.URL(), Enabled: true,
		Rules: []models.RSSRule{
			{ID: "r1", Name: "规则", Enabled: true, Mode: "keyword", Include: "1080p",
				SavePath: "/dl", Category: "c", Tags: "t"},
		},
	}
	e := newTestEngine(t, models.RSSConfig{Feeds: []models.RSSFeed{feed}}, qbm.newClient())

	// 第一次拉取：快照
	e.fetchFeed(feed)
	if n := qbm.addCount(); n != 0 {
		t.Fatalf("快照模式不应提交，实际 %d 次", n)
	}
	st := e.Status()[0]
	if !st.Snapshot {
		t.Error("首次拉取应是快照模式")
	}
	if st.SeenCount != 3 {
		t.Errorf("SeenCount = %d, 期望 3", st.SeenCount)
	}
	if st.ItemCount != 3 {
		t.Errorf("ItemCount = %d, 期望 3", st.ItemCount)
	}
	if st.LastOK == nil || !*st.LastOK {
		t.Errorf("快照拉取 LastOK 应为 true")
	}
	if len(st.RecentItems) != 3 {
		t.Fatalf("RecentItems 长度 = %d, 期望 3", len(st.RecentItems))
	}
	for _, ri := range st.RecentItems {
		if ri.Action != "skipped_snapshot" {
			t.Errorf("快照条目 Action = %q, 期望 skipped_snapshot", ri.Action)
		}
	}
	// 快照后应立即落盘
	data, err := os.ReadFile(e.stateFile)
	if err != nil {
		t.Fatalf("快照后应写入状态文件：%v", err)
	}
	var p seenPersist
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatalf("状态文件解析失败：%v", err)
	}
	if len(p["f-snap"]) != 3 {
		t.Errorf("状态文件里 f-snap 应有 3 条 key，got %v", p["f-snap"])
	}

	// 第二次拉取：seen 已有内容，进入正常模式但所有条目都已见过 → 无处理
	e.fetchFeed(feed)
	if n := qbm.addCount(); n != 0 {
		t.Errorf("已 seen 的条目不应再提交，实际 %d 次", n)
	}
	st = e.Status()[0]
	if st.Snapshot {
		t.Error("seen 非空后不应再是快照")
	}
	if st.Matched != 0 || st.Downloaded != 0 {
		t.Errorf("已 seen 条目不应再处理，got %d/%d", st.Matched, st.Downloaded)
	}
}

// TestFetchFeed_ResetFeedForcesRescan ResetFeed：清空 seen 并强制重新扫描
func TestFetchFeed_ResetFeedForcesRescan(t *testing.T) {
	tor := newStaticServer("torrent-content")
	defer tor.Close()
	feedSrv := newMockFeed(rssDoc(
		rssItemXML("Show.1080p.mkv", "", "g1", tor.URL),
		rssItemXML("Other.720p.mkv", "", "g2", tor.URL),
	))
	defer feedSrv.Close()
	qbm := newMockQB()
	defer qbm.Close()

	feed := models.RSSFeed{
		ID: "f1", Name: "源一", URL: feedSrv.URL(), Enabled: true,
		Rules: []models.RSSRule{
			{ID: "r1", Name: "规则", Enabled: true, Mode: "keyword", Include: "1080p",
				SavePath: "/dl", Category: "c", Tags: "t"},
		},
	}
	e := newTestEngine(t, models.RSSConfig{Feeds: []models.RSSFeed{feed}}, qbm.newClient())

	// 第一次：快照
	e.fetchFeed(feed)
	if n := qbm.addCount(); n != 0 {
		t.Fatalf("快照模式不应提交，实际 %d 次", n)
	}
	if !e.Status()[0].Snapshot {
		t.Fatal("首次应是快照")
	}

	// 重置该源
	e.ResetFeed("f1")
	st := e.Status()[0]
	if st.SeenCount != 0 {
		t.Errorf("ResetFeed 后 SeenCount = %d, 期望 0", st.SeenCount)
	}
	if st.ItemCount != 0 || st.Matched != 0 || st.Downloaded != 0 || st.Failed != 0 ||
		st.Snapshot || st.LastOK != nil || st.LastError != "" {
		t.Errorf("ResetFeed 后运行态应复位：%+v", st)
	}
	if len(st.RecentItems) != 0 {
		t.Errorf("ResetFeed 后 RecentItems 应清空，got %d", len(st.RecentItems))
	}
	if !e.isFreshFeed("f1") {
		t.Error("ResetFeed 应清空该 feed 的 seen")
	}
	s := e.getOrCreateState("f1")
	if !s.forceRescan {
		t.Error("ResetFeed 应置 forceRescan")
	}
	// 状态文件同步
	data, err := os.ReadFile(e.stateFile)
	if err != nil {
		t.Fatalf("ResetFeed 应落盘：%v", err)
	}
	var p seenPersist
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatalf("状态文件解析失败：%v", err)
	}
	if _, ok := p["f1"]; ok {
		t.Errorf("状态文件不应再含 f1：%v", p)
	}

	// 再次拉取：forceRescan 跳过快照 → 按规则正常提交
	e.fetchFeed(feed)
	if n := qbm.addCount(); n != 1 {
		t.Fatalf("重置后应提交 1 次，实际 %d 次", n)
	}
	st = e.Status()[0]
	if st.Snapshot {
		t.Error("forceRescan 后不应再进入快照")
	}
	if st.Matched != 1 || st.Downloaded != 1 {
		t.Errorf("Matched/Downloaded = %d/%d, 期望 1/1", st.Matched, st.Downloaded)
	}
	if st.SeenCount != 2 {
		t.Errorf("SeenCount = %d, 期望 2", st.SeenCount)
	}
	if s.forceRescan {
		t.Error("forceRescan 应被本次拉取消费掉")
	}
}

// TestResetAll_ClearsEverything 覆盖 ResetAll
func TestResetAll_ClearsEverything(t *testing.T) {
	e := newTestEngine(t, models.RSSConfig{}, nil)
	ok := true
	for _, fid := range []string{"f1", "f2"} {
		e.seen[fid] = map[string]bool{"g1": true, "g2": true}
		s := e.getOrCreateState(fid)
		s.downloaded = 3
		s.failed = 1
		s.itemCount = 5
		s.matched = 3
		s.snapshot = true
		s.lastOK = &ok
		s.lastError = "上次出错"
		s.pushRecent(RecentItem{Title: "t", Action: "downloaded"})
	}

	e.ResetAll()

	for _, fid := range []string{"f1", "f2"} {
		if !e.isFreshFeed(fid) {
			t.Errorf("%s 的 seen 应被清空", fid)
		}
		s := e.getOrCreateState(fid)
		if s.downloaded != 0 || s.failed != 0 || s.itemCount != 0 || s.matched != 0 ||
			s.snapshot || s.lastOK != nil || s.lastError != "" {
			t.Errorf("%s 运行态未复位：%+v", fid, s)
		}
		if !s.forceRescan {
			t.Errorf("%s 应置 forceRescan", fid)
		}
		if n := len(s.collectRecent()); n != 0 {
			t.Errorf("%s 的 recent 应清空，got %d", fid, n)
		}
	}
	data, err := os.ReadFile(e.stateFile)
	if err != nil {
		t.Fatalf("ResetAll 应落盘：%v", err)
	}
	var p seenPersist
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatalf("状态文件解析失败：%v", err)
	}
	if len(p) != 0 {
		t.Errorf("ResetAll 后状态文件应为空，got %v", p)
	}
}

// ==================== 失败分支 ====================

// TestFetchFeed_HTTPStatusError 源返回 500 → LastOK=false
func TestFetchFeed_HTTPStatusError(t *testing.T) {
	feedSrv := newMockFeed("server error")
	feedSrv.set("server error", http.StatusInternalServerError)
	defer feedSrv.Close()
	qbm := newMockQB()
	defer qbm.Close()

	feed := models.RSSFeed{ID: "f-err", Name: "错误源", URL: feedSrv.URL(), Enabled: true}
	e := newTestEngine(t, models.RSSConfig{Feeds: []models.RSSFeed{feed}}, qbm.newClient())

	e.fetchFeed(feed)

	st := e.Status()[0]
	if st.LastOK == nil || *st.LastOK {
		t.Errorf("LastOK 应为 false，got %v", st.LastOK)
	}
	if st.LastError != "status 500" {
		t.Errorf("LastError = %q, 期望 status 500", st.LastError)
	}
	if st.Fetching {
		t.Error("失败后 Fetching 应为 false")
	}
	if st.ItemCount != 0 {
		t.Errorf("失败时 ItemCount = %d", st.ItemCount)
	}
	if st.Snapshot {
		t.Error("失败时不应是快照")
	}
	if n := qbm.addCount(); n != 0 {
		t.Errorf("失败时不应提交，got %d", n)
	}
}

// TestFetchFeed_InvalidXML 源返回非法 XML → 解析失败分支
func TestFetchFeed_InvalidXML(t *testing.T) {
	feedSrv := newMockFeed("这不是合法的 XML <rss")
	defer feedSrv.Close()
	qbm := newMockQB()
	defer qbm.Close()

	feed := models.RSSFeed{ID: "f-badxml", Name: "坏源", URL: feedSrv.URL(), Enabled: true}
	e := newTestEngine(t, models.RSSConfig{Feeds: []models.RSSFeed{feed}}, qbm.newClient())

	e.fetchFeed(feed)

	st := e.Status()[0]
	if st.LastOK == nil || *st.LastOK {
		t.Errorf("LastOK 应为 false，got %v", st.LastOK)
	}
	if st.LastError == "" {
		t.Error("解析失败应记录 LastError")
	}
	if st.ItemCount != 0 || st.Matched != 0 {
		t.Errorf("解析失败不应有条目统计：%+v", st)
	}
}

// TestFetchFeed_TorrentDownloadFail 让它滚回 seen，下次重试
func TestFetchFeed_TorrentDownloadFailRollsBackSeen(t *testing.T) {
	badTor := newMockFeed("gone")
	badTor.set("gone", http.StatusNotFound)
	defer badTor.Close()
	feedSrv := newMockFeed(rssDoc(rssItemXML("Show.1080p.mkv", "", "g1", badTor.URL())))
	defer feedSrv.Close()
	qbm := newMockQB()
	defer qbm.Close()

	feed := models.RSSFeed{
		ID: "f-fail", Name: "失败源", URL: feedSrv.URL(), Enabled: true,
		Rules: []models.RSSRule{
			{ID: "r1", Name: "规则", Enabled: true, Mode: "keyword", Include: "1080p",
				SavePath: "/dl", Category: "c", Tags: "t"},
		},
	}
	e := newTestEngine(t, models.RSSConfig{Feeds: []models.RSSFeed{feed}}, qbm.newClient())
	e.seen[feed.ID] = map[string]bool{"primed": true}

	e.fetchFeed(feed)

	st := e.Status()[0]
	if st.Failed != 1 {
		t.Errorf("Failed = %d, 期望 1", st.Failed)
	}
	if st.Matched != 0 || st.Downloaded != 0 {
		t.Errorf("提交失败不应计入 Matched/Downloaded，got %d/%d", st.Matched, st.Downloaded)
	}
	if len(st.RecentItems) != 1 {
		t.Fatalf("RecentItems 长度 = %d, 期望 1", len(st.RecentItems))
	}
	ri := st.RecentItems[0]
	if ri.Action != "failed" || !strings.Contains(ri.Error, "下载 torrent 失败") {
		t.Errorf("失败记录不对：%+v", ri)
	}
	if ri.RuleName != "规则" {
		t.Errorf("失败记录应带规则名，got %q", ri.RuleName)
	}
	// 失败要从 seen 里滚回去，下一轮重试
	if e.seen[feed.ID]["g1"] {
		t.Error("提交失败应从 seen 回滚")
	}
	// 再拉一次：还会重试，仍然失败
	e.fetchFeed(feed)
	if got := badTor.hitCount(); got != 2 {
		t.Errorf("种子下载应重试，hits = %d, 期望 2", got)
	}
	if e.seen[feed.ID]["g1"] {
		t.Error("重试仍失败时 seen 不应保留")
	}
}

// TestFetchFeed_AddQBFail qB 连不上 → AddTorrent 失败同样回滚 seen
func TestFetchFeed_AddQBFail(t *testing.T) {
	tor := newStaticServer("torrent-content")
	defer tor.Close()
	feedSrv := newMockFeed(rssDoc(rssItemXML("Show.1080p.mkv", "", "g1", tor.URL)))
	defer feedSrv.Close()

	feed := models.RSSFeed{
		ID: "f-qb", Name: "qB 失败源", URL: feedSrv.URL(), Enabled: true,
		Rules: []models.RSSRule{
			{ID: "r1", Name: "规则", Enabled: true, Mode: "keyword", Include: "1080p",
				SavePath: "/dl", Category: "c", Tags: "t"},
		},
	}
	e := newTestEngine(t, models.RSSConfig{Feeds: []models.RSSFeed{feed}},
		qb.New("http://127.0.0.1:1", "u", "p", "k"))
	e.seen[feed.ID] = map[string]bool{"primed": true}

	e.fetchFeed(feed)

	st := e.Status()[0]
	if st.Failed != 1 {
		t.Errorf("Failed = %d, 期望 1", st.Failed)
	}
	if len(st.RecentItems) != 1 || !strings.Contains(st.RecentItems[0].Error, "qBittorrent") {
		t.Errorf("失败记录不对：%+v", st.RecentItems)
	}
	if e.seen[feed.ID]["g1"] {
		t.Error("qB 提交失败应从 seen 回滚")
	}
}

// TestFetchFeed_UploadLimitPassedToQB 规则里的 uploadLimit 透传给 qB：
// qb.Client.AddTorrent 添加成功后会异步调用 setUploadLimit（源码固定 3 秒后触发），
// 这里用轮询等待而不是固定 sleep。
func TestFetchFeed_UploadLimitPassedToQB(t *testing.T) {
	tor := newStaticServer("torrent-content")
	defer tor.Close()
	feedSrv := newMockFeed(rssDoc(rssItemXML("Show.1080p.mkv", "", "g1", tor.URL)))
	defer feedSrv.Close()
	qbm := newMockQB()
	defer qbm.Close()

	feed := models.RSSFeed{
		ID: "f-limit", Name: "限速源", URL: feedSrv.URL(), Enabled: true,
		Rules: []models.RSSRule{
			{ID: "r1", Name: "规则", Enabled: true, Mode: "keyword", Include: "1080p",
				SavePath: "/dl", Category: "c", Tags: "t", UploadLimit: 128},
		},
	}
	e := newTestEngine(t, models.RSSConfig{Feeds: []models.RSSFeed{feed}}, qbm.newClient())
	e.seen[feed.ID] = map[string]bool{"primed": true}

	e.fetchFeed(feed)
	if n := qbm.addCount(); n != 1 {
		t.Fatalf("应提交 1 次，实际 %d", n)
	}
	waitFor(t, 6*time.Second, func() bool {
		lims := qbm.limitList()
		return len(lims) > 0 && lims[0] == 128*1024
	}, "qB 收到 setUploadLimit(limit=131072)")
}

// ==================== ForceFetch / fetchAll ====================

// TestForceFetch_TriggersFetchAndSkipsDisabledFeed 覆盖 ForceFetch 与 fetchAll 的禁用分支
func TestForceFetch_TriggersFetchAndSkipsDisabledFeed(t *testing.T) {
	tor := newStaticServer("torrent-content")
	defer tor.Close()
	feedSrv := newMockFeed(rssDoc(rssItemXML("Show.1080p.mkv", "", "g1", tor.URL)))
	defer feedSrv.Close()
	offSrv := newMockFeed(rssDoc(rssItemXML("Other.1080p.mkv", "", "x1", tor.URL)))
	defer offSrv.Close()
	qbm := newMockQB()
	defer qbm.Close()

	onFeed := models.RSSFeed{
		ID: "f-on", Name: "启用源", URL: feedSrv.URL(), Enabled: true,
		Rules: []models.RSSRule{
			{ID: "r1", Name: "规则", Enabled: true, Mode: "keyword", Include: "1080p",
				SavePath: "/dl", Category: "c", Tags: "t"},
		},
	}
	offFeed := models.RSSFeed{ID: "f-off", Name: "禁用源", URL: offSrv.URL(), Enabled: false}

	e := newTestEngine(t, models.RSSConfig{
		Enabled: false, Feeds: []models.RSSFeed{onFeed, offFeed},
	}, qbm.newClient())
	// 预置 seen，直接进入正常模式
	e.seen[onFeed.ID] = map[string]bool{"primed": true}

	e.ForceFetch()

	waitFor(t, 3*time.Second, func() bool { return qbm.addCount() >= 1 }, "ForceFetch 后 qB 收到 add")
	if n := offSrv.hitCount(); n != 0 {
		t.Errorf("禁用的订阅源不应被拉取，hits = %d", n)
	}
	waitFor(t, 3*time.Second, func() bool {
		st := e.Status()
		return len(st) == 2 && st[0].LastOK != nil && *st[0].LastOK
	}, "ForceFetch 拉取完成")
}

// ==================== 生命周期 ====================

// TestSetClient 覆盖 SetClient 热替换
func TestSetClient(t *testing.T) {
	qbm := newMockQB()
	defer qbm.Close()
	e := newTestEngine(t, models.RSSConfig{}, qbm.newClient())
	old := e.client
	next := qbm.newClient()
	if next == old {
		t.Fatal("测试前置：两次 newClient 应是不同指针")
	}
	e.SetClient(next)
	if e.client != next {
		t.Errorf("SetClient 后应替换为新 client")
	}
}

// TestStartStop_Lifecycle 覆盖 Start / runPoller / Stop
// Interval=0 → 走默认 15 分钟分支
func TestStartStop_Lifecycle(t *testing.T) {
	feedSrv := newMockFeed(rssDoc(rssItemXML("Show.1080p.mkv", "", "g1", "")))
	defer feedSrv.Close()
	qbm := newMockQB()
	defer qbm.Close()

	feed := models.RSSFeed{ID: "f1", Name: "源", URL: feedSrv.URL(), Enabled: true}
	e := newTestEngine(t, models.RSSConfig{
		Enabled: true, Interval: 0, Feeds: []models.RSSFeed{feed},
	}, qbm.newClient())

	stop := stopEngine(e)
	t.Cleanup(stop)

	e.Start()
	if e.fetchStop == nil || e.saveStop == nil {
		t.Fatal("Start 应启动轮询协程")
	}
	// 启动时立即拉一次
	waitFor(t, 3*time.Second, func() bool {
		st := e.Status()
		return len(st) == 1 && st[0].LastOK != nil
	}, "启动后的立即拉取")
	if n := feedSrv.hitCount(); n < 1 {
		t.Errorf("启动后应至少拉取 1 次，hits = %d", n)
	}

	stop()
	if _, err := os.Stat(e.stateFile); err != nil {
		t.Errorf("Stop 应落盘 seen 状态：%v", err)
	}
}

// TestRunPoller_Disabled 未启用时 Start/Reload 都不应启动协程
func TestRunPoller_Disabled(t *testing.T) {
	feedSrv := newMockFeed(rssDoc(rssItemXML("Show.1080p.mkv", "", "g1", "")))
	defer feedSrv.Close()

	feed := models.RSSFeed{ID: "f1", Name: "源", URL: feedSrv.URL(), Enabled: true}
	e := newTestEngine(t, models.RSSConfig{Enabled: false, Feeds: []models.RSSFeed{feed}}, nil)

	e.Start()
	if e.fetchStop != nil || e.saveStop != nil {
		t.Fatal("未启用时 Start 不应启动轮询协程")
	}
	if n := feedSrv.hitCount(); n != 0 {
		t.Errorf("未启用时不应拉取，hits = %d", n)
	}

	// 停用状态下的 Reload：既不替换 client，也不启动协程
	e.Reload(nil)
	if e.fetchStop != nil || e.saveStop != nil {
		t.Error("停用时 Reload 不应启动轮询协程")
	}
	if e.client != nil {
		t.Error("Reload(nil) 不应替换 client")
	}
}

// TestReload_StartsPoller 覆盖 Reload 的启用分支（首次启动，无旧协程）
//
// 注意：Reload 里 `close(e.fetchStop)/e.fetchStop = nil`（以及 saveStop 那组）
// 无法在 -race 下覆盖——源码中轮询协程无同步地读 e.fetchStop/e.saveStop，
// 而 Reload 直接写这两个字段，属于源码自带的 data race（已实测会被 -race 报出）。
// 只有"此前从未启动过轮询"时调 Reload 才是安全的，因此这里只测首次启动。
func TestReload_StartsPoller(t *testing.T) {
	feedSrv := newMockFeed(rssDoc(rssItemXML("Show.1080p.mkv", "", "g1", "")))
	defer feedSrv.Close()
	qbm := newMockQB()
	defer qbm.Close()

	feed := models.RSSFeed{ID: "f1", Name: "源", URL: feedSrv.URL(), Enabled: true}
	e := newTestEngine(t, models.RSSConfig{
		Enabled: true, Interval: 1, Feeds: []models.RSSFeed{feed},
	}, nil)

	stop := stopEngine(e)
	t.Cleanup(stop)

	cli := qbm.newClient()
	e.Reload(cli)
	if e.client != cli {
		t.Error("Reload 应替换 client")
	}
	if e.fetchStop == nil || e.saveStop == nil {
		t.Fatal("启用时 Reload 应启动轮询协程")
	}
	waitFor(t, 3*time.Second, func() bool {
		st := e.Status()
		return len(st) == 1 && st[0].LastOK != nil
	}, "Reload 后的立即拉取")

	stop()
}

// TestPollerGoroutinesExitViaStop 直接关闭 e.stop，让两个后台协程走
// `case <-e.stop` 分支退出（Stop() 会同时关 fetchStop/saveStop，两者会随机选）
func TestPollerGoroutinesExitViaStop(t *testing.T) {
	feedSrv := newMockFeed(rssDoc(rssItemXML("Show.1080p.mkv", "", "g1", "")))
	defer feedSrv.Close()
	qbm := newMockQB()
	defer qbm.Close()

	feed := models.RSSFeed{ID: "f1", Name: "源", URL: feedSrv.URL(), Enabled: true}
	e := newTestEngine(t, models.RSSConfig{
		Enabled: true, Interval: 1, Feeds: []models.RSSFeed{feed},
	}, qbm.newClient())

	e.Start()
	waitFor(t, 3*time.Second, func() bool {
		st := e.Status()
		return len(st) == 1 && st[0].LastOK != nil
	}, "启动后的立即拉取")

	// 先落盘一次，记录 mtime
	e.saveSeen()
	before, err := os.Stat(e.stateFile)
	if err != nil {
		t.Fatalf("状态文件应存在：%v", err)
	}

	// 只关 e.stop：saveStop 协程走 `case <-e.stop` → saveSeen 后退出
	close(e.stop)
	waitFor(t, 3*time.Second, func() bool {
		after, err := os.Stat(e.stateFile)
		return err == nil && after.ModTime().After(before.ModTime())
	}, "e.stop 关闭后 saveStop 协程应再落盘一次")
}

// ==================== loadSeen / saveSeen 边界 ====================

// TestSaveSeen_WriteFailure 状态文件的 .tmp 被目录占位 → 写失败只告警
func TestSaveSeen_WriteFailure(t *testing.T) {
	e := newTestEngine(t, models.RSSConfig{}, nil)
	e.stateFile = filepath.Join(t.TempDir(), "rss_seen.json")
	if err := os.MkdirAll(e.stateFile+".tmp", 0o755); err != nil {
		t.Fatalf("准备 .tmp 目录失败：%v", err)
	}
	e.seen["f1"] = map[string]bool{"g1": true}
	e.saveSeen() // 只告警不 panic
	if _, err := os.Stat(e.stateFile); !os.IsNotExist(err) {
		t.Errorf("写失败时不应生成状态文件，stat err = %v", err)
	}
}

// TestLoadSeen_ReadFailure 状态路径是目录 → 读失败（非 not-exist）只告警
func TestLoadSeen_ReadFailure(t *testing.T) {
	e := newTestEngine(t, models.RSSConfig{}, nil)
	e.stateFile = t.TempDir() // 目录
	e.seen = make(map[string]map[string]bool)
	e.loadSeen() // 只告警不 panic
	if len(e.seen) != 0 {
		t.Errorf("读失败时 seen 应保持为空，got %v", e.seen)
	}
}
