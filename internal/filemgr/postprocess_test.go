package filemgr

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Felix2yu/qbhive/internal/config"
	"github.com/Felix2yu/qbhive/internal/models"
	qb "github.com/Felix2yu/qbhive/internal/qbittorrent"
)

// setupPostProcess 返回一个后处理用的 Manager：qB 客户端走最小 mock，/torrents/files
// 用 filesJSON 作答（各用例据此喂「任务自己的文件列表」），持久化状态与配置都落在
// 临时目录里，用例之间互不干扰。
func setupPostProcess(t *testing.T, pc models.PostProcessConfig, filesJSON ...string) *Manager {
	t.Helper()
	return setupPostFixture(t, pc, filesJSON...).m
}

// postFixture 除了 Manager 还留出配置路径与 mock 地址，
// 供「模拟重启再建一个 Manager（读同一份状态文件）」的用例使用
type postFixture struct {
	m      *Manager
	cfgPth string
	qbURL  string
}

// restart 用同一份配置文件与同一个 mock 服务端新建 Manager：等价于进程重启后重新加载状态
func (f postFixture) restart(t *testing.T, pc models.PostProcessConfig) *Manager {
	t.Helper()
	m := New(config.New(f.cfgPth), qb.New(f.qbURL, "u", "p", ""))
	cfg := m.cfg.Get()
	cfg.FileManager.PostProcess = pc
	m.cfg.Set(cfg)
	return m
}

