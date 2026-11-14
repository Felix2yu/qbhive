package web

// 归档接口测试。账本自身的过滤/排序/分页语义在 internal/archive/store_test.go 里覆盖，
// 这里只验证 web 层该做的事：查询参数有没有真的传进 Query、鉴权是否挡住写操作、
// 逃生口是否只动账本不动 qB、手动退休是否绕过 dryRun 且 deleteFiles 恒 false。

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Felix2yu/qbhive/internal/archive"
	"github.com/Felix2yu/qbhive/internal/models"
	"github.com/gin-gonic/gin"
)

// =============== 夹具 ===============

type archiveEnv struct {
	srv   *Server
	store *archive.Store
	qb    *mockQB
	dir   string
}

func newArchiveEnv(t *testing.T, token, torrentsBody string) *archiveEnv {
	t.Helper()
	m := newMockQB(t)
	if torrentsBody != "" {
		m.torrentsBody = torrentsBody
	}
	dir := t.TempDir()
	s := setupTestServerFull(t, dir, m.URL(), token)
	store := archive.NewStore(filepath.Join(dir, "archive.jsonl"))
	s.SetArchive(store, archive.NewRetirer(s.cfg, s.qbClient, store, s.fileMgr))
	return &archiveEnv{srv: s, store: store, qb: m, dir: dir}
}

func (e *archiveEnv) seed(t *testing.T, recs ...archive.Record) {
	t.Helper()
	for _, r := range recs {
		if err := e.store.Append(r); err != nil {
			t.Fatalf("写入账本失败：%v", err)
		}
	}
}

// archiveRouter 只挂归档三条路由，但带上与 Start 相同的鉴权中间件
func archiveRouter(s *Server) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(s.authMiddleware())
	api := r.Group("/api")
	api.GET("/archive", s.archiveSearch)
	api.POST("/archive/retire", s.archiveRetire)
	api.DELETE("/archive/:hash", s.archiveDelete)
	return r
}

func archRecord(hash, name, category, tags, savePath string, size, completedOn, retiredAt int64) archive.Record {
	return archive.Record{
		Hash: hash, Name: name, Category: category, Tags: tags,
		SavePath: savePath, ContentPath: savePath + "/" + name,
		Tracker: "http://tr.example/announce",
		Magnet:  "magnet:?xt=urn:btih:" + hash,
		Size:    size, Uploaded: size * 2, AddedOn: completedOn - 100,
		CompletedOn: completedOn, Ratio: 2, RetiredAt: retiredAt,
		Reason: archive.ReasonLostFiles,
	}
}

// seedThree 三条记录，每个过滤维度都能单独区分出来；h3 是已建档未退休的候选。
// size 刻意与 name/completedOn 不同序：否则「sort 参数没接上」也会因为
// 默认 completedOn 排序恰好同序而测不出来。
func (e *archiveEnv) seedThree(t *testing.T) {
	e.seed(t,
		archRecord("h1", "Alpha.mkv", "电影", "转码,4k", "/media/alpha", 300, 1000, 1100),
		archRecord("h2", "Beta.mkv", "剧集", "4k", "/media/beta", 100, 2000, 2100),
		archRecord("h3", "Gamma.mkv", "电影", "转码", "/data/gamma", 200, 3000, 0),
	)
}

type archivePage struct {
	Records []archive.Record `json:"records"`
	Total   int              `json:"total"`
}

func searchArchive(t *testing.T, r *gin.Engine, query string) archivePage {
	t.Helper()
	w := serveJSON(t, r, "GET", "/api/archive?"+query, "")
	if w.Code != 200 {
		t.Fatalf("检索应返回 200，got %d body=%s", w.Code, w.Body.String())
	}
	resp := decodeAPI(t, w)
	if !resp.Success {
		t.Fatalf("检索应 success，got message=%q", resp.Message)
	}
	var page archivePage
	resp.bindData(t, &page)
	return page
}

func hashesOf(page archivePage) string {
	out := make([]string, 0, len(page.Records))
	for _, r := range page.Records {
		out = append(out, r.Hash)
	}
	return strings.Join(out, ",")
}

