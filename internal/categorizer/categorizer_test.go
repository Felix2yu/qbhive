package categorizer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Felix2yu/qbhive/internal/config"
	"github.com/Felix2yu/qbhive/internal/models"
	qb "github.com/Felix2yu/qbhive/internal/qbittorrent"
)

// ---------- 条件求值 ----------

func TestCountLatinAndCJK(t *testing.T) {
	cases := []struct {
		in    string
		latin int
		cjk   int
	}{
		{"SONE-123 部长与社长", 4, 5},
		{"Star.Wars.Episode.1080p.x264", 17, 0},
		{"这是一个相当长的中文名称超过十个汉字的文件名", 0, 22},
		{"", 0, 0},
	}
	for _, c := range cases {
		if got := countLatin(c.in); got != c.latin {
			t.Errorf("countLatin(%q) = %d, want %d", c.in, got, c.latin)
		}
		if got := countCJK(c.in); got != c.cjk {
			t.Errorf("countCJK(%q) = %d, want %d", c.in, got, c.cjk)
		}
	}
}

// 编号式命名必须躲开 H-264 / x264-GROUP 这类编码标签的误伤。
// 这是宽松写法 `[A-Za-z]+-[0-9]+` 的真实翻车点：Movie.Name.2020.H-264 会被判成编号。
func TestCodeCondNoFalsePositive(t *testing.T) {
	hit := []string{"SONE-123 部长与社长", "MIDE-140-C", "ABC-123", "AB-1234"}
	miss := []string{
		"Movie.Name.2020.H-264.1080p",
		"The.Show.S01E01.1080p.x264-GROUP",
		"Boku.no.Hero.Academia.S07E12.1080p.WEB-DL.x265",
		"这是一个中文名称哦超过四个字",
	}
	cond := models.CategoryCond{Type: models.CondCode, MinLetters: 2, MinDigits: 2}
	m, err := compileCond(cond)
	if err != nil {
		t.Fatalf("compileCond: %v", err)
	}
	for _, s := range hit {
		if !m.Match(s) {
			t.Errorf("应命中编号式：%q", s)
		}
	}
	for _, s := range miss {
		if m.Match(s) {
			t.Errorf("不应命中编号式：%q", s)
		}
	}
}

func TestCompileCondUnknownType(t *testing.T) {
	if _, err := compileCond(models.CategoryCond{Type: "nope"}); err == nil {
		t.Fatal("未知类型应当报错，不能静默当成不命中")
	}
	if _, err := compileCond(models.CategoryCond{Type: models.CondRegex, Pattern: "("}); err == nil {
		t.Fatal("非法正则应当报错")
	}
}

// ---------- 规则优先级与兜底 ----------

func testRules() []models.CategoryRule {
	on := true
	return []models.CategoryRule{
		{
			ID: "code", Name: "编号式命名", Enabled: true, Category: "编号",
			When: []models.CategoryCond{{Type: models.CondCode, MinLetters: 2, MinDigits: 2, Anchored: &on}},
		},
		{
			ID: "latin", Name: "英文长名", Enabled: true, Category: "外文",
			When: []models.CategoryCond{{Type: models.CondLatinCount, Min: 15}},
		},
		{
			ID: "cjk", Name: "中文长名", Enabled: true, Category: "中文",
			When: []models.CategoryCond{{Type: models.CondCJKCount, Min: 10}},
		},
	}
}

func TestEvaluatePriorityAndFallback(t *testing.T) {
	rules, err := compileRules(testRules())
	if err != nil {
		t.Fatalf("compileRules: %v", err)
	}
	cases := []struct {
		name       string
		wantTarget string
		wantRule   string
	}{
		{"SONE-123 部长与社长", "编号", "编号式命名"},
		{"Star.Wars.Episode.1080p.x264", "外文", "英文长名"},
		{"这是一个相当长的中文名称超过十个汉字", "中文", "中文长名"},
		{"短名", "兜底", ""}, // 走兜底，规则名为空
	}
	for _, c := range cases {
		target, rule := evaluate(c.name, rules, "兜底")
		if target != c.wantTarget || rule != c.wantRule {
			t.Errorf("evaluate(%q) = (%q, %q), want (%q, %q)", c.name, target, rule, c.wantTarget, c.wantRule)
		}
	}
}