func setupPostFixture(t *testing.T, pc models.PostProcessConfig, filesJSON ...string) postFixture {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("QBHIVE_CONFIG", cfgPath)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Set-Cookie", "SID=test")
		w.Write([]byte("Ok."))
	})
	mux.HandleFunc("/api/v2/torrents/files", func(w http.ResponseWriter, _ *http.Request) {
		body := "[]"
		if len(filesJSON) > 0 && filesJSON[0] != "" {
			body = filesJSON[0]
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	m := New(config.New(cfgPath), qb.New(srv.URL, "u", "p", ""))
	cfg := m.cfg.Get()
	cfg.FileManager.PostProcess = pc
	m.cfg.Set(cfg)
	return postFixture{m: m, cfgPth: cfgPath, qbURL: srv.URL}
}

// qbFiles 把文件名列表拼成 qB /torrents/files 的响应体
func qbFiles(names ...string) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"name":`)
		b.WriteString(strconv.Quote(n))
		b.WriteString(`,"size":100,"progress":1}`)
	}
	b.WriteByte(']')
	return b.String()
}

func TestShellSingleQuote(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/tmp/a.mov", "'/tmp/a.mov'"},
		{"/tmp/a b.mov", "'/tmp/a b.mov'"},
		{"/tmp/it's.mov", `'/tmp/it'\''s.mov'`},
		{"", "''"},
	}
	for _, c := range cases {
		if got := shellSingleQuote(c.in); got != c.want {
			t.Errorf("shellSingleQuote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRenderPostCommand(t *testing.T) {
	tor := models.QBTorrent{Name: "任务 A", Category: "电影", Tags: "转码", SavePath: "/dl/movie"}
	got, err := renderPostCommand(`/usr/bin/shortcuts run "Permute HEVC 50% 缩放" -i "$FILE"`, tor, "/Users/me/Downloads/我的 电影.mov")
	if err != nil {
		t.Fatalf("渲染失败：%v", err)
	}
	if !strings.Contains(got, `/usr/bin/shortcuts run "Permute HEVC 50% 缩放" -i "$FILE"`) {
		t.Errorf("$FILE 之外的内容被改动了：%s", got)
	}
	// {} 占位符必须替换成单引号字面量，含空格与中文也不丢
	if got, _ := renderPostCommand("echo {file}", tor, "/tmp/a b 中文.mov"); got != "echo '/tmp/a b 中文.mov'" {
		t.Errorf("占位符替换结果不对：%s", got)
	}
	if got, _ := renderPostCommand("echo {name} {category} {tags} {dir} {savepath}", tor, "/tmp/a.mov"); got != "echo '任务 A' '电影' '转码' '/tmp' '/dl/movie'" {
		t.Errorf("占位符替换结果不对：%s", got)
	}
	if _, err := renderPostCommand("   ", tor, "/x"); err == nil {
		t.Error("空命令应报错")
	}
}

// 两套取值方式必须齐平：postEnvNames 里每个环境变量都能被 shell 读到原值，
// 对应的小写 {} 占位符也都能替换成单引号字面量（加新变量时漏一边就会在这里失败）
func TestPostCommand_EnvAndPlaceholders(t *testing.T) {
	tor := models.QBTorrent{Name: "任务 A", Category: "电影", Tags: "转码,1080p", SavePath: t.TempDir()}
	file := filepath.Join(tor.SavePath, "任务 A.mov")
	want := map[string]string{
		"FILE": file, "DIR": filepath.Dir(file), "NAME": tor.Name,
		"CATEGORY": tor.Category, "TAGS": tor.Tags, "SAVEPATH": tor.SavePath,
	}
	for _, name := range postEnvNames {
		code, out, err := execPostCommand(`printf %s "$`+name+`"`, tor, file, 10)
		if err != nil || code != 0 {
			t.Fatalf("$%s 执行失败：code=%d err=%v out=%s", name, code, err, out)
		}
		if got := strings.TrimSpace(out); got != want[name] {
			t.Errorf("$%s = %q，want %q", name, got, want[name])
		}

		cmd, err := renderPostCommand("printf %s {"+strings.ToLower(name)+"}", tor, file)
		if err != nil {
			t.Fatalf("{%s} 渲染失败：%v", strings.ToLower(name), err)
		}
		code, out, err = execPostCommand(cmd, tor, file, 10)
		if err != nil || code != 0 {
			t.Fatalf("{%s} 执行失败：code=%d err=%v out=%s", strings.ToLower(name), code, err, out)
		}
		if got := strings.TrimSpace(out); got != want[name] {
			t.Errorf("{%s} = %q，want %q", strings.ToLower(name), got, want[name])
		}
	}
}

// 路径含空格、中文、单引号；$FILE 与 {file} 两种写法都必须能安全取出内容
func TestExecPostCommand_QuotedPath(t *testing.T) {
	dir := t.TempDir()
	name := `电影 名称 '引号'.mov`
	p := filepath.Join(dir, name)
	writeFile(t, p, name)
	writeFile(t, filepath.Join(dir, "x"), "x")
	tor := models.QBTorrent{Name: "n", SavePath: dir}
	cases := []struct{ cmd, file, want string }{
		{`cat "$FILE"`, p, name},
		{`cat {file}`, p, name}, // 占位符自带单引号
		{`cat {dir}/x`, dir + "/x", "x"},
		{`cat "$DIR/x"`, p, "x"},
	}
	for _, c := range cases {
		// 带 {} 的用例先过一遍占位符渲染（与真实执行路径一致）
		cmd := c.cmd
		if strings.Contains(cmd, "{") {
			var e error
			if cmd, e = renderPostCommand(cmd, tor, c.file); e != nil {
				t.Fatal(e)
			}
		}
		code, out, err := execPostCommand(cmd, tor, c.file, 10)
		if err != nil {
			t.Fatalf("%s 执行出错：%v", cmd, err)
		}
		if code != 0 {
			t.Fatalf("%s 退出码非 0：%d（%s）", cmd, code, out)
		}
		if strings.TrimSpace(out) != c.want {
			t.Errorf("%s 输出 %q，want %q", cmd, strings.TrimSpace(out), c.want)
		}
	}
}

// 超时的命令必须被杀掉，而不是无限挂着
func TestExecPostCommand_Timeout(t *testing.T) {
	_, _, err := execPostCommand("sleep 30", models.QBTorrent{}, "/tmp/x", 1)
	if err == nil {
		t.Fatal("超时应返回错误")
	}
}

func TestMatchExt(t *testing.T) {
	cases := []struct {
		file, list string
		want       bool
	}{
		{"/a/b.MOV", "mov,mp4", true},   // 大小写不敏感
		{"/a/b.mkv", "mov,mp4", false},  // 不在白名单
		{"/a/b.mov", "", true},          // 留空放行
		{"/a/b.txt", "txt , mov", true}, // 含空格分隔
		{"/a/b.txt", "txt,,mov", true},  // 空项不误伤
		{"/a/b", "mov", false},          // 无扩展名
		{"/a/b.mov.exe", "mov", false},
	}
	for _, c := range cases {
		if got := matchExt(c.file, c.list); got != c.want {
			t.Errorf("matchExt(%q, %q) = %v, want %v", c.file, c.list, got, c.want)
		}
	}
}

// 名单分隔符兼容：中文输入法打出来的全角逗号、顿号、分号、竖线、换行都得拆开。
// 只认半角逗号的话，「动漫，剧集」会被当成一个匹配不上的分类，看起来就像名单没生效。
func TestSplitNameListSeparators(t *testing.T) {
	want := "动漫|剧集|国创"
	for _, list := range []string{
		"动漫,剧集,国创",
		"动漫，剧集，国创",
		"动漫、剧集、国创",
		"动漫; 剧集；国创",
		"动漫 | 剧集｜国创",
		"动漫\n剧集\r\n国创",
		"  动漫 ,, 剧集,国创, ",
	} {
		if got := strings.Join(splitNameList(list), "|"); got != want {
			t.Errorf("splitNameList(%q) = %q，want %q", list, got, want)
		}
	}
	// 分隔符写法不同不该改变指纹：否则「把全角逗号改成半角」会白白重扫一批历史任务
	sigOf := func(s string) string {
		return postFilterSignature(models.PostProcessConfig{CategoryInclude: s})
	}
	if sigOf("动漫，剧集") != sigOf("动漫,剧集") {
		t.Error("全角与半角逗号的名单应得到同一个指纹")
	}
	// 过滤与扩展名白名单都吃这套分隔符
	tor := models.QBTorrent{Category: "剧集", Tags: "国创"}
	skip, why := postTaskFiltered(tor, models.PostProcessConfig{CategoryInclude: "动漫，剧集", TagInclude: "国创｜海外"})
	if skip {
		t.Errorf("全角分隔的名单应能命中，why=%s", why)
	}
	if !matchExt("/a/b.MP4", "mov，mp4") {
		t.Error("扩展名白名单也该认全角逗号")
	}
	if matchExt("/a/b.mkv", "mov，mp4") {
		t.Error("扩展名不在白名单内不该放行")
	}
}

// 分类/标签过滤：整任务级判定，命中即整个任务不转码
func TestPostTaskFiltered(t *testing.T) {
	cases := []struct {
		name string
		tor  models.QBTorrent
		pc   models.PostProcessConfig
		want bool
	}{
		{"名单全空不过滤", models.QBTorrent{Category: "动漫", Tags: "转码"}, models.PostProcessConfig{}, false},
		{"名单只有空项不过滤", models.QBTorrent{Category: "动漫"},
			models.PostProcessConfig{CategoryInclude: " , ", TagExclude: ","}, false},
		{"分类包含命中", models.QBTorrent{Category: "动漫"},
			models.PostProcessConfig{CategoryInclude: "电影, 动漫 "}, false},
		{"分类包含未命中", models.QBTorrent{Category: "剧集"},
			models.PostProcessConfig{CategoryInclude: "电影,动漫"}, true},
		{"分类大小写不敏感", models.QBTorrent{Category: "Anime"},
			models.PostProcessConfig{CategoryInclude: "anime"}, false},
		// 未分类不命中任何包含名单：否则「填了包含却没打分类」的任务会被静默转码
		{"未分类不满足包含名单", models.QBTorrent{Category: "  "},
			models.PostProcessConfig{CategoryInclude: "动漫"}, true},
		{"分类排除命中", models.QBTorrent{Category: "电影"},
			models.PostProcessConfig{CategoryExclude: "电影,纪录片"}, true},
		{"排除优先于包含", models.QBTorrent{Category: "电影"},
			models.PostProcessConfig{CategoryInclude: "电影", CategoryExclude: "电影"}, true},
		{"未分类不受排除名单影响", models.QBTorrent{},
			models.PostProcessConfig{CategoryExclude: "电影"}, false},
		{"标签包含命中其一", models.QBTorrent{Tags: "1080p, 转码 "},
			models.PostProcessConfig{TagInclude: "转码,剧集"}, false},
		{"标签包含未命中", models.QBTorrent{Tags: "1080p"},
			models.PostProcessConfig{TagInclude: "转码"}, true},
		{"无标签不满足包含名单", models.QBTorrent{Tags: " , "},
			models.PostProcessConfig{TagInclude: "转码"}, true},
		{"标签排除命中", models.QBTorrent{Tags: "真人,已看"},
			models.PostProcessConfig{TagExclude: "已看"}, true},
		{"分类通过但标签被排除", models.QBTorrent{Category: "动漫", Tags: "真人"},
			models.PostProcessConfig{CategoryInclude: "动漫", TagExclude: "真人"}, true},
		{"分类与标签两个维度都通过", models.QBTorrent{Category: "动漫", Tags: "转码,1080p"},
			models.PostProcessConfig{CategoryInclude: "动漫", TagInclude: "转码"}, false},
	}
	for _, c := range cases {
		skip, why := postTaskFiltered(c.tor, c.pc)
		if skip != c.want {
			t.Errorf("%s: skip=%v want=%v（原因：%s）", c.name, skip, c.want, why)
		}
		if skip && why == "" {
			t.Errorf("%s: 跳过时必须给出原因，返回了 (%v, %q)", c.name, skip, why)
		}
		if !skip && why != "" {
			t.Errorf("%s: 未跳过不该带原因，返回了 (%v, %q)", c.name, skip, why)
		}
	}
}

// 被分类/标签过滤掉的任务：整任务跳过、不发文件列表请求，并把「因过滤而终结」连同
// 当时的名单指纹一起记进状态；名单一改（指纹变了）就解除终结、重新派发
func TestHandlePostProcess_SkipByCategoryTag(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "clip.mov"), "x")
	// 文件列表故意喂一个「会被转码」的 mov：只有过滤生效才不会派发它
	m := setupPostProcess(t, models.PostProcessConfig{
		Enabled: true, Command: "echo hi", Extensions: "mov", CategoryInclude: "动漫",
	}, qbFiles("clip.mov"))
	tor := models.QBTorrent{Name: "n", Hash: "h", SavePath: dir, Category: "电影", Tags: "转码"}
	done, retry := m.handlePostProcess(tor)
	if !done || retry {
		t.Errorf("过滤命中应直接跳过（done=true, retry=false），got (%v, %v)", done, retry)
	}
	rec := m.cleanState["h"]
	if rec == nil || !rec.PostDone || rec.PostFilterList == "" {
		t.Fatalf("应记下「因过滤终结」+ 名单指纹，got %+v", rec)
	}
	if len(rec.PostDispatched) != 0 {
		t.Errorf("被过滤的任务不该派发任何文件，got %+v", rec.PostDispatched)
	}
	// 名单没变时不该反复重判（否则每轮都要重新发一次文件列表）
	if done, _ := m.handlePostProcess(tor); !done {
		t.Error("名单未变时应保持终结")
	}
	// 放宽名单：不重启、不手改状态文件，同一任务就该补转
	cfg := m.cfg.Get()
	cfg.FileManager.PostProcess.CategoryInclude = "动漫,电影"
	m.cfg.Set(cfg)
	if done, _ := m.handlePostProcess(tor); done {
		t.Error("放宽名单后同一任务应重新参与转码")
	}
	rec = m.cleanState["h"]
	if rec == nil || len(rec.PostDispatched) != 1 || rec.PostFilterList != "" {
		t.Errorf("放宽名单后应派发文件并清掉过滤指纹，got %+v", rec)
	}
}