// mockDeletes 取出 mock 收到的 /torrents/delete 请求参数
func mockDeletes(m *mockQB) []url.Values {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []url.Values
	for _, a := range m.actionReq {
		if !strings.HasPrefix(a, "/api/v2/torrents/delete?") {
			continue
		}
		v, err := url.ParseQuery(strings.TrimPrefix(a, "/api/v2/torrents/delete?"))
		if err != nil {
			continue
		}
		out = append(out, v)
	}
	return out
}

func archBoolPtr(b bool) *bool { return &b }

const twoTasksJSON = `[{"hash":"aaa","name":"A.mkv","state":"stoppedUP","progress":1,"size":100,` +
	`"completion_on":1000,"added_on":900,"save_path":"/media/a","category":"电影","tags":"转码","tracker":"http://tr/announce"},` +
	`{"hash":"bbb","name":"B.mkv","state":"missingFiles","progress":1,"size":200,` +
	`"completion_on":2000,"added_on":1900,"save_path":"/media/b","category":"剧集","tags":"","tracker":""}]`

// =============== GET /api/archive：查询参数是否真的落到 Query ===============

func TestArchiveSearch_QueryParams(t *testing.T) {
	e := newArchiveEnv(t, "", "")
	e.seedThree(t)
	r := archiveRouter(e.srv)

	// 未指定排序时默认 completedOn 降序，即 h3,h2,h1
	cases := []struct {
		name  string
		query url.Values
		want  string
	}{
		{"无参数返回全部", url.Values{}, "h3,h2,h1"},
		{"q 匹配名称且大小写不敏感", url.Values{"q": {"aLpHa"}}, "h1"},
		{"q 匹配保存路径", url.Values{"q": {"/DATA"}}, "h3"},
		{"q 无命中", url.Values{"q": {"不存在"}}, ""},
		{"category 精确匹配", url.Values{"category": {"电影"}}, "h3,h1"},
		{"category 不做子串匹配", url.Values{"category": {"电"}}, ""},
		{"tag 命中标签串中任一项", url.Values{"tag": {"4K"}}, "h2,h1"},
		{"tag 中文", url.Values{"tag": {"转码"}}, "h3,h1"},
		{"from 按完成时间下界", url.Values{"from": {"2000"}}, "h3,h2"},
		{"to 按完成时间上界", url.Values{"to": {"2000"}}, "h2,h1"},
		{"from+to 闭区间", url.Values{"from": {"2000"}, "to": {"2000"}}, "h2"},
		{"pending=1 只看候选", url.Values{"pending": {"1"}}, "h3"},
		{"pending=true 同义", url.Values{"pending": {"true"}}, "h3"},
		{"pending=0 不过滤", url.Values{"pending": {"0"}}, "h3,h2,h1"},
		{"sort=name 升序", url.Values{"sort": {"name"}, "order": {"asc"}}, "h1,h2,h3"},
		{"sort=name 降序", url.Values{"sort": {"name"}, "order": {"desc"}}, "h3,h2,h1"},
		{"sort=size 升序", url.Values{"sort": {"size"}, "order": {"asc"}}, "h2,h3,h1"},
		{"sort=size 降序", url.Values{"sort": {"size"}, "order": {"desc"}}, "h1,h3,h2"},
		{"sort=addedOn 升序", url.Values{"sort": {"addedOn"}, "order": {"asc"}}, "h1,h2,h3"},
		{"sort=retiredAt 升序", url.Values{"sort": {"retiredAt"}, "order": {"asc"}}, "h3,h1,h2"},
		{"sort=completedOn 显式降序", url.Values{"sort": {"completedOn"}, "order": {"desc"}}, "h3,h2,h1"},
		{"offset+limit 翻页", url.Values{"sort": {"name"}, "order": {"asc"}, "offset": {"1"}, "limit": {"1"}}, "h2"},
		{"非法 limit/offset 走默认值", url.Values{"limit": {"abc"}, "offset": {"abc"}}, "h3,h2,h1"},
		{"非法 from/to 视为不限", url.Values{"from": {"abc"}, "to": {"abc"}}, "h3,h2,h1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page := searchArchive(t, r, tc.query.Encode())
			if got := hashesOf(page); got != tc.want {
				t.Errorf("want %q got %q", tc.want, got)
			}
		})
	}
}