// 用名字特征而非枚举 state 判定「能不能安全分类」。这是 ddc2b109... 误判的根因修复：
// 排队中的磁链（queuedDL）名字仍是 hex hash 原文，曾经被 latinCount 误判成「外文」。
func TestReadyForCategory(t *testing.T) {
	cases := []struct {
		name string
		st   string
		size int64
		want bool
	}{
		{"ddc2b10963e785dc1ef2687ebebf0902ec2ae977", "queuedDL", -1, false},     // 本次 bug：排队中 + hex 名字
		{"8AC3731AD4B2A2B1C4D5E6F708192A3B4C5D6E7F", "metaDL", -1, false},      // 正在取元数据 + hex 名字
		{"magnet:?xt=urn:btih:8AC3", "metaDL", -1, false},                      // magnet 链接原文
		{"ddc2b10963e785dc1ef2687ebebf0902ec2ae977", "downloading", 8589934592, false}, // 名字仍是 hash：还没解析出真实名，不动
		{"SONE-123 部长与社长", "queuedDL", 8589934592, true},                  // 带 dn 的 magnet，名字已是真实名
		{"Star.Wars.Episode.1080p.x264", "queuedDL", 8589934592, true},         // 真实英文名 + 排队中
		{"这是一个相当长的中文名称超过十个汉字", "downloading", 8589934592, true},
		{"短名", "downloading", 0, true}, // size 字段缺失(=0) 不能拦：否则整个功能静默失效
	}
	for _, c := range cases {
		got := readyForCategory(models.QBTorrent{Name: c.name, State: c.st, Size: c.size})
		if got != c.want {
			t.Errorf("readyForCategory(%q,%s,%d) = %v, want %v", c.name, c.st, c.size, got, c.want)
		}
	}
}

// 顺序即优先级：把「英文长名」挪到第一条后，同时满足英文与编号的名字应归外文
func TestEvaluateOrderMatters(t *testing.T) {
	rules := testRules()
	rules[0], rules[1] = rules[1], rules[0]
	compiled, err := compileRules(rules)
	if err != nil {
		t.Fatalf("compileRules: %v", err)
	}
	// MIDE-140-C 只有 5 个字母，不够英文长名，仍落编号
	if target, _ := evaluate("MIDE-140-C", compiled, ""); target != "编号" {
		t.Errorf("换序后 MIDE-140-C 应为编号，got %q", target)
	}
}

// 空条件的规则绝不能命中任何任务，否则新建的空规则会吞掉整个库
func TestEmptyWhenNeverMatches(t *testing.T) {
	compiled, err := compileRules([]models.CategoryRule{
		{ID: "empty", Enabled: true, Category: "吞并", When: nil},
	})
	if err != nil {
		t.Fatalf("compileRules: %v", err)
	}
	if target, _ := evaluate("任意任务名", compiled, ""); target != "" {
		t.Errorf("空条件规则不应命中，got %q", target)
	}
}

func TestDisabledRuleSkipped(t *testing.T) {
	rules := testRules()
	rules[0].Enabled = false
	compiled, err := compileRules(rules)
	if err != nil {
		t.Fatalf("compileRules: %v", err)
	}
	if target, _ := evaluate("SONE-123 部长", compiled, ""); target == "编号" {
		t.Error("停用的规则不应参与匹配")
	}
}

// ---------- 校验与指纹 ----------

func TestValidateRules(t *testing.T) {
	on := true
	bad := []struct {
		name string
		in   []models.CategoryRule
	}{
		{"缺少分类名", []models.CategoryRule{{ID: "a", Enabled: true, When: []models.CategoryCond{{Type: models.CondCode}}}}},
		{"非法分类名", []models.CategoryRule{{ID: "a", Enabled: true, Category: "a\\b", When: []models.CategoryCond{{Type: models.CondCode}}}}},
		{"路径穿越", []models.CategoryRule{{ID: "a", Enabled: true, Category: "../x", When: []models.CategoryCond{{Type: models.CondCode}}}}},
		{"未知类型", []models.CategoryRule{{ID: "a", Enabled: true, Category: "x", When: []models.CategoryCond{{Type: "bogus"}}}}},
		{"非法正则", []models.CategoryRule{{ID: "a", Enabled: true, Category: "x", When: []models.CategoryCond{{Type: models.CondRegex, Pattern: "("}}}}},
	}
	for _, c := range bad {
		if msg := ValidateRules(c.in); msg == "" {
			t.Errorf("%s：应当被校验拦下", c.name)
		}
	}
	good := []models.CategoryRule{
		{ID: "a", Enabled: true, Category: "编号", When: []models.CategoryCond{{Type: models.CondCode, Anchored: &on}}},
	}
	if msg := ValidateRules(good); msg != "" {
		t.Errorf("合法规则不应报错：%s", msg)
	}
}