// 正常转码完成的任务不受「改了过滤名单」影响：不该被重新拖回转码
func TestHandlePostProcess_FilterChangeKeepsSettledTasksSettled(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "clip.mov"), "x")
	m := setupPostProcess(t, models.PostProcessConfig{
		Enabled: true, Command: "echo hi", Extensions: "mov",
	}, qbFiles("clip.mov"))
	tor := models.QBTorrent{Name: "n", Hash: "h", SavePath: dir, Category: "动漫"}
	m.handlePostProcess(tor)
	m.markPostSettled(tor.Hash)
	if rec := m.cleanState["h"]; rec == nil || !rec.PostDone || rec.PostFilterList != "" {
		t.Fatalf("正常终结不该带过滤指纹，got %+v", rec)
	}
	cfg := m.cfg.Get()
	cfg.FileManager.PostProcess.CategoryInclude = "动漫"
	m.cfg.Set(cfg)
	if done, _ := m.handlePostProcess(tor); !done {
		t.Error("转完过的任务不该因为名单改动就被重开")
	}
	if n := len(m.cleanState["h"].PostDispatched); n != 1 {
		t.Errorf("不该重复派发，派发数 = %d", n)
	}
}

// 过滤状态要落盘：重启后仍认得「这个任务是被过滤挡掉的」，且只有名单真改了才解除终结
func TestPostFilterState_PersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "clip.mov"), "x")
	pc := models.PostProcessConfig{
		Enabled: true, Command: "echo hi", Extensions: "mov", CategoryInclude: "动漫",
	}
	f := setupPostFixture(t, pc, qbFiles("clip.mov"))
	tor := models.QBTorrent{Name: "n", Hash: "h", SavePath: dir, Category: "电影"}
	f.m.handlePostProcess(tor)
	f.m.flushPostState()
	if rec := f.m.cleanState["h"]; rec == nil || rec.PostFilterList == "" {
		t.Fatalf("应记下过滤指纹，got %+v", rec)
	}

	// 换个 Manager 读同一份状态文件（模拟重启）
	m2 := f.restart(t, pc)
	if rec := m2.cleanState["h"]; rec == nil || !rec.PostDone || rec.PostFilterList == "" {
		t.Fatalf("状态应已持久化，got %+v", rec)
	}
	if done, _ := m2.handlePostProcess(tor); !done {
		t.Error("重启后被过滤的任务应保持终结")
	}
	// 空格差异不算改名单：不该白白重开
	pc2 := pc
	pc2.CategoryInclude = " 动漫 "
	m2 = f.restart(t, pc2)
	if done, _ := m2.handlePostProcess(tor); !done {
		t.Error("纯空格改动不应解除终结")
	}
	// 真改了名单才重开
	pc3 := pc
	pc3.CategoryInclude = "动漫,电影"
	m2 = f.restart(t, pc3)
	if done, _ := m2.handlePostProcess(tor); done {
		t.Error("放宽名单后应解除终结并派发")
	}
	if rec := m2.cleanState["h"]; rec == nil || len(rec.PostDispatched) != 1 {
		t.Errorf("应派发被漏掉的文件，got %+v", rec)
	}
}

// 名单一改就清空进程内的已跳过表：被标过 done 的任务在这一轮之后能重新评估
func TestEnsurePostFilterRescan(t *testing.T) {
	m := setupPostProcess(t, models.PostProcessConfig{Enabled: true, Command: "echo"}, qbFiles())
	pc := m.cfg.Get().FileManager.PostProcess
	m.ensurePostFilterRescan(pc) // 首轮记下指纹（此时还没有任何任务被标过）
	m.done["h1"] = true
	m.done["h2"] = true
	m.ensurePostFilterRescan(pc) // 名单没变：不能每轮都把表清掉，否则历史任务反复重扫
	if len(m.done) != 2 {
		t.Errorf("名单未变不该清空已跳过表，got %v", m.done)
	}
	pc2 := pc
	pc2.CategoryInclude = "动漫"
	m.ensurePostFilterRescan(pc2)
	if len(m.done) != 0 {
		t.Errorf("名单变化应清空已跳过表，got %v", m.done)
	}
	// 关掉后处理就别白重扫一遍几百个任务
	m.done["h3"] = true
	pc3 := pc2
	pc3.Enabled = false
	m.ensurePostFilterRescan(pc3)
	if len(m.done) != 1 {
		t.Error("后处理未开启时不该清空已跳过表")
	}
}