func TestArchiveSearch_TotalIsMatchCountNotPageSize(t *testing.T) {
	e := newArchiveEnv(t, "", "")
	e.seedThree(t)
	page := searchArchive(t, archiveRouter(e.srv), "limit=1&offset=0")
	if page.Total != 3 {
		t.Errorf("total 应是命中总数 3，got %d", page.Total)
	}
	if len(page.Records) != 1 {
		t.Errorf("本页应只有 1 条，got %d", len(page.Records))
	}
}

func TestArchiveSearch_EmptyIsArrayNotNull(t *testing.T) {
	e := newArchiveEnv(t, "", "")
	w := serveJSON(t, archiveRouter(e.srv), "GET", "/api/archive", "")
	var resp struct {
		Data struct {
			Records json.RawMessage `json:"records"`
			Total   int             `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if string(resp.Data.Records) != "[]" {
		t.Errorf("空账本的 records 必须是 []（前端会读 .length），got %s", resp.Data.Records)
	}
	if resp.Data.Total != 0 {
		t.Errorf("total 应为 0，got %d", resp.Data.Total)
	}
}

func TestArchiveSearch_RecordCarriesMagnetAndPaths(t *testing.T) {
	e := newArchiveEnv(t, "", "")
	e.seed(t, archRecord("h1", "Alpha.mkv", "电影", "转码", "/media/alpha", 100, 1000, 1100))
	page := searchArchive(t, archiveRouter(e.srv), "")
	if len(page.Records) != 1 {
		t.Fatalf("应有 1 条，got %d", len(page.Records))
	}
	r := page.Records[0]
	if r.Magnet != "magnet:?xt=urn:btih:h1" {
		t.Errorf("磁链应原样下发供前端复制，got %q", r.Magnet)
	}
	if r.SavePath != "/media/alpha" || r.ContentPath != "/media/alpha/Alpha.mkv" {
		t.Errorf("路径字段丢失：%+v", r)
	}
	if r.RetiredAt != 1100 || r.Reason != archive.ReasonLostFiles {
		t.Errorf("退休时刻/理由丢失：%+v", r)
	}
}

func TestArchiveSearch_ModuleNotLoaded(t *testing.T) {
	s := setupTestServerFull(t, t.TempDir(), "", "")
	w := serveJSON(t, archiveRouter(s), "GET", "/api/archive", "")
	if w.Code != 500 {
		t.Fatalf("未注入账本应返回 500，got %d", w.Code)
	}
	if resp := decodeAPI(t, w); resp.Success || resp.Message == "" {
		t.Errorf("应 success=false 且带说明，got %+v", resp)
	}
}

// =============== DELETE /api/archive/:hash：逃生口 ===============

func TestArchiveDelete_RemovesRecordAndPersists(t *testing.T) {
	e := newArchiveEnv(t, "", "")
	e.seedThree(t)
	r := archiveRouter(e.srv)

	w := serveJSON(t, r, "DELETE", "/api/archive/h1", "")
	if w.Code != 200 {
		t.Fatalf("删除应返回 200，got %d body=%s", w.Code, w.Body.String())
	}
	if resp := decodeAPI(t, w); !resp.Success {
		t.Fatalf("应 success，got %+v", resp)
	}
	if _, ok := e.store.Get("h1"); ok {
		t.Error("h1 应已从账本移除")
	}
	if e.store.Len() != 2 {
		t.Errorf("应剩 2 条，got %d", e.store.Len())
	}
	// 落盘：重启后守卫读到的账本里也不该再有它
	reloaded := archive.NewStore(filepath.Join(e.dir, "archive.jsonl"))
	if _, ok := reloaded.Get("h1"); ok {
		t.Error("移除必须落盘，否则重启后守卫又开始拦")
	}
	// 逃生口只动账本：不碰 qB 的任务，也不碰磁盘文件
	if len(mockDeletes(e.qb)) != 0 {
		t.Errorf("逃生口不该向 qB 发删除请求，got %v", mockDeletes(e.qb))
	}
	if len(e.qb.torrentsRequests()) != 0 {
		t.Errorf("逃生口不该拉 qB 任务表，got %v", e.qb.torrentsRequests())
	}
}

func TestArchiveDelete_NotFound(t *testing.T) {
	e := newArchiveEnv(t, "", "")
	e.seedThree(t)
	w := serveJSON(t, archiveRouter(e.srv), "DELETE", "/api/archive/nope", "")
	if w.Code != 400 {
		t.Fatalf("不存在的记录应返回 400，got %d body=%s", w.Code, w.Body.String())
	}
	if resp := decodeAPI(t, w); resp.Success || resp.Message == "" {
		t.Errorf("应带错误说明，got %+v", resp)
	}
	if e.store.Len() != 3 {
		t.Errorf("失败的删除不该动账本，got %d 条", e.store.Len())
	}
}

func TestArchiveDelete_BlankHash(t *testing.T) {
	e := newArchiveEnv(t, "", "")
	w := serveJSON(t, archiveRouter(e.srv), "DELETE", "/api/archive/%20", "")
	if w.Code != 400 {
		t.Fatalf("空白 hash 应返回 400，got %d body=%s", w.Code, w.Body.String())
	}
	if resp := decodeAPI(t, w); resp.Success || !strings.Contains(resp.Message, "hash") {
		t.Errorf("应提示缺少 hash，got %+v", resp)
	}
}

func TestArchiveDelete_ModuleNotLoaded(t *testing.T) {
	s := setupTestServerFull(t, t.TempDir(), "", "")
	w := serveJSON(t, archiveRouter(s), "DELETE", "/api/archive/h1", "")
	if w.Code != 500 {
		t.Fatalf("未注入账本应返回 500，got %d", w.Code)
	}
}

// =============== POST /api/archive/retire：手动退休 ===============

func TestArchiveRetire_BypassesDryRunAndKeepsFiles(t *testing.T) {
	e := newArchiveEnv(t, "", twoTasksJSON)
	cfg := e.srv.cfg.Get()
	cfg.Archive.Enabled = true
	cfg.Archive.DryRun = archBoolPtr(true) // 手动退休必须绕过它
	e.srv.cfg.Set(cfg)

	w := serveJSON(t, archiveRouter(e.srv), "POST", "/api/archive/retire", `{"hashes":["aaa","bbb"]}`)
	if w.Code != 200 {
		t.Fatalf("手动退休应返回 200，got %d body=%s", w.Code, w.Body.String())
	}
	var data struct {
		Retired int `json:"retired"`
	}
	decodeAPI(t, w).bindData(t, &data)
	if data.Retired != 2 {
		t.Errorf("应退休 2 条，got %d", data.Retired)
	}

	dels := mockDeletes(e.qb)
	if len(dels) != 1 {
		t.Fatalf("应只发一次批量删除请求，got %d 次", len(dels))
	}
	if dels[0].Get("deleteFiles") != "false" {
		t.Errorf("deleteFiles 必须恒 false（磁盘文件保留），got %q", dels[0].Get("deleteFiles"))
	}
	if got := strings.Split(dels[0].Get("hashes"), "|"); len(got) != 2 {
		t.Errorf("两条 hash 应在同一批里，got %q", dels[0].Get("hashes"))
	}
	for _, h := range []string{"aaa", "bbb"} {
		rec, ok := e.store.Get(h)
		if !ok {
			t.Fatalf("%s 应已建档", h)
		}
		if rec.RetiredAt <= 0 {
			t.Errorf("%s 删成功后应填退休时刻，got %d", h, rec.RetiredAt)
		}
		if rec.Reason != archive.ReasonManual {
			t.Errorf("%s 退休理由应为 manual，got %q", h, rec.Reason)
		}
		if rec.Magnet == "" || rec.SavePath == "" {
			t.Errorf("%s 记录缺磁链或路径：%+v", h, rec)
		}
	}
}

func TestArchiveRetire_SkipsHashNotInQB(t *testing.T) {
	e := newArchiveEnv(t, "", twoTasksJSON)
	w := serveJSON(t, archiveRouter(e.srv), "POST", "/api/archive/retire", `{"hashes":["aaa","ghost"]}`)
	if w.Code != 200 {
		t.Fatalf("应返回 200，got %d body=%s", w.Code, w.Body.String())
	}
	var data struct {
		Retired int `json:"retired"`
	}
	decodeAPI(t, w).bindData(t, &data)
	if data.Retired != 1 {
		t.Errorf("qB 里没有的 hash 不该计数，got %d", data.Retired)
	}
	if _, ok := e.store.Get("ghost"); ok {
		t.Error("qB 里没有的任务不该建档")
	}
}

func TestArchiveRetire_BlankAndEmptyHashes(t *testing.T) {
	e := newArchiveEnv(t, "", twoTasksJSON)
	r := archiveRouter(e.srv)
	for _, body := range []string{`{"hashes":[]}`, `{"hashes":["  ",""]}`, `{}`} {
		w := serveJSON(t, r, "POST", "/api/archive/retire", body)
		if w.Code != 400 {
			t.Errorf("body=%s 应返回 400，got %d", body, w.Code)
		}
		if resp := decodeAPI(t, w); resp.Success || !strings.Contains(resp.Message, "hash") {
			t.Errorf("body=%s 应提示缺少 hash，got %+v", body, resp)
		}
	}
	if len(mockDeletes(e.qb)) != 0 {
		t.Error("参数不合法时不该发任何删除请求")
	}
}

// 非法 body 一律 400 且零副作用。绑定失败与「hash 列表为空」都会落到 400，
// 但前者必须报「参数错误」而不是「缺少任务 hash」——前端要能区分
// 「请求体坏了」和「你一条都没选」。
func TestArchiveRetire_InvalidJSON(t *testing.T) {
	e := newArchiveEnv(t, "", twoTasksJSON)
	r := archiveRouter(e.srv)
	for _, body := range []string{`not-json`, `{"hashes":["aaa"`, `{"hashes":"aaa"}`} {
		w := serveJSON(t, r, "POST", "/api/archive/retire", body)
		if w.Code != 400 {
			t.Errorf("body=%s 应返回 400，got %d", body, w.Code)
		}
		resp := decodeAPI(t, w)
		if resp.Success || resp.Message != "参数错误" {
			t.Errorf("body=%s 应报参数错误，got %+v", body, resp)
		}
	}
	if len(mockDeletes(e.qb)) != 0 {
		t.Error("参数不合法时不该发任何删除请求")
	}
	if e.store.Len() != 0 {
		t.Error("参数不合法时不该建档")
	}
}

func TestArchiveRetire_UpstreamFailureKeepsCandidate(t *testing.T) {
	e := newArchiveEnv(t, "", twoTasksJSON)
	e.qb.actionStatus = http.StatusInternalServerError
	w := serveJSON(t, archiveRouter(e.srv), "POST", "/api/archive/retire", `{"hashes":["aaa"]}`)
	if w.Code != 500 {
		t.Fatalf("qB 删除失败应返回 500，got %d body=%s", w.Code, w.Body.String())
	}
	if resp := decodeAPI(t, w); resp.Success || resp.Message == "" {
		t.Errorf("应带错误说明，got %+v", resp)
	}
	rec, ok := e.store.Get("aaa")
	if !ok {
		t.Fatal("删失败也要留档，下轮才能重试")
	}
	if rec.RetiredAt != 0 {
		t.Errorf("删失败不该标记已退休，got %d", rec.RetiredAt)
	}
}

func TestArchiveRetire_ModuleNotLoaded(t *testing.T) {
	s := setupTestServerFull(t, t.TempDir(), "", "")
	w := serveJSON(t, archiveRouter(s), "POST", "/api/archive/retire", `{"hashes":["aaa"]}`)
	if w.Code != 500 {
		t.Fatalf("未注入退休器应返回 500，got %d", w.Code)
	}
}

// 退休会把任务从 qB 移除，直连列表缓存必须一起失效，
// 否则快照未就绪的那条回退路径上，页面还会显示已退休的任务最长 5 秒。
func TestArchiveRetire_InvalidatesTorrentListCache(t *testing.T) {
	e := newArchiveEnv(t, "", twoTasksJSON)
	e.srv.tcacheMu.Lock()
	e.srv.tcache["all||"] = &cacheEntry{data: []models.QBTorrent{}, expiresAt: time.Now().Add(time.Minute)}
	e.srv.tcacheMu.Unlock()

	w := serveJSON(t, archiveRouter(e.srv), "POST", "/api/archive/retire", `{"hashes":["aaa"]}`)
	if w.Code != 200 {
		t.Fatalf("应返回 200，got %d body=%s", w.Code, w.Body.String())
	}
	e.srv.tcacheMu.Lock()
	n := len(e.srv.tcache)
	e.srv.tcacheMu.Unlock()
	if n != 0 {
		t.Errorf("退休后直连列表缓存应被清空，还剩 %d 条", n)
	}
}

// =============== 鉴权 ===============

func TestArchiveAuth_WriteEndpointsRequireToken(t *testing.T) {
	e := newArchiveEnv(t, "secret123", twoTasksJSON)
	e.seedThree(t)
	r := archiveRouter(e.srv)

	w := serveJSON(t, r, "GET", "/api/archive", "")
	if w.Code != 401 {
		t.Errorf("未登录检索应 401，got %d", w.Code)
	}
	w = serveJSON(t, r, "DELETE", "/api/archive/h1", "")
	if w.Code != 401 {
		t.Errorf("未登录删除记录应 401，got %d", w.Code)
	}
	if _, ok := e.store.Get("h1"); !ok {
		t.Error("401 之后记录必须还在")
	}
	w = serveJSON(t, r, "POST", "/api/archive/retire", `{"hashes":["aaa"]}`)
	if w.Code != 401 {
		t.Errorf("未登录手动退休应 401，got %d", w.Code)
	}
	if len(mockDeletes(e.qb)) != 0 {
		t.Error("401 之后不该向 qB 发删除请求")
	}

	req := httptest.NewRequest("GET", "/api/archive", nil)
	req.Header.Set("Authorization", "Bearer secret123")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Errorf("带正确令牌应 200，got %d body=%s", w.Code, w.Body.String())
	}
}

// =============== 真实路由注册（Start 里挂没挂上） ===============

// archiveHTTP 向真实监听端口发一次请求。响应体读完再关：
// 提前 Close 会让这条连接被丢弃，Server.Shutdown 要等它收尾，测试凭空慢几秒。
func archiveHTTP(t *testing.T, method, rawURL, body string) (int, string) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, rawURL, reader)
	if err != nil {
		t.Fatalf("构造请求失败：%v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s 失败：%v", method, rawURL, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应失败：%v", err)
	}
	return resp.StatusCode, string(b)
}

func TestArchiveRoutesRegisteredInStart(t *testing.T) {
	e := newArchiveEnv(t, "secret123", twoTasksJSON)
	e.seedThree(t)
	addr := freeLocalAddr(t)
	cfg := e.srv.cfg.Get()
	cfg.Server.Listen = addr
	e.srv.cfg.Set(cfg)
	go func() { _ = e.srv.Start("") }()
	t.Cleanup(func() { _ = e.srv.Shutdown() })
	waitListening(t, addr)
	base := "http://" + addr

	// 无令牌：三条路由都必须是 401 而不是 404（404 说明根本没注册）
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/api/archive", ""},
		{"POST", "/api/archive/retire", `{"hashes":[]}`},
		{"DELETE", "/api/archive/h1", ""},
	} {
		if code, _ := archiveHTTP(t, tc.method, base+tc.path, tc.body); code != 401 {
			t.Errorf("%s %s 无令牌应 401（404 意味着路由没注册），got %d", tc.method, tc.path, code)
		}
	}

	if code, body := archiveHTTP(t, "GET", base+"/api/archive?token=secret123&pending=1", ""); code != 200 || !strings.Contains(body, `"h3"`) {
		t.Errorf("带令牌检索候选应命中 h3，got %d %s", code, body)
	}
	if code, body := archiveHTTP(t, "DELETE", base+"/api/archive/h1?token=secret123", ""); code != 200 {
		t.Errorf("带令牌删除记录应 200，got %d %s", code, body)
	}
	if _, ok := e.store.Get("h1"); ok {
		t.Error("h1 应已被移除")
	}
	if code, body := archiveHTTP(t, "POST", base+"/api/archive/retire?token=secret123", `{"hashes":["aaa"]}`); code != 200 || !strings.Contains(body, `"retired":1`) {
		t.Errorf("带令牌手动退休应退休 1 条，got %d %s", code, body)
	}
}