func TestRulesSignatureChanges(t *testing.T) {
	ac := models.AutoCategoryConfig{Rules: testRules()}
	base := rulesSignature(ac)
	ac.Rules[1].Category = "英语"
	if rulesSignature(ac) == base {
		t.Error("改分类目标后指纹应变化")
	}
	ac.Rules[1].Category = "外文"
	ac.Fallback = "其他"
	if rulesSignature(ac) == base {
		t.Error("改兜底后指纹应变化")
	}
}

// ---------- 试算 ----------

func TestPreviewByNames(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("QBHIVE_CONFIG", filepath.Join(dir, "config.json"))
	m := New(config.New(filepath.Join(dir, "config.json")), nil)
	ac := models.AutoCategoryConfig{Rules: testRules()}
	items, err := m.Preview(ac, []string{"SONE-123", "这是一个相当长的中文名称超过十个汉字", "短名"}, 0)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("want 3 items, got %d", len(items))
	}
	if items[0].Target != "编号" || items[0].Rule != "编号式命名" {
		t.Errorf("第一条应为编号，got %+v", items[0])
	}
	if items[1].Target != "中文" {
		t.Errorf("第二条应为中文，got %+v", items[1])
	}
	if items[2].Target != "" || items[2].Reason == "" {
		t.Errorf("第三条应无结论并给出原因，got %+v", items[2])
	}
}

// ---------- 端到端：真的会调用 qB 的 setCategory ----------

type mockQB struct {
	mu       sync.Mutex
	torrents []models.QBTorrent
	calls    map[string][]string // category → hashes
}

func newMockQBServer(t *testing.T, list []models.QBTorrent) (*httptest.Server, *mockQB) {
	t.Helper()
	st := &mockQB{torrents: list, calls: map[string][]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "SID=testsid; Path=/")
		w.Write([]byte("Ok."))
	})
	mux.HandleFunc("/api/v2/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("Ok."))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/torrents/info":
			st.mu.Lock()
			defer st.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(st.torrents)
		case "/api/v2/torrents/setCategory":
			_ = r.ParseForm()
			cat := r.FormValue("category")
			st.mu.Lock()
			st.calls[cat] = append(st.calls[cat], r.FormValue("hashes"))
			st.mu.Unlock()
			w.Write([]byte("Ok."))
		default:
			w.WriteHeader(200)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, st
}

func TestScanAppliesCategories(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	t.Setenv("QBHIVE_CONFIG", cfgPath)

	list := []models.QBTorrent{
		{Hash: "h1", Name: "SONE-123 部长与社长"},
		{Hash: "h2", Name: "Star.Wars.Episode.1080p.x264"},
		{Hash: "h3", Name: "这是一个相当长的中文名称超过十个汉字"},
		{Hash: "h4", Name: "短名"},
		{Hash: "h5", Name: "ABC-999 已有分类", Category: "手动整理"},
	}
	srv, st := newMockQBServer(t, list)
	cli := qb.New(srv.URL, "u", "p", "")

	cfgMgr := config.New(cfgPath)
	on := true
	base := cfgMgr.Get()
	base.AutoCategory = models.AutoCategoryConfig{
		Enabled: true, Interval: 60, OnlyIfEmpty: &on, Rules: testRules(),
	}
	cfgMgr.Set(base)

	m := New(cfgMgr, cli)
	m.scan()

	st.mu.Lock()
	defer st.mu.Unlock()
	if got := len(st.calls["编号"]); got != 1 {
		t.Errorf("编号分类应被调用 1 次，got %d", got)
	}
	if st.calls["编号"][0] != "h1" {
		t.Errorf("编号分类应作用于 h1，got %q", st.calls["编号"])
	}
	if st.calls["外文"][0] != "h2" {
		t.Errorf("外文分类应作用于 h2，got %q", st.calls["外文"])
	}
	if st.calls["中文"][0] != "h3" {
		t.Errorf("中文分类应作用于 h3，got %q", st.calls["中文"])
	}
	// h4 无结论、h5 已有分类，都不该出现在任何一次调用里
	seen := map[string]bool{}
	for _, hs := range st.calls {
		for _, h := range hs {
			seen[h] = true
		}
	}
	if len(seen) != 3 || !seen["h1"] || !seen["h2"] || !seen["h3"] {
		t.Errorf("只有 h1/h2/h3 应被分类，实际：%+v", seen)
	}
}