// 走完整 scan 的过滤闭环：名单挡掉的任务不转码、状态落盘；把名单放宽后同一任务
// 在下一轮扫描里被补转，且只补转一次
func TestScan_FilterSkipsThenBackfillsAfterListChange(t *testing.T) {
	torrents := `[{"hash":"h1","name":"电影任务","state":"stoppedUP","progress":1.0,"category":"电影","tags":"转码","save_path":"{SAVE}","content_path":"{SAVE}"}]`
	m, _, srv, saveDir := setupFilemgr(t, torrents)
	defer srv.Close()
	writeFile(t, filepath.Join(saveDir, "本片.mp4"), "x")

	log := filepath.Join(t.TempDir(), "files.txt")
	pc := models.PostProcessConfig{
		Enabled: true, Extensions: "mp4", Command: "echo \"$FILE\" >> " + shellSingleQuote(log),
		CategoryInclude: "动漫",
	}
	cfg := m.cfg.Get()
	cfg.FileManager.PostProcess = pc
	m.cfg.Set(cfg)

	m.scan()
	if _, err := os.ReadFile(log); err == nil {
		t.Fatal("分类不在包含名单里就不该转码")
	}
	rec := m.cleanState["h1"]
	if rec == nil || !rec.PostDone || rec.PostFilterList == "" {
		t.Fatalf("应记下「因过滤终结」，got %+v", rec)
	}
	if _, err := os.Stat(m.statePath); err != nil {
		t.Errorf("过滤状态应已落盘（%s）：%v", m.statePath, err)
	}
	b, _ := os.ReadFile(m.statePath)
	if !strings.Contains(string(b), `"postFilterList"`) {
		t.Errorf("状态文件里应能看到过滤指纹，got %s", b)
	}

	// 放宽名单：不用重启、不用清状态，下一轮扫描就该补转
	cfg = m.cfg.Get()
	cfg.FileManager.PostProcess.CategoryInclude = "动漫,电影"
	m.cfg.Set(cfg)
	m.scan()
	deadline := time.Now().Add(5 * time.Second)
	var dispatched string
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(log); err == nil {
			dispatched = strings.TrimSpace(string(b))
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if want := filepath.Join(saveDir, "本片.mp4"); dispatched != want {
		t.Fatalf("放宽名单后应补转 %s，实际 %q", want, dispatched)
	}
	// 转完之后再来一轮，不该重复派发
	m.scan()
	m.scan()
	b, _ = os.ReadFile(log)
	if n := len(strings.Split(strings.TrimSpace(string(b)), "\n")); n != 1 {
		t.Errorf("同一个文件不该转两遍，日志有 %d 行：%s", n, b)
	}
}

// 目标解析以 qB 报出的文件列表为准：文件散在共享分类根（旁边几百个别人的文件）时，
// 依然只认本任务自己的视频；qB 的未完成临时名、取不到、越界的文件名都有各自的处理。
func TestPendingPostTargets(t *testing.T) {
	dir := t.TempDir()
	inside := func(n string) string { return filepath.Join(dir, n) }
	writeFile(t, inside("a.mp4"), "x")
	if err := os.MkdirAll(inside("子目录"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, inside("子目录/b.mp4"), "x")
	writeFile(t, inside("0字节.mp4"), "")

	pc := models.PostProcessConfig{Enabled: true, Command: "true", Extensions: "mp4"}
	tests := []struct {
		name      string
		files     string
		want      []string
		wantWait  bool
		wantCount int
	}{
		{"多文件种子只取本任务的视频", qbFiles("a.mp4", "子目录/b.mp4", "封面.jpg"),
			[]string{inside("a.mp4"), inside("子目录/b.mp4")}, false, 2},
		{"!qB 临时名算等待", qbFiles("a.mp4.!qB"), nil, true, 0},
		{"磁盘上取不到算等待", qbFiles("还没落盘.mp4"), nil, true, 0},
		{"0 字节文件算等待", qbFiles("0字节.mp4"), nil, true, 0},
		{"非白名单直接终结（不等待）", qbFiles("封面.jpg"), nil, false, 0},
		{"空文件列表直接终结", "[]", nil, false, 0},
		{"越界文件名忽略", qbFiles("../外面.mp4"), nil, false, 0},
		{"已派发过的文件不再重复", qbFiles("a.mp4"), nil, false, 0},
	}
	for _, tc := range tests {
		m := setupPostProcess(t, pc, tc.files)
		// 「已派发」用例：先把 a.mp4 记进状态，再看它是否被过滤掉
		if tc.name == "已派发过的文件不再重复" {
			m.markPostDispatched("h", inside("a.mp4"))
		}
		got, waiting, err := m.pendingPostTargets(models.QBTorrent{Name: "n", Hash: "h", SavePath: dir}, pc)
		if err != nil {
			t.Errorf("%s：不该报错：%v", tc.name, err)
		}
		if waiting != tc.wantWait {
			t.Errorf("%s：waiting = %v，want %v", tc.name, waiting, tc.wantWait)
		}
		if len(got) != tc.wantCount {
			t.Errorf("%s：targets = %v，want %d 项", tc.name, got, tc.wantCount)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s：targets[%d] = %q，want %q", tc.name, i, got[i], tc.want[i])
			}
		}
	}
}

// postTargetPath：拼接后必须仍在 save_path 之内
func TestPostTargetPath(t *testing.T) {
	cases := []struct {
		save, name, want string
		ok               bool
	}{
		{"/dl/日本", "abc.mp4", "/dl/日本/abc.mp4", true},
		{"/dl/日本", "子目录/abc.mp4", "/dl/日本/子目录/abc.mp4", true},
		{"/dl/日本", "../../etc/passwd.mp4", "", false},
		{"/dl/日本", "abc/../其他.mp4", "/dl/日本/其他.mp4", true},
		{"", "abc.mp4", "", false},
		{"/dl/日本", "", "", false},
	}
	for _, c := range cases {
		got, ok := postTargetPath(c.save, c.name)
		if ok != c.ok || got != c.want {
			t.Errorf("postTargetPath(%q, %q) = (%q, %v)，want (%q, %v)", c.save, c.name, got, ok, c.want, c.ok)
		}
	}
}

// 没有命中白名单的文件时不执行命令，且任务直接终结（不再重试）
func TestHandlePostProcess_SkipByExtension(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "clip.txt"), "x")
	m := setupPostProcess(t, models.PostProcessConfig{
		Enabled: true, Command: "echo hi", Extensions: "mov,mp4",
	}, qbFiles("clip.txt"))
	done, retry := m.handlePostProcess(models.QBTorrent{Name: "n", Hash: "h", SavePath: dir})
	if !done || retry {
		t.Errorf("应直接完成而非暂缓，got done=%v retry=%v", done, retry)
	}
	if rec := m.cleanState["h"]; rec == nil || !rec.PostDone {
		t.Error("应记录 postDone")
	}
}

// 命中白名单就派发：命令在后台异步跑（不阻塞调用方），且本轮不终结（可能还有文件排队）
func TestHandlePostProcess_LaunchesCommand(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "clip.mov"), "x")
	marker := filepath.Join(dir, "ran.flag")
	m := setupPostProcess(t, models.PostProcessConfig{
		Enabled: true, Command: "touch " + shellSingleQuote(marker), Extensions: "mov",
	}, qbFiles("clip.mov"))
	tor := models.QBTorrent{Name: "n", Hash: "h", SavePath: dir}
	if done, _ := m.handlePostProcess(tor); done {
		t.Error("还有文件可能排队时不该标 done")
	}
	rec := m.cleanState["h"]
	if rec == nil || len(rec.PostDispatched) != 1 || rec.PostDispatched[0] != filepath.Join(dir, "clip.mov") {
		t.Fatalf("应记录已派发的文件，got %+v", rec)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("后台命令未落地")
}

