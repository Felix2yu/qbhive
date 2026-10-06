package filemgr

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Felix2yu/qbhive/internal/config"
	"github.com/Felix2yu/qbhive/internal/models"
	qb "github.com/Felix2yu/qbhive/internal/qbittorrent"
)

// setupPostProcess 返回一个后处理用的 Manager：qB 客户端走最小 mock（后处理用例
// 不触发归档），持久化状态与配置都落在临时目录里，用例之间互不干扰。
func setupPostProcess(t *testing.T, pc models.PostProcessConfig) *Manager {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("QBHIVE_CONFIG", cfgPath)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Set-Cookie", "SID=test")
		w.Write([]byte("Ok."))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	m := New(config.New(cfgPath), qb.New(srv.URL, "u", "p", ""))
	cfg := m.cfg.Get()
	cfg.FileManager.PostProcess = pc
	m.cfg.Set(cfg)
	return m
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
	tor := models.QBTorrent{Name: "任务 A", Category: "电影", SavePath: "/dl/movie"}
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
	if got, _ := renderPostCommand("echo {name} {category} {dir} {savepath}", tor, "/tmp/a.mov"); got != "echo '任务 A' '电影' '/tmp' '/dl/movie'" {
		t.Errorf("占位符替换结果不对：%s", got)
	}
	if _, err := renderPostCommand("   ", tor, "/x"); err == nil {
		t.Error("空命令应报错")
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

func TestResolveTarget(t *testing.T) {
	dir := t.TempDir()
	single := filepath.Join(dir, "only.mkv")
	writeFile(t, single, "x")
	// 多文件目录不该被当成目标（宁可跳过也不猜路径）
	multi := t.TempDir()
	writeFile(t, filepath.Join(multi, "1.mkv"), "x")
	writeFile(t, filepath.Join(multi, "2.mkv"), "x")

	tests := []struct {
		name string
		t    models.QBTorrent
		want string
		ok   bool
	}{
		{"content_path 是文件（单文件种子）", models.QBTorrent{ContentPath: single}, single, true},
		{"目录内唯一文件（归档落点）", models.QBTorrent{ContentPath: dir, SavePath: dir}, single, true},
		{"savePath 回退", models.QBTorrent{SavePath: dir}, single, true},
		{"目录内多个文件", models.QBTorrent{ContentPath: multi}, "", false},
		{"目录不存在", models.QBTorrent{ContentPath: filepath.Join(dir, "nope")}, "", false},
		{"无路径", models.QBTorrent{}, "", false},
	}
	m := setupPostProcess(t, models.PostProcessConfig{})
	for _, tc := range tests {
		got, ok := m.resolveTarget(tc.t)
		if ok != tc.ok || (tc.ok && got != tc.want) {
			t.Errorf("%s：resolveTarget → (%q, %v)，want (%q, %v)", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

// 扩展名不匹配时不执行命令，且任务直接标记 postDone（不再重试）
func TestHandlePostProcess_SkipByExtension(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "clip.txt")
	writeFile(t, f, "x")
	m := setupPostProcess(t, models.PostProcessConfig{
		Enabled: true, Command: "echo hi", Extensions: "mov,mp4",
	})
	done, retry := m.handlePostProcess(models.QBTorrent{Name: "n", Hash: "h", ContentPath: dir})
	if !done || retry {
		t.Errorf("应直接完成而非暂缓，got done=%v retry=%v", done, retry)
	}
	if rec := m.cleanState["h"]; rec == nil || !rec.PostDone {
		t.Error("应记录 postDone")
	}
}

// 命令正常启动时立即标记 postDone，命令在后台异步跑（不阻塞调用方）
func TestHandlePostProcess_LaunchesCommand(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "clip.mov"), "x")
	marker := filepath.Join(dir, "ran.flag")
	m := setupPostProcess(t, models.PostProcessConfig{
		Enabled: true, Command: "touch " + shellSingleQuote(marker), Extensions: "mov",
	})
	_, _ = m.handlePostProcess(models.QBTorrent{Name: "n", Hash: "h", ContentPath: dir})
	if rec := m.cleanState["h"]; rec == nil || !rec.PostDone {
		t.Fatal("启动后应立刻标记 postDone")
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

// 重复触发不会重复派发（重试用例之外的路径：命令是"写过就算"的计数器文件）
func TestHandlePostProcess_OnceOnly(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "clip.mov"), "x")
	counter := filepath.Join(dir, "count.txt")
	m := setupPostProcess(t, models.PostProcessConfig{
		Enabled: true, Command: "echo x >> " + shellSingleQuote(counter), Extensions: "mov",
	})
	tor := models.QBTorrent{Name: "n", Hash: "h", ContentPath: dir}
	m.handlePostProcess(tor)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(counter); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	m.handlePostProcess(tor) // 第二次应被 PostDone 挡住
	time.Sleep(500 * time.Millisecond)
	b, _ := os.ReadFile(counter)
	if n := strings.Count(string(b), "x"); n != 1 {
		t.Errorf("PostDone 后不应重复派发，实际写文件 %d 次", n)
	}
}

// 未开启 / 没配命令时都不该把任务卡住
func TestHandlePostProcess_NoopCases(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.mov"), "x")
	m := setupPostProcess(t, models.PostProcessConfig{})
	if done, _ := m.handlePostProcess(models.QBTorrent{ContentPath: dir}); !done {
		t.Error("未开启时应直接完成")
	}
	m2 := setupPostProcess(t, models.PostProcessConfig{Enabled: true})
	if done, _ := m2.handlePostProcess(models.QBTorrent{ContentPath: dir}); !done {
		t.Error("没配命令时应直接完成而非反复重试")
	}
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
	out := filepath.Join(dir, "转码后.mov")
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
	out := filepath.Join(dir, "转码后.mov")
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
	out := filepath.Join(dir, "半成品.mov")
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
	out := filepath.Join(dir, "转码后.mov")
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
	out := filepath.Join(dir, "转码后.mov")
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
	old := filepath.Join(dir, "以前就有的.mov")
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
	sub := filepath.Join(dir, "转码后.srt")
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
	out := filepath.Join(dir, "转码后.mp4")
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
	out2 := filepath.Join(dir, "转码后2.mp4")
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
	out := filepath.Join(dir, "转码后.mp4")
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

// 手动测试运行：未启用 / 没配命令 / 没填路径都要给出明确报错
func TestRunPostProcessTest_Validation(t *testing.T) {
	m := setupPostProcess(t, models.PostProcessConfig{})
	if _, _, err := m.RunPostProcessTest("/tmp/x.mov"); err == nil {
		t.Error("未启用时应报错")
	}
	m2 := setupPostProcess(t, models.PostProcessConfig{Enabled: true})
	if _, _, err := m2.RunPostProcessTest("/tmp/x.mov"); err == nil {
		t.Error("没配命令时应报错")
	}
	if _, _, err := m2.RunPostProcessTest(""); err == nil {
		t.Error("没填测试文件时应报错")
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
	code, out, err := m.RunPostProcessTest(src)
	if err != nil || code != 0 {
		t.Fatalf("测试运行失败：code=%d err=%v out=%s", code, err, out)
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