// 规则没变、分类已生效时，重复扫描不应重复调用 qB
func TestScanIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	t.Setenv("QBHIVE_CONFIG", cfgPath)

	list := []models.QBTorrent{{Hash: "h1", Name: "SONE-123 部长"}}
	srv, st := newMockQBServer(t, list)
	cli := qb.New(srv.URL, "u", "p", "")

	cfgMgr := config.New(cfgPath)
	on := true
	base := cfgMgr.Get()
	base.AutoCategory = models.AutoCategoryConfig{
		Enabled: true, Interval: 60, OnlyIfEmpty: &on, Rules: testRules(),
	}
	cfgMgr.Set(base)

	m := New(cfgMgr, cli)
	m.scan()
	st.mu.Lock()
	first := len(st.calls["编号"])
	st.mu.Unlock()
	// 第二轮：qB 侧已带上分类，状态里也记了指纹，不应再调
	st.torrents = []models.QBTorrent{{Hash: "h1", Name: "SONE-123 部长", Category: "编号"}}
	m.scan()
	st.mu.Lock()
	second := len(st.calls["编号"])
	st.mu.Unlock()
	if second != first {
		t.Errorf("重复扫描不应重复调用：第一轮 %d 次，第二轮 %d 次", first, second)
	}
}

// 磁链在拿到元数据之前，qB 返回的名字是 hex hash 或 magnet 链接原文。
// 这两类名字的拉丁字母数在 17~42，必然越过 latinCount 阈值被判成「外文」；
// 而一旦在这时打上分类，「只处理未分类任务」的闸门会让之后永远不再重新
// 评估，错分类被永久固化。所以元数据到位前必须一个都不动。
func TestScanSkipsMetadataPending(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	t.Setenv("QBHIVE_CONFIG", cfgPath)

	list := []models.QBTorrent{
		{Hash: "m1", Name: "magnet:?xt=urn:btih:8AC3731AD4B2A2B1C4D5E6F708192A3B4C5D6E7F",
			State: "metaDL", Size: -1},
		{Hash: "m2", Name: "8AC3731AD4B2A2B1C4D5E6F708192A3B4C5D6E7F",
			State: "forcedMetaDL", Size: -1},
		{Hash: "m3", Name: "E5C8B1A7D4F2A9B6C3E8D1F4A7B2C5E8D9F1A4B7",
			State: "metaDL", Size: -1},
		// 本次 bug 复现：排队中的磁链，名字仍是 hex hash 原文，state=queuedDL
		{Hash: "q1", Name: "ddc2b10963e785dc1ef2687ebebf0902ec2ae977",
			State: "queuedDL", Size: -1},
		// 对照组：元数据已到位的正常任务，必须照常分类
		{Hash: "ok1", Name: "SONE-123 部长与社长", State: "downloading", Size: 8589934592},
	}
	srv, st := newMockQBServer(t, list)
	cli := qb.New(srv.URL, "u", "p", "")

	cfgMgr := config.New(cfgPath)
	on := true
	base := cfgMgr.Get()
	base.AutoCategory = models.AutoCategoryConfig{
		Enabled: true, Interval: 60, OnlyIfEmpty: &on, Rules: testRules(),
	}
	cfgMgr.Set(base)

	m := New(cfgMgr, cli)
	m.scan()

	st.mu.Lock()
	defer st.mu.Unlock()
	seen := map[string]bool{}
	for _, hs := range st.calls {
		for _, h := range hs {
			seen[h] = true
		}
	}
	for _, h := range []string{"m1", "m2", "m3", "q1"} {
		if seen[h] {
			t.Errorf("%s 处于待元数据状态，不该被分类", h)
		}
	}
	if !seen["ok1"] {
		t.Errorf("元数据已到位的 ok1 应被正常分类，实际调用：%+v", st.calls)
	}
}