// 全局串行：上一条命令还在跑时，第二个文件不该被并发派发，任务留到下轮
func TestHandlePostProcess_Serializes(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.mov"), "x")
	counter := filepath.Join(dir, "count.txt")
	m := setupPostProcess(t, models.PostProcessConfig{
		Enabled: true, Command: "sleep 2 && echo x >> " + shellSingleQuote(counter), Extensions: "mov",
	}, qbFiles("a.mov"))
	if done, _ := m.handlePostProcess(models.QBTorrent{Name: "n", Hash: "h", SavePath: dir}); done {
		t.Error("派发后不该标 done")
	}
	// 闸门还被第一条命令占着：第二个任务必须让路
	done, retry := m.handlePostProcess(models.QBTorrent{Name: "n2", Hash: "h2", SavePath: dir})
	if done || !retry {
		t.Errorf("占用中应暂缓（done=false, retry=true），got (%v, %v)", done, retry)
	}
	if rec := m.cleanState["h2"]; rec != nil && len(rec.PostDispatched) > 0 {
		t.Error("闸门占用时不该派发任何文件")
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if m.tryAcquirePost() {
			m.releasePost()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("命令跑完后闸门应释放")
}

// 一个种子里多个视频：逐个派发，全部派完才终结
func TestHandlePostProcess_DispatchesEveryMatchingFile(t *testing.T) {
	dir := t.TempDir()
	names := []string{"1.mov", "2.mov", "3.mov"}
	for _, n := range names {
		writeFile(t, filepath.Join(dir, n), "x")
	}
	counter := filepath.Join(dir, "count.txt")
	m := setupPostProcess(t, models.PostProcessConfig{
		Enabled: true, Command: "echo x >> " + shellSingleQuote(counter), Extensions: "mov",
	}, qbFiles(names...))
	tor := models.QBTorrent{Name: "n", Hash: "h", SavePath: dir}
	// 串行闸门要放行：等上一条命令跑完再走下一轮
	for round := 0; round < 4; round++ {
		m.handlePostProcess(tor)
		for i := 0; i < 250; i++ {
			if m.tryAcquirePost() {
				m.releasePost()
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if rec := m.cleanState["h"]; rec == nil || !rec.PostDone || len(rec.PostDispatched) != 3 {
		t.Fatalf("三个文件都该派发并最终终结，got %+v", rec)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(counter)
		if strings.Count(string(b), "x") == 3 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	b, _ := os.ReadFile(counter)
	t.Errorf("应各执行一次，实际 %d 次", strings.Count(string(b), "x"))
}

// 重复触发不会重复派发（同一个文件只交给 shell 一次）
func TestHandlePostProcess_OnceOnly(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "clip.mov"), "x")
	counter := filepath.Join(dir, "count.txt")
	m := setupPostProcess(t, models.PostProcessConfig{
		Enabled: true, Command: "echo x >> " + shellSingleQuote(counter), Extensions: "mov",
	}, qbFiles("clip.mov"))
	tor := models.QBTorrent{Name: "n", Hash: "h", SavePath: dir}
	m.handlePostProcess(tor)
	// 派发后先把闸门等回来，再走后面的轮次（否则会被串行挡住，看不出重复派发）
	waitPostIdle(t, m)
	m.handlePostProcess(tor)
	waitPostIdle(t, m)
	m.handlePostProcess(tor) // 第三次已终结
	time.Sleep(300 * time.Millisecond)
	b, _ := os.ReadFile(counter)
	if n := strings.Count(string(b), "x"); n != 1 {
		t.Errorf("同一文件不应重复派发，实际写文件 %d 次", n)
	}
}

// 已终结（postDone）的老记录：直接放行，不再解析、不再派发
func TestHandlePostProcess_LegacySettledStaysSettled(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "clip.mp4"), "x")
	m := setupPostProcess(t, models.PostProcessConfig{
		Enabled: true, Command: "echo x", Extensions: "mp4",
	}, qbFiles("clip.mp4"))
	m.markPostSettled("h")
	if done, _ := m.handlePostProcess(models.QBTorrent{Name: "n", Hash: "h", SavePath: dir}); !done {
		t.Error("已终结的任务应直接返回 done")
	}
}

// 待转文件一直取不到：先反复暂缓，超过轮数上限才终结（不能第一轮就判没活干）
func TestHandlePostProcess_WaitingDefersThenSettles(t *testing.T) {
	dir := t.TempDir()
	m := setupPostProcess(t, models.PostProcessConfig{
		Enabled: true, Command: "echo x", Extensions: "mp4",
	}, qbFiles("还没落盘.mp4"))
	tor := models.QBTorrent{Name: "n", Hash: "h", SavePath: dir}
	for i := 0; i < postMaxWaits-1; i++ {
		if done, retry := m.handlePostProcess(tor); done || !retry {
			t.Fatalf("第 %d 轮应暂缓重试，got done=%v retry=%v", i, done, retry)
		}
	}
	if rec := m.cleanState["h"]; rec != nil && rec.PostDone {
		t.Error("未到上限不该终结")
	}
	if done, _ := m.handlePostProcess(tor); !done {
		t.Error("超过等待上限后应终结，不再空转")
	}
}

// 未开启 / 没配命令时都不该把任务卡住
func TestHandlePostProcess_NoopCases(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.mov"), "x")
	m := setupPostProcess(t, models.PostProcessConfig{}, qbFiles("a.mov"))
	if done, _ := m.handlePostProcess(models.QBTorrent{Hash: "h1", SavePath: dir}); !done {
		t.Error("未开启时应直接完成")
	}
	m2 := setupPostProcess(t, models.PostProcessConfig{Enabled: true}, qbFiles("a.mov"))
	if done, _ := m2.handlePostProcess(models.QBTorrent{Hash: "h2", SavePath: dir}); !done {
		t.Error("没配命令时应直接完成而非反复重试")
	}
}

// waitPostIdle 等后处理串行闸门空出来（最多 5s）
func waitPostIdle(t *testing.T, m *Manager) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if m.tryAcquirePost() {
			m.releasePost()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("后处理闸门未被释放")
}

// 命令失败会退避重试，且成功之前不触发清理
func TestRunPostCommandWithRetry_RetryThenGiveUp(t *testing.T) {
	old := postRetryDelay
	postRetryDelay = 5 * time.Millisecond
	defer func() { postRetryDelay = old }()

	pc := models.PostProcessConfig{Enabled: true, Command: "exit 1", SizePrune: true, Timeout: 30}
	prunes := 0
	ok := runPostCommandWithRetry(models.QBTorrent{Name: "n"}, "/tmp/x.mov", pc, func(_ models.QBTorrent, _ string, _ time.Time) { prunes++ })
	if ok {
		t.Error("命令失败应返回 false")
	}
	if prunes != 0 {
		t.Errorf("命令失败不应触发清理，prunes=%d", prunes)
	}
}

// 总预算用尽时直接放弃：每条尝试 2s、总预算 1s，应该只跑一次就收手，
// 而不是跑满 3 次（跑满要 6s 以上）
func TestRunPostCommandWithRetry_TimeoutBudget(t *testing.T) {
	old := postRetryDelay
	postRetryDelay = 200 * time.Millisecond
	defer func() { postRetryDelay = old }()

	pc := models.PostProcessConfig{Enabled: true, Command: "sleep 2; exit 1", Timeout: 1}
	start := time.Now()
	ok := runPostCommandWithRetry(models.QBTorrent{Name: "n"}, "/tmp/x.mov", pc, nil)
	if ok {
		t.Fatal("应判定失败")
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Errorf("总超时应裁掉重试，实际耗时 %s（疑似跑满了 %d 次尝试）", elapsed, postMaxAttempts)
	}
}

// 命令成功即触发一次清理
func TestRunPostCommandWithRetry_SuccessRunsPrune(t *testing.T) {
	pc := models.PostProcessConfig{Enabled: true, Command: "true", SizePrune: true}
	prunes := 0
	ok := runPostCommandWithRetry(models.QBTorrent{Name: "n"}, "/tmp/x.mov", pc,
		func(_ models.QBTorrent, _ string, _ time.Time) { prunes++ })
	if !ok || prunes != 1 {
		t.Errorf("命令成功应触发一次清理，ok=%v prunes=%d", ok, prunes)
	}
}

// okPtr 构造 *bool：PostProcessConfig.Verify 是指针，缺省（nil）才表示「按默认开启」
func okPtr(b bool) *bool { return &b }

// 产物比原片小 → 删原片留产物（关掉删除前校验时的纯大小判定）
func TestPostPrune_OutputSmaller(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "原片.mov")
	writeFileSized(t, src, 1000)
	out := filepath.Join(dir, "原片.mp4")
	writeFileSized(t, out, 400)
	pc := models.PostProcessConfig{SizePrune: true, Verify: okPtr(false)}
	m := setupPostProcess(t, pc)
	m.postPrune(models.QBTorrent{Name: "n"}, src, time.Now().Add(-time.Second), pc)
	if _, err := os.Stat(src); err == nil {
		t.Error("产物更小，应删除原片")
	}
	if _, err := os.Stat(out); err != nil {
		t.Error("产物应保留")
	}
}

// 产物比原片大 → 保留原片，删掉产物
func TestPostPrune_OutputBigger(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "原片.mov")
	writeFileSized(t, src, 400)
	out := filepath.Join(dir, "原片.mp4")
	writeFileSized(t, out, 1000)
	pc := models.PostProcessConfig{SizePrune: true, Verify: okPtr(false)}
	m := setupPostProcess(t, pc)
	m.postPrune(models.QBTorrent{Name: "n"}, src, time.Now().Add(-time.Second), pc)
	if _, err := os.Stat(src); err != nil {
		t.Error("产物更大，应保留原片")
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("产物应被删除")
	}
}

// 产物过小（疑似半成品）→ 原片、产物都留
func TestPostPrune_TinyOutputKeepsSource(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "原片.mov")
	writeFileSized(t, src, 100000)
	out := filepath.Join(dir, "原片.mp4")
	writeFileSized(t, out, 100) // 远低于 5% 下限
	pc := models.PostProcessConfig{SizePrune: true, Verify: okPtr(false)}
	m := setupPostProcess(t, pc)
	m.postPrune(models.QBTorrent{Name: "n"}, src, time.Now().Add(-time.Second), pc)
	if _, err := os.Stat(src); err != nil {
		t.Error("产物过小不该删原片")
	}
	if _, err := os.Stat(out); err != nil {
		t.Error("过小的产物也不该被删")
	}
}

// 核心诉求：转码失败留下的产物（校验不通过）绝不能顶掉原片。
// 这里产物明明更小（转码被截断的典型样子），但校验器不认它 → 原片必须留着。
// 本机没有 ffprobe 时走的是另一条同样安全的分支：校验器不可用 → 放弃清理。
func TestPostPrune_InvalidOutputKeepsSource(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "原片.mov")
	writeFileSized(t, src, 1000)
	out := filepath.Join(dir, "原片.mp4")
	writeFileSized(t, out, 400) // 更小，但只是字节垃圾不是视频
	pc := models.PostProcessConfig{SizePrune: true, Verify: okPtr(true)}
	m := setupPostProcess(t, pc)
	m.postPrune(models.QBTorrent{Name: "n"}, src, time.Now().Add(-time.Second), pc)
	if _, err := os.Stat(src); err != nil {
		t.Error("产物未通过校验，绝不能删除原片")
	}
}

// 关闭删除前校验时回到纯大小判定：产物更小就直接删原片（老行为，给不需要校验的场景留口子）
func TestPostPrune_VerifyOffUsesSizeOnly(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "原片.mov")
	writeFileSized(t, src, 1000)
	out := filepath.Join(dir, "原片.mp4")
	writeFileSized(t, out, 400)
	pc := models.PostProcessConfig{SizePrune: true, Verify: okPtr(false)}
	m := setupPostProcess(t, pc)
	m.postPrune(models.QBTorrent{Name: "n"}, src, time.Now().Add(-time.Second), pc)
	if _, err := os.Stat(src); err == nil {
		t.Error("校验关闭时应按大小判定删除原片")
	}
}

// 没有新产物 → 一个文件都不动
func TestPostPrune_NoNewOutput(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "原片.mov")
	writeFileSized(t, src, 500)
	pc := models.PostProcessConfig{SizePrune: true}
	m := setupPostProcess(t, pc)
	m.postPrune(models.QBTorrent{Name: "n"}, src, time.Now().Add(-time.Second), pc)
	if _, err := os.Stat(src); err != nil {
		t.Error("没有产物时不应动原片")
	}
}

// 命令开始之前就存在的旧文件不算产物，绝不能据此删原片
func TestPostPrune_IgnoresOlderFiles(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "原片.mov")
	writeFileSized(t, src, 1000)
	old := filepath.Join(dir, "原片.mkv")
	writeFileSized(t, old, 10)
	pc := models.PostProcessConfig{SizePrune: true}
	m := setupPostProcess(t, pc)
	m.postPrune(models.QBTorrent{Name: "n"}, src, time.Now(), pc)
	if _, err := os.Stat(src); err != nil {
		t.Error("旧文件不该被当成产物，从而误删原片")
	}
	if _, err := os.Stat(old); err != nil {
		t.Error("旧文件不该被删除")
	}
}

// 非视频文件（字幕、封面）不算产物
func TestPostPrune_IgnoresNonVideo(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "原片.mov")
	writeFileSized(t, src, 1000)
	sub := filepath.Join(dir, "原片.srt")
	writeFileSized(t, sub, 10)
	pc := models.PostProcessConfig{SizePrune: true}
	m := setupPostProcess(t, pc)
	m.postPrune(models.QBTorrent{Name: "n"}, src, time.Now().Add(-time.Second), pc)
	if _, err := os.Stat(src); err != nil {
		t.Error("没有视频产物时不应删原片")
	}
	if _, err := os.Stat(sub); err != nil {
		t.Error("字幕不该被删")
	}
}

// 散在共享分类根时最危险的情形：同目录里另一个任务刚下完的新视频（体积更小、mtime
// 也更新）不是本次转码的产物。只按「同目录 + 新文件」判产物就会把别人的文件顶成本次
// 产物，进而删掉原片或删掉别人的文件，所以产物必须与原片同名。
func TestPostPrune_IgnoresForeignFileInSharedRoot(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "原片.mov")
	writeFileSized(t, src, 1000)
	foreign := filepath.Join(dir, "隔壁任务刚下完的片子.mp4")
	writeFileSized(t, foreign, 10)
	pc := models.PostProcessConfig{SizePrune: true, Verify: okPtr(false)}
	m := setupPostProcess(t, pc)
	m.postPrune(models.QBTorrent{Name: "n"}, src, time.Now().Add(-time.Second), pc)
	if _, err := os.Stat(src); err != nil {
		t.Error("异名的新文件不该被当成产物，从而删掉原片")
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Error("别的任务的文件绝不能被删")
	}
}