// 存量自愈：曾经在 hex 阶段被误打分类、现在仍未就绪的任务，扫描时应清除错误分类
// （qB 侧取消分类 + 内存记录删除），且只在 qB 现状仍等于记录值（用户没手动改过）时动 qB。
func TestScanHealsMisclassifiedPending(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	t.Setenv("QBHIVE_CONFIG", cfgPath)

	hash := "ddc2b10963e785dc1ef2687ebebf0902ec2ae977"
	srv, st := newMockQBServer(t, []models.QBTorrent{
		// 真实名字仍是 hash，元数据未就绪；qB 现状分类 W 等于 categorizer 记录值
		{Hash: hash, Name: hash, State: "queuedDL", Size: -1, Category: "W"},
	})
	cli := qb.New(srv.URL, "u", "p", "")

	cfgMgr := config.New(cfgPath)
	on := true
	base := cfgMgr.Get()
	base.AutoCategory = models.AutoCategoryConfig{
		Enabled: true, Interval: 60, OnlyIfEmpty: &on, Rules: testRules(),
	}
	cfgMgr.Set(base)

	m := New(cfgMgr, cli)
	// 预置历史误打记录（categorizer 之前在 hex 阶段误打成了 W）
	m.state[hash] = &catRecord{Cat: "W", Sig: "old"}

	m.scan()

	// 应当清掉误打：qB 收到一次空分类调用，且 state 记录被删除
	st.mu.Lock()
	cleared := st.calls[""]
	st.mu.Unlock()
	if len(cleared) != 1 || cleared[0] != hash {
		t.Errorf("误打分类应被清除（空分类调用），实际：%+v", st.calls)
	}
	if _, ok := m.state[hash]; ok {
		t.Errorf("清除后 state 记录应被删除")
	}
}

// 用户手动改过分类的未就绪任务：categorizer 不应越权清除，只删自己的记录。
func TestScanRespectsManuallyChangedPending(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	t.Setenv("QBHIVE_CONFIG", cfgPath)

	hash := "ddc2b10963e785dc1ef2687ebebf0902ec2ae977"
	srv, st := newMockQBServer(t, []models.QBTorrent{
		// 用户已手动把分类从 W 改成「手动整理」
		{Hash: hash, Name: hash, State: "queuedDL", Size: -1, Category: "手动整理"},
	})
	cli := qb.New(srv.URL, "u", "p", "")

	cfgMgr := config.New(cfgPath)
	on := true
	base := cfgMgr.Get()
	base.AutoCategory = models.AutoCategoryConfig{
		Enabled: true, Interval: 60, OnlyIfEmpty: &on, Rules: testRules(),
	}
	cfgMgr.Set(base)

	m := New(cfgMgr, cli)
	m.state[hash] = &catRecord{Cat: "W", Sig: "old"} // categorizer 误打记录

	m.scan()

	st.mu.Lock()
	cleared := st.calls[""]
	st.mu.Unlock()
	if len(cleared) != 0 {
		t.Errorf("用户手动改过的分类不应被清除，实际：%+v", st.calls)
	}
	if _, ok := m.state[hash]; ok {
		t.Errorf("categorizer 自己的误打记录应被删除")
	}
}

// 元数据到位后（state 转成 downloading）必须补上分类，
// 否则磁链任务会永远停在未分类状态。
func TestScanPicksUpAfterMetadataArrives(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	t.Setenv("QBHIVE_CONFIG", cfgPath)

	hash := "8AC3731AD4B2A2B1C4D5E6F708192A3B4C5D6E7F"
	srv, st := newMockQBServer(t, []models.QBTorrent{
		{Hash: "m1", Name: "magnet:?xt=urn:btih:" + hash, State: "metaDL", Size: -1},
	})
	cli := qb.New(srv.URL, "u", "p", "")

	cfgMgr := config.New(cfgPath)
	on := true
	base := cfgMgr.Get()
	base.AutoCategory = models.AutoCategoryConfig{
		Enabled: true, Interval: 60, OnlyIfEmpty: &on, Rules: testRules(),
	}
	cfgMgr.Set(base)

	m := New(cfgMgr, cli)
	m.scan()
	st.mu.Lock()
	if n := len(st.calls); n != 0 {
		t.Errorf("待元数据阶段不应调用 setCategory，实际 %d 次", n)
	}
	st.mu.Unlock()

	// 元数据到位：名字变成真实的中文名，state 转 downloading
	st.mu.Lock()
	st.torrents = []models.QBTorrent{
		{Hash: "m1", Name: "这是一个相当长的中文名称超过十个汉字", State: "downloading", Size: 8589934592},
	}
	st.mu.Unlock()

	m.scan()
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.calls["中文"]) != 1 || st.calls["中文"][0] != "m1" {
		t.Errorf("元数据到位后 m1 应被归入「中文」，实际：%+v", st.calls)
	}
}

// 状态文件应落在 QBHIVE_CONFIG 所在目录，与既有模块保持同一约定
func TestStatePathFollowsConfigDir(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	t.Setenv("QBHIVE_CONFIG", cfgPath)
	m := New(config.New(cfgPath), nil)
	if m.statePath != filepath.Join(dir, stateFile) {
		t.Errorf("statePath = %q", m.statePath)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("配置目录应存在：%v", err)
	}
}