// 纯函数：校验结果与大小组合出的最终取舍
func TestPickPruneAction(t *testing.T) {
	good := probeCandidate{path: "/d/转码后.mp4", size: 400, ok: true, checked: true, detail: "hevc 1920x1080"}
	bad := probeCandidate{path: "/d/坏.mp4", size: 300, ok: false, checked: true, detail: "ffprobe 未解出视频流"}
	tiny := probeCandidate{path: "/d/半成品.mp4", size: 10, ok: true, checked: true, detail: "hevc 1920x1080"}
	unchecked := probeCandidate{path: "/d/未校验.mp4", size: 400, ok: true, detail: ""}

	cases := []struct {
		name     string
		srcSize  int64
		outs     []probeCandidate
		wantKeep string
		wantDrop string
	}{
		{"产物更小且有效 → 删原片", 1000, []probeCandidate{good}, good.path, "/d/原片.mov"},
		{"产物更大但有效 → 删产物留原片", 300, []probeCandidate{good}, "/d/原片.mov", good.path},
		{"产物无效 → 一个都不删", 1000, []probeCandidate{bad}, "", ""},
		{"产物无效且更小 → 仍然不删原片（核心诉求）", 1000, []probeCandidate{bad}, "", ""},
		{"产物过小 → 两边都不删", 100000, []probeCandidate{tiny}, "", ""},
		{"未校验（关闭校验）→ 按大小删原片", 1000, []probeCandidate{unchecked}, unchecked.path, "/d/原片.mov"},
		{"有效产物 + 无效产物 → 仍以有效产物为准", 1000, []probeCandidate{bad, good}, good.path, "/d/原片.mov"},
	}
	for _, c := range cases {
		keep, drop, reason := pickPruneAction("/d/原片.mov", c.srcSize, c.outs)
		if keep != c.wantKeep {
			t.Errorf("%s：keep = %q，want %q", c.name, keep, c.wantKeep)
		}
		got := ""
		if len(drop) > 0 {
			got = drop[0]
		}
		if got != c.wantDrop {
			t.Errorf("%s：drop[0] = %q，want %q（reason=%s）", c.name, got, c.wantDrop, reason)
		}
	}
}

// 正向验证（本机有 ffprobe/ffmpeg 时才跑）：产物是真能解出视频流的 mp4 且更小，
// 这才够格顶掉原片；产物是同样大小的字节垃圾时不顶。
func TestPostPrune_RealMediaValidation(t *testing.T) {
	probe, ok := findProbeBinary("")
	if !ok {
		t.Skip("本机未安装 ffprobe")
	}
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("本机未安装 ffmpeg，无法造真实视频样片")
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "原片.mp4")
	genVideo(t, ff, out)
	outSize := func() int64 { fi, _ := os.Stat(out); return fi.Size() }()

	// 原片取产物体积的 4 倍+1KB：既保证「产物更小」，又远高于 5% 的半成品下限
	srcSize := outSize*4 + 1024
	src := filepath.Join(dir, "原片.mov")
	writeFileSized(t, src, srcSize)

	// 1) 有效产物 + 更小 → 删原片留产物
	pc := models.PostProcessConfig{SizePrune: true, Verify: okPtr(true)}
	m := setupPostProcess(t, pc)
	m.postPrune(models.QBTorrent{Name: "n"}, src, time.Now().Add(-time.Second), pc)
	if _, err := os.Stat(src); err == nil {
		t.Error("产物通过 ffprobe 校验且更小，应删除原片")
	}
	if _, err := os.Stat(out); err != nil {
		t.Error("产物应保留")
	}

	// 2) 校验器指向不存在的路径 → 校验器不可用，必须放弃清理（原片留着）
	src2 := filepath.Join(dir, "原片2.mov")
	writeFileSized(t, src2, outSize*4+1024)
	out2 := filepath.Join(dir, "原片2.mp4")
	genVideo(t, ff, out2)
	pc2 := models.PostProcessConfig{SizePrune: true, Verify: okPtr(true), ProbePath: "/nonexistent/ffprobe"}
	m2 := setupPostProcess(t, pc2)
	m2.postPrune(models.QBTorrent{Name: "n2"}, src2, time.Now().Add(-time.Second), pc2)
	if _, err := os.Stat(src2); err != nil {
		t.Error("校验器不可用时不能删原片")
	}
	_ = probe
}

// 完整串联真实链路：命令把原片复制成产物 → 命令返回 0 → 回调里用真 ffprobe 校验 →
// 判定产物可用且更小 → 删原片。校验这层要是被绕过，这里就会留下原片而测试失败。
func TestPostProcess_FullChainDeletesSourceWhenProbePasses(t *testing.T) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("本机未安装 ffmpeg")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "原片.mov")
	master := filepath.Join(dir, "样片.mp4") // 命令从这份真视频"转"出产物
	out := filepath.Join(dir, "原片.mp4")
	genVideo(t, ff, master)
	outSize := func() int64 { fi, _ := os.Stat(master); return fi.Size() }()
	if err := os.WriteFile(src, make([]byte, outSize*4+4096), 0o644); err != nil {
		t.Fatal(err)
	}
	probe, ok := findProbeBinary("")
	if !ok {
		t.Skip("本机未安装 ffprobe")
	}
	pc := models.PostProcessConfig{
		Enabled:   true,
		Command:   "cp " + shellSingleQuote(master) + " " + shellSingleQuote(out),
		SizePrune: true,
		Verify:    okPtr(true),
		ProbePath: probe,
	}
	m := setupPostProcess(t, pc)
	okRun := runPostCommandWithRetry(models.QBTorrent{Name: "n", Hash: "h"}, src, pc,
		func(tt models.QBTorrent, s string, started time.Time) {
			m.postPrune(tt, s, started, pc)
		})
	if !okRun {
		t.Fatal("命令应执行成功")
	}
	if _, err := os.Stat(src); err == nil {
		t.Error("产物通过 ffprobe 校验且更小，全链路跑通后应删除原片")
	}
	if _, err := os.Stat(out); err != nil {
		t.Error("产物应保留")
	}
}

// genVideo 用 ffmpeg 造一个 1 秒可解码的小视频，作为「转码产物」样片
func genVideo(t *testing.T, ffmpeg, out string) {
	t.Helper()
	cmd := exec.Command(ffmpeg, "-y", "-v", "error",
		"-f", "lavfi", "-i", "testsrc=d=1:s=320x240:r=10",
		"-c:v", "mpeg4", "-pix_fmt", "yuv420p", out)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		t.Skipf("ffmpeg 造样片失败（本机可能缺编码器）：%v", err)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatal("样片未生成")
	}
}

// findProbeBinary：显式路径优先，给错路径就直接判「校验器不可用」
func TestFindProbeBinary(t *testing.T) {
	if _, ok := findProbeBinary("/nonexistent/ffprobe"); ok {
		t.Error("不存在的路径不该被当成可用校验器")
	}
	// 本机若有 ffprobe（PATH 或常见路径），空串探测应该找得到
	if p, ok := findProbeBinary(""); ok {
		if p == "" {
			t.Error("探测到 ffprobe 但返回空路径")
		}
	}
}

// 校验器对垃圾文件的判定：要么找到 ffprobe 判 invalid，要么压根找不到校验器，
// 两条路都得保证「不删原片」，不能出现"既判 invalid 又找不到"的自相矛盾
func TestProbeInvalidFile(t *testing.T) {
	dir := t.TempDir()
	junk := filepath.Join(dir, "坏文件.mp4")
	writeFileSized(t, junk, 4096)
	bin, ok := findProbeBinary("")
	if !ok {
		t.Skip("本机未安装 ffprobe，跳过真实校验断言")
	}
	valid, detail := probeVideoFile(bin, junk)
	if valid {
		t.Errorf("垃圾文件不该被判为有效视频（detail=%s）", detail)
	}
	if !strings.Contains(detail, "视频") && !strings.Contains(detail, "失败") && !strings.Contains(detail, "解析") {
		t.Errorf("失败原因描述不清晰：%q", detail)
	}
}

// 核心保障：归档「还在异步移动中」时绝不能跑后处理，否则就是把归档前的旧路径
// 拿去转码。这里让原目录里仍留着文件（模拟 qB 移动未落地），跑一轮 scan，
// 命令不该被执行；等目录真的空了（移动落地）再跑一轮，命令才该被触发。
func TestScan_WaitsForFlattenBeforePostProcess(t *testing.T) {
	torrents := `[{"hash":"h1","name":"存档","state":"stoppedUP","progress":1.0,"save_path":"{SAVE}"}]`
	m, _, srv, saveDir := setupFilemgr(t, torrents)
	defer srv.Close()
	tvDir := filepath.Join(saveDir, "存档")
	if err := os.MkdirAll(tvDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(tvDir, "电影.mov"), "x")

	cfg := m.cfg.Get()
	cfg.FileManager.PostProcess = models.PostProcessConfig{
		Enabled: true, Command: "true", Extensions: "mov",
	}
	m.cfg.Set(cfg)
	// 预置「已提交 qB 异步移动、待清理原目录」状态，让本轮 scan 走 finishFlatten
	m.setFlattenDir("h1", tvDir)

	marker := filepath.Join(t.TempDir(), "ran.flag")
	cfg.FileManager.PostProcess.Command = "touch " + shellSingleQuote(marker)
	m.cfg.Set(cfg)

	m.scan() // 移动未落地：原目录仍有文件
	for i := 0; i < 30; i++ {
		if _, err := os.Stat(marker); err == nil {
			t.Fatal("归档未完成就跑了后处理，转码可能作用在移动前的路径上")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 模拟 qB 移动落地：文件到了保存根、原目录被清掉，下一轮就该触发了
	os.RemoveAll(tvDir)
	writeFile(t, filepath.Join(saveDir, "电影.mov"), "x")
	m.scan()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("归档落地后应触发后处理")
}

// 线上最常见的形态：文件直接散在共享分类根（content_path == save_path，同一目录里
// 还躺着别的任务的几百个文件）。旧实现靠「目录里恰好一个可见文件」猜目标，这种形态
// 一个都认不出来（生产日志里 628 次全是「未解析到唯一目标文件」，且每次跳过都把任务
// 永久标 done）。改成按 qB 报的任务文件列表认目标后，这一轮就该真正派发命令。
func TestScan_PostProcessFiresInSharedCategoryRoot(t *testing.T) {
	torrents := `[{"hash":"h1","name":"散在分类根的任务","state":"stoppedUP","progress":1.0,"save_path":"{SAVE}","content_path":"{SAVE}"}]`
	m, _, srv, saveDir := setupFilemgr(t, torrents)
	defer srv.Close()
	// 本任务的目标（命中白名单）
	writeFile(t, filepath.Join(saveDir, "本片.mp4"), "x")
	// 同目录的邻居：非白名单的视频与图片，绝不该被拿去转码
	writeFile(t, filepath.Join(saveDir, "邻居.mkv"), "x")
	writeFile(t, filepath.Join(saveDir, "封面.jpg"), "x")

	cfg := m.cfg.Get()
	marker := filepath.Join(t.TempDir(), "ran.flag")
	log := filepath.Join(t.TempDir(), "files.txt")
	cfg.FileManager.PostProcess = models.PostProcessConfig{
		Enabled:    true,
		Command:    "echo \"$FILE\" >> " + shellSingleQuote(log) + " && touch " + shellSingleQuote(marker),
		Extensions: "mp4",
	}
	m.cfg.Set(cfg)

	m.scan()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("散在共享分类根的任务也应触发后处理")
	}
	b, _ := os.ReadFile(log)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 1 || lines[0] != filepath.Join(saveDir, "本片.mp4") {
		t.Errorf("只该转码白名单内的本任务文件，实际派发：%q", string(b))
	}
	// 派发完还没终结（终结要等下一轮确认没有更多文件），下轮 scan 后应终结
	m.scan()
	if rec := m.cleanState["h1"]; rec == nil || !rec.PostDone {
		t.Errorf("所有文件派发完、无更多匹配文件后应终结，got %+v", rec)
	}
}

// 手动测试运行：未启用 / 没配命令 / 没填路径都要给出明确报错
func TestRunPostProcessTest_Validation(t *testing.T) {
	m := setupPostProcess(t, models.PostProcessConfig{})
	if _, err := m.RunPostProcessTest("/tmp/x.mov", "", ""); err == nil {
		t.Error("未启用时应报错")
	}
	m2 := setupPostProcess(t, models.PostProcessConfig{Enabled: true})
	if _, err := m2.RunPostProcessTest("/tmp/x.mov", "", ""); err == nil {
		t.Error("没配命令时应报错")
	}
	if _, err := m2.RunPostProcessTest("", "", ""); err == nil {
		t.Error("没填测试文件时应报错")
	}
}

// 测试运行的模拟分类/标签要真的传进命令，并且回报「按当前名单这组会不会被转码」；
// 判定只是回报，命令本身照样跑（否则没法验证脚本里的分类/标签分支）
func TestRunPostProcessTest_SimulatesCategoryTags(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "片源.mov")
	writeFile(t, src, "x")
	m := setupPostProcess(t, models.PostProcessConfig{
		Enabled: true, Command: `printf '%s|%s' "$CATEGORY" "$TAGS"`,
		CategoryInclude: "动漫", TagExclude: "真人",
	})
	res, err := m.RunPostProcessTest(src, "动漫", "真人,1080p")
	if err != nil || res.Code != 0 {
		t.Fatalf("测试运行失败：code=%d err=%v out=%s", res.Code, err, res.Output)
	}
	if res.Output != "动漫|真人,1080p" {
		t.Errorf("$CATEGORY/$TAGS 没拿到模拟值：%q", res.Output)
	}
	if !res.Filtered || !strings.Contains(res.Reason, "标签") {
		t.Errorf("带排除标签应回报不转码，got filtered=%v reason=%q", res.Filtered, res.Reason)
	}
	res2, err := m.RunPostProcessTest(src, "动漫", "转码")
	if err != nil || res2.Code != 0 {
		t.Fatalf("测试运行失败：%v", err)
	}
	if res2.Filtered {
		t.Errorf("命中包含名单应回报会转码，reason=%q", res2.Reason)
	}
}

// 手动测试运行能真正把带空格的中文路径喂进去（用 cp 造个副本侧面验证参数可解析）
func TestRunPostProcessTest_RunsCommand(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "输入 文件.mov")
	writeFile(t, src, "payload")
	dst := filepath.Join(dir, "副本.mov")
	m := setupPostProcess(t, models.PostProcessConfig{
		Enabled: true, Command: "cp \"$FILE\" \"" + dst + "\"",
	})
	res, err := m.RunPostProcessTest(src, "", "")
	if err != nil || res.Code != 0 {
		t.Fatalf("测试运行失败：code=%d err=%v out=%s", res.Code, err, res.Output)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Error("测试命令未生效")
	}
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeFileSized(t *testing.T, p string, size int64) {
	t.Helper()
	if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}
