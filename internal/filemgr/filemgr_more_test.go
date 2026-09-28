package filemgr

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Felix2yu/qbhive/internal/config"
	"github.com/Felix2yu/qbhive/internal/models"
	qb "github.com/Felix2yu/qbhive/internal/qbittorrent"
)

// filemgrEnv 是可定制的 qB mock：torrents 内容与 stop/renameFile/start 的状态码都能动态改，
// 用于覆盖 handleCompleted 的成功/回退/失败等分支。
type filemgrEnv struct {
	m   *Manager
	srv *httptest.Server

	mu           sync.Mutex
	torrentsJSON string
	stopStatus  int
	renameStatus int
	startStatus int
	callLog      []string
	infoCalls    int
	saveDir      string
}

func newFilemgrEnv(t *testing.T, enabled bool, scanInterval int, torrentsJSON string) *filemgrEnv {
	t.Helper()
	env := &filemgrEnv{torrentsJSON: torrentsJSON, saveDir: t.TempDir()}

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
		_, _ = w.Write([]byte(strings.ReplaceAll(body, "{SAVE}", env.saveDir)))
	})
	record := func(kind string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm()
			id := r.FormValue("hashes")
			if id == "" {
				id = r.FormValue("hash")
			}
			env.mu.Lock()
			env.callLog = append(env.callLog, kind+":"+id)
			st := 0
			if kind == "renameFile" {
				st = env.renameStatus
			} else if kind == "stop" {
				st = env.stopStatus
			} else if kind == "start" {
				st = env.startStatus
			}
			env.mu.Unlock()
			if st == 0 {
				st = http.StatusOK
			}
			w.WriteHeader(st)
		}
	}
	mux.HandleFunc("/api/v2/torrents/stop", record("stop"))
	mux.HandleFunc("/api/v2/torrents/renameFile", record("renameFile"))
	mux.HandleFunc("/api/v2/torrents/start", record("start"))

	env.srv = httptest.NewServer(mux)
	t.Cleanup(env.srv.Close)

	cfgPath := filepath.Join(t.TempDir(), "config.json")
	// 隔离持久化状态：cleanState / audit 路径取自 QBHIVE_CONFIG 所在目录，
	// 否则会共用相对 data/ 文件，归档 pending 记录会跨测试串味。
	t.Setenv("QBHIVE_CONFIG", cfgPath)
	cfg := config.New(cfgPath)
	cfg.Set(models.AppConfig{
		FileManager: models.FileManagerConfig{Enabled: enabled, ScanInterval: scanInterval},
	})
	env.m = New(cfg, qb.New(env.srv.URL, "u", "p", ""))
	return env
}

func (e *filemgrEnv) setTorrents(json string) {
	e.mu.Lock()
	e.torrentsJSON = json
	e.mu.Unlock()
}

func (e *filemgrEnv) calls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.callLog))
	copy(out, e.callLog)
	return out
}

func (e *filemgrEnv) infoCallCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.infoCalls
}

func (e *filemgrEnv) callCount(kind string) int {
	n := 0
	for _, c := range e.calls() {
		if strings.HasPrefix(c, kind+":") {
			n++
		}
	}
	return n
}

// waitForFilemgr 轮询直到 fn 为真，上限 800ms
func waitForFilemgr(t *testing.T, fn func() bool) bool {
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

// settleFilemgr 给后台 ticker goroutine 一点退出时间
func settleFilemgr() { time.Sleep(80 * time.Millisecond) }

// skipIfRoot 权限位测试在 root 下不生效（CI 容器常为 root）
func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root 下 chmod 权限位不生效，跳过")
	}
}

// ============ Start / Stop / Reset ============

func TestManager_Start_EnabledSpawnsTicker(t *testing.T) {
	env := newFilemgrEnv(t, true, 15, `[]`)
	env.m.Start()
	if env.m.tickerStop == nil {
		t.Fatal("Start 后应创建 tickerStop")
	}
	env.m.Stop()
	settleFilemgr()
}

func TestManager_Start_DisabledNoTicker(t *testing.T) {
	env := newFilemgrEnv(t, false, 15, `[]`)
	env.m.Start()
	if env.m.tickerStop != nil {
		t.Error("停用时 Start 不应创建 ticker")
	}
}

func TestManager_Start_NonPositiveIntervalUsesDefault(t *testing.T) {
	env := newFilemgrEnv(t, true, 0, `[]`)
	// ScanInterval<=0 走默认 15s，不应 panic
	env.m.Start()
	if env.m.tickerStop == nil {
		t.Fatal("ScanInterval=0 也应按默认间隔启动 ticker")
	}
	env.m.Stop()
	settleFilemgr()
}

func TestManager_Stop_ExitTickerGoroutine(t *testing.T) {
	env := newFilemgrEnv(t, true, 15, `[]`)
	env.m.Start()
	env.m.Stop() // close(m.stop) → goroutine 走 <-m.stop 分支退出
	settleFilemgr()
}

func TestManager_Reset_ClearsDoneMap(t *testing.T) {
	env := newFilemgrEnv(t, true, 15, `[]`)
	env.m.done["h1"] = true
	env.m.done["h2"] = true
	env.m.Reset()
	if len(env.m.done) != 0 {
		t.Errorf("Reset 后 done 应为空: %v", env.m.done)
	}
}

// ============ Reload ============

func TestManager_Reload_RestartsRunningTicker(t *testing.T) {
	env := newFilemgrEnv(t, true, 15, `[]`)
	env.m.Start()
	old := env.m.tickerStop

	cfg := env.m.cfg.Get()
	cfg.FileManager.ScanInterval = -1 // 触发 Reload 里的默认间隔分支
	env.m.cfg.Set(cfg)
	env.m.Reload(nil) // 不传 client，避免与后台 goroutine 并发写 m.client

	if env.m.tickerStop == nil || env.m.tickerStop == old {
		t.Error("Reload 应关闭旧 ticker 并重建")
	}
	env.m.Stop()
	settleFilemgr()
}

func TestManager_Reload_DisabledStopsTicker(t *testing.T) {
	env := newFilemgrEnv(t, true, 15, `[]`)
	env.m.Start()
	cfg := env.m.cfg.Get()
	cfg.FileManager.Enabled = false
	env.m.cfg.Set(cfg)

	env.m.Reload(nil)
	if env.m.tickerStop != nil {
		t.Error("停用后 Reload 不应保留 tickerStop")
	}
}

func TestManager_Reload_WithNewClient(t *testing.T) {
	env := newFilemgrEnv(t, false, 15, `[]`)
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "SID=test")
		_, _ = w.Write([]byte("Ok."))
	}))
	defer srv2.Close()

	newCli := qb.New(srv2.URL, "u2", "p2", "")
	env.m.Reload(newCli)
	if env.m.client != newCli {
		t.Error("Reload 应热替换 client 指针")
	}
}

// TestManager_RunTicker_TriggersScan 覆盖 runTicker 的 ticker.C 分支：
// 空列表下 scan 不写 m.done，主线程也不读它，因此 Stop 收尾无竞争。
func TestManager_RunTicker_TriggersScan(t *testing.T) {
	env := newFilemgrEnv(t, true, 15, `[]`)
	env.m.runTicker(50 * time.Millisecond)
	if !waitForFilemgr(t, func() bool { return env.infoCallCount() > 0 }) {
		t.Fatal("ticker 应触发 scan 并请求 torrent 列表")
	}
	env.m.Stop()
	settleFilemgr()
}

// TestManager_RunTicker_ReloadStopsIdleTicker 覆盖 runTicker 的 <-stop 分支：
// 1 小时间隔保证 goroutine 没跑过 scan，再用 Reload 关闭 tickerStop 唤醒它。
func TestManager_RunTicker_ReloadStopsIdleTicker(t *testing.T) {
	env := newFilemgrEnv(t, true, 15, `[]`)
	env.m.runTicker(time.Hour)
	if env.m.tickerStop == nil {
		t.Fatal("runTicker 应创建 tickerStop")
	}
	env.m.Reload(nil)
	if env.m.tickerStop == nil {
		t.Fatal("Reload 后应重建 tickerStop")
	}
	settleFilemgr()
	env.m.Stop()
	settleFilemgr()
}

// ============ handleCompleted 分支 ============

// torrent 短 hash 至少 8 位（回退改名会用到 Hash[:8]）
const testHash = "h1h2h3h4h5h6"

// TestManager_handleCompleted_NotADirReturnsEarly 覆盖 torrentDir 不是目录时的提前返回
func TestManager_handleCompleted_NotADirReturnsEarly(t *testing.T) {
	env := newFilemgrEnv(t, false, 15, `[]`)
	// 让 savePath/Name 直接指向一个普通文件
	if err := os.WriteFile(filepath.Join(env.saveDir, "Movie"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	env.m.handleCompleted(models.QBTorrent{Hash: testHash, Name: "Movie", SavePath: env.saveDir})
	if len(env.calls()) != 0 {
		t.Errorf("非目录场景不应调用 qB 接口: %v", env.calls())
	}
}

// TestManager_handleCompleted_ReadDirErrorReturnsEarly 覆盖 os.ReadDir 失败分支
func TestManager_handleCompleted_ReadDirErrorReturnsEarly(t *testing.T) {
	skipIfRoot(t)
	env := newFilemgrEnv(t, false, 15, `[]`)
	torrentDir := filepath.Join(env.saveDir, "Movie")
	if err := os.MkdirAll(torrentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(torrentDir, "video.mkv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 去掉目录的 r-x 权限 → ReadDir 必然失败
	if err := os.Chmod(torrentDir, 0o000); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(torrentDir, 0o755) }()

	env.m.handleCompleted(models.QBTorrent{Hash: testHash, Name: "Movie", SavePath: env.saveDir})
	if len(env.calls()) != 0 {
		t.Errorf("ReadDir 失败时不应调用 qB 接口: %v", env.calls())
	}
}

// TestManager_handleCompleted_HiddenFilesFiltered 覆盖隐藏文件过滤：
// 目录里只有 1 个常规文件 + 若干隐藏文件时仍按单文件场景处理。
func TestManager_handleCompleted_HiddenFilesFiltered(t *testing.T) {
	env := newFilemgrEnv(t, false, 15, `[]`)
	torrentDir := filepath.Join(env.saveDir, "Movie")
	if err := os.MkdirAll(torrentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".DS_Store", ".hidden", "video.mkv"} {
		if err := os.WriteFile(filepath.Join(torrentDir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	env.m.handleCompleted(models.QBTorrent{Hash: testHash, Name: "Movie", SavePath: env.saveDir})
	if env.callCount("renameFile") != 1 {
		t.Errorf("隐藏文件应被过滤，期望走单文件 renameFile 路径: %v", env.calls())
	}
	if env.callCount("start") != 1 {
		t.Errorf("renameFile 成功后应启动任务: %v", env.calls())
	}
}

// TestManager_handleCompleted_MultipleFilesSkipped 覆盖「不是单文件场景」的提前返回
func TestManager_handleCompleted_MultipleFilesSkipped(t *testing.T) {
	env := newFilemgrEnv(t, false, 15, `[]`)
	torrentDir := filepath.Join(env.saveDir, "Movie")
	if err := os.MkdirAll(torrentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.mkv", "b.mkv"} {
		if err := os.WriteFile(filepath.Join(torrentDir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	env.m.handleCompleted(models.QBTorrent{Hash: testHash, Name: "Movie", SavePath: env.saveDir})
	if len(env.calls()) != 0 {
		t.Errorf("多文件场景应提前返回: %v", env.calls())
	}
}

// TestManager_handleCompleted_SubDirNotRegular 覆盖「唯一条目是子目录（非常规文件）」的提前返回
func TestManager_handleCompleted_SubDirNotRegular(t *testing.T) {
	env := newFilemgrEnv(t, false, 15, `[]`)
	torrentDir := filepath.Join(env.saveDir, "Movie")
	if err := os.MkdirAll(filepath.Join(torrentDir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	env.m.handleCompleted(models.QBTorrent{Hash: testHash, Name: "Movie", SavePath: env.saveDir})
	if len(env.calls()) != 0 {
		t.Errorf("子目录不满足 IsRegular，应提前返回: %v", env.calls())
	}
}

// TestManager_handleCompleted_RenameFileOK 覆盖 qB renameFile 成功路径：
// stop → renameFile → start；mock 不真搬文件，所以最后 os.Remove 空目录失败（正常分支）。
func TestManager_handleCompleted_RenameFileOK(t *testing.T) {
	env := newFilemgrEnv(t, false, 15, `[]`)
	torrentDir := filepath.Join(env.saveDir, "Movie")
	if err := os.MkdirAll(torrentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(torrentDir, "video.mkv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	env.m.handleCompleted(models.QBTorrent{Hash: testHash, Name: "Movie", SavePath: env.saveDir})

	calls := env.calls()
	if len(calls) != 3 || !strings.HasPrefix(calls[0], "stop:") ||
		!strings.HasPrefix(calls[1], "renameFile:") || !strings.HasPrefix(calls[2], "start:") {
		t.Errorf("期望 stop→renameFile→start，got %v", calls)
	}
	// renameFile 只是 mock，文件仍在原目录 → os.Remove 会失败但不影响流程
	if _, err := os.Stat(filepath.Join(torrentDir, "video.mkv")); err != nil {
		t.Errorf("mock renameFile 不会真搬文件，源文件应还在: %v", err)
	}
}

// TestManager_handleCompleted_FallbackLocalRename 覆盖 renameFile 失败后的本地回退成功路径，
// 同时覆盖 stop 失败、start 失败、os.Remove 成功三个分支。
func TestManager_handleCompleted_FallbackLocalRename(t *testing.T) {
	env := newFilemgrEnv(t, false, 15, `[]`)
	env.mu.Lock()
	env.stopStatus = http.StatusInternalServerError
	env.renameStatus = http.StatusInternalServerError
	env.startStatus = http.StatusInternalServerError
	env.mu.Unlock()

	torrentDir := filepath.Join(env.saveDir, "Movie")
	if err := os.MkdirAll(torrentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(torrentDir, "video.mkv"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	env.m.handleCompleted(models.QBTorrent{Hash: testHash, Name: "Movie", SavePath: env.saveDir})

	calls := env.calls()
	if len(calls) != 3 || !strings.HasPrefix(calls[0], "stop:") ||
		!strings.HasPrefix(calls[1], "renameFile:") || !strings.HasPrefix(calls[2], "start:") {
		t.Errorf("期望 stop→renameFile→start（即便都失败也要继续），got %v", calls)
	}
	// 本地回退应把文件搬到 savePath 下
	moved := filepath.Join(env.saveDir, "video.mkv")
	data, err := os.ReadFile(moved)
	if err != nil {
		t.Fatalf("回退后文件应在 savePath 下: %v", err)
	}
	if string(data) != "data" {
		t.Errorf("文件内容被改动: %q", data)
	}
	// 源目录应被清掉
	if _, err := os.Stat(torrentDir); err == nil {
		t.Error("空的 torrent 目录应被删除")
	}
}

// TestManager_handleCompleted_FallbackTargetExists 覆盖「回退目标已存在 → 改成 base_hash8.ext」
func TestManager_handleCompleted_FallbackTargetExists(t *testing.T) {
	env := newFilemgrEnv(t, false, 15, `[]`)
	env.mu.Lock()
	env.renameStatus = http.StatusInternalServerError
	env.mu.Unlock()

	torrentDir := filepath.Join(env.saveDir, "Movie")
	if err := os.MkdirAll(torrentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(torrentDir, "video.mkv"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	// savePath 下已存在同名文件
	existing := filepath.Join(env.saveDir, "video.mkv")
	if err := os.WriteFile(existing, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	env.m.handleCompleted(models.QBTorrent{Hash: testHash, Name: "Movie", SavePath: env.saveDir})

	want := filepath.Join(env.saveDir, "video_"+testHash[:8]+".mkv")
	data, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("目标已存在时应改名到 %s: %v", filepath.Base(want), err)
	}
	if string(data) != "new" {
		t.Errorf("改名后的文件内容错误: %q", data)
	}
	// 原有同名文件不能被动
	if old, err := os.ReadFile(existing); err != nil || string(old) != "old" {
		t.Errorf("已存在的同名文件不应被覆盖: %q err=%v", old, err)
	}
}

// TestManager_handleCompleted_FallbackRenameErrorResumes 覆盖本地 os.Rename 失败分支：
// savePath 只读 → 搬移失败 → 恢复任务后直接返回（不删目录）。
func TestManager_handleCompleted_FallbackRenameErrorResumes(t *testing.T) {
	skipIfRoot(t)
	env := newFilemgrEnv(t, false, 15, `[]`)
	env.mu.Lock()
	env.renameStatus = http.StatusInternalServerError
	env.mu.Unlock()

	torrentDir := filepath.Join(env.saveDir, "Movie")
	if err := os.MkdirAll(torrentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(torrentDir, "video.mkv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// savePath 只读 → os.Rename 目标不可写
	if err := os.Chmod(env.saveDir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(env.saveDir, 0o755) }()

	env.m.handleCompleted(models.QBTorrent{Hash: testHash, Name: "Movie", SavePath: env.saveDir})

	// 失败路径必须尝试恢复任务
	if env.callCount("start") != 1 {
		t.Errorf("本地改名失败也应尝试 start: %v", env.calls())
	}
	// 目录不能被删掉
	if _, err := os.Stat(torrentDir); err != nil {
		t.Errorf("改名失败时源目录应保留: %v", err)
	}
}

// TestManager_handleCompleted_ContentPathEqualsSavePath_NeverDeletesRoot 是 DSOD
// 删库事故的回归：content_path == save_path（多文件种子无根目录、文件散在共享保存根）
// 时 rel == "."。此路径下既无可上移的子目录，也绝不允许对保存根做 os.RemoveAll。
// 即便保存根里恰好只剩一个「别人的」文件（被其它任务归档移进来的），本任务也必须
// 原样跳过——无任何 qB 调用、不删保存根、不动其中文件。
func TestManager_handleCompleted_ContentPathEqualsSavePath_NeverDeletesRoot(t *testing.T) {
	env := newFilemgrEnv(t, false, 15, `[]`)
	saveRoot := env.saveDir // save_path 本身就是共享保存根
	// 另一个任务的大文件恰好躺在保存根（模拟别的 torrent 归档把它移到了这里）
	precious := filepath.Join(saveRoot, "489155_someoneelse.mkv")
	if err := os.WriteFile(precious, []byte("7.3GB precious data"), 0o644); err != nil {
		t.Fatal(err)
	}

	// DSOD 的 content_path == save_path，目录里只有那一个（别人的）文件
	already, didWork := env.m.handleCompleted(models.QBTorrent{
		Hash: testHash, Name: "DSOD-060-UC", SavePath: saveRoot, ContentPath: saveRoot,
	})

	if !already || didWork {
		t.Errorf("内容路径==保存路径应跳过（already=true,didWork=false），got already=%v didWork=%v", already, didWork)
	}
	if len(env.calls()) != 0 {
		t.Errorf("内容路径==保存路径绝不能触发任何 qB 调用: %v", env.calls())
	}
	// 保存根与其中文件必须原封不动
	if _, err := os.Stat(saveRoot); err != nil {
		t.Errorf("保存根绝不能被删除: %v", err)
	}
	if data, err := os.ReadFile(precious); err != nil || string(data) != "7.3GB precious data" {
		t.Errorf("保存根内文件绝不能被动/删: err=%v data=%q", err, data)
	}
	if env.m.getFlattenDir(testHash) != "" {
		t.Errorf("跳过场景不应记录 pending 清理目录: %q", env.m.getFlattenDir(testHash))
	}
}

// TestManager_handleCompleted_CleansDirAfterContentPathBecomesFile 模拟真实 qB 5.x：
// 移动落地后 content_path 会从目录变成新文件路径。旧实现依赖 content_path 定位目录，
// 下一轮会判定 !IsDir 直接 done，空目录永远泄漏；新实现用记录的 pending 目录完成清理。
func TestManager_handleCompleted_CleansDirAfterContentPathBecomesFile(t *testing.T) {
	env := newFilemgrEnv(t, false, 15, `[]`)
	save := env.saveDir
	sub := filepath.Join(save, "Foo")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "Foo.mkv"), []byte("movie"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 第一轮：content_path 指向子目录，提交 qB 移动（mock 不真搬），记录 pending
	already1, did1 := env.m.handleCompleted(models.QBTorrent{
		Hash: testHash, Name: "Foo", SavePath: save, ContentPath: sub,
	})
	if already1 || did1 {
		t.Fatalf("第一轮应提交移动并等待清理（false,false），got (%v,%v)", already1, did1)
	}
	if env.callCount("renameFile") != 1 {
		t.Fatalf("第一轮应提交 renameFile: %v", env.calls())
	}
	if env.m.getFlattenDir(testHash) != sub {
		t.Fatalf("应记录待清理目录 %s，实际 %q", sub, env.m.getFlattenDir(testHash))
	}

	// 模拟 qB 异步移动落地：文件移到保存根，子目录变空，content_path 变为文件路径
	if err := os.Rename(filepath.Join(sub, "Foo.mkv"), filepath.Join(save, "Foo.mkv")); err != nil {
		t.Fatal(err)
	}

	// 第二轮：content_path 已变成文件路径（旧实现会 !IsDir 直接 done，泄漏空目录）
	already2, did2 := env.m.handleCompleted(models.QBTorrent{
		Hash: testHash, Name: "Foo", SavePath: save, ContentPath: filepath.Join(save, "Foo.mkv"),
	})
	if already2 || !did2 {
		t.Fatalf("第二轮应清理空目录并标记完成（false,true），got (%v,%v)", already2, did2)
	}
	if _, err := os.Stat(sub); !os.IsNotExist(err) {
		t.Errorf("空目录应被清理，stat err=%v", err)
	}
	if data, err := os.ReadFile(filepath.Join(save, "Foo.mkv")); err != nil || string(data) != "movie" {
		t.Errorf("归档后的文件必须完好: err=%v data=%q", err, data)
	}
	if env.m.getFlattenDir(testHash) != "" {
		t.Errorf("清理后应清空 pending，实际 %q", env.m.getFlattenDir(testHash))
	}
	if env.callCount("renameFile") != 1 {
		t.Errorf("第二轮不应再提交 qB 调用: %v", env.calls())
	}
}

// TestManager_finishFlatten_NeverDeletesNonEmptyDir 保证 qB 移动迟迟不落地时，
// finishFlatten 只等待/放弃，绝不删除仍有文件的目录（防数据丢失）。
func TestManager_finishFlatten_NeverDeletesNonEmptyDir(t *testing.T) {
	env := newFilemgrEnv(t, false, 15, `[]`)
	save := env.saveDir
	sub := filepath.Join(save, "Foo")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// 文件仍在原目录（模拟 qB 移动尚未落地）
	if err := os.WriteFile(filepath.Join(sub, "Foo.mkv"), []byte("movie"), 0o644); err != nil {
		t.Fatal(err)
	}
	tor := models.QBTorrent{Hash: testHash, Name: "Foo", SavePath: save, ContentPath: sub}

	// 未落地：应等待（false,false），绝不删目录
	env.m.setFlattenDir(testHash, sub)
	if already, did := env.m.finishFlatten(tor, sub); already || did {
		t.Errorf("目录仍有文件时应等待（false,false），got (%v,%v)", already, did)
	}
	if data, err := os.ReadFile(filepath.Join(sub, "Foo.mkv")); err != nil || string(data) != "movie" {
		t.Fatalf("非空目录绝不能被删: err=%v data=%q", err, data)
	}

	// 把等待计数推到上限前一轮，再触发一次即放弃：应清 pending 且返回 (true,false)，仍不删目录
	env.m.mu.Lock()
	env.m.cleanState[testHash].FlattenAttempts = flattenMaxAttempts - 1
	env.m.mu.Unlock()
	if already, did := env.m.finishFlatten(tor, sub); !already || did {
		t.Errorf("超过上限应放弃（true,false），got (%v,%v)", already, did)
	}
	if data, err := os.ReadFile(filepath.Join(sub, "Foo.mkv")); err != nil || string(data) != "movie" {
		t.Errorf("放弃后也绝不能删非空目录: err=%v data=%q", err, data)
	}
	if env.m.getFlattenDir(testHash) != "" {
		t.Errorf("放弃后应清空 pending，实际 %q", env.m.getFlattenDir(testHash))
	}
}

func TestIsStrictSubdir(t *testing.T) {
	cases := []struct {
		parent, child string
		want          bool
	}{
		{"/a", "/a/b", true},
		{"/a", "/a/b/c", true},
		{"/a", "/a", false},       // 相等 → 不是严格子目录（保存根本身）
		{"/a", "/b", false},       // 完全在外
		{"/a", "/ab", false},      // 前缀障眼（不是 /a 的子路径）
		{"/a", "/a/../a/b", true}, // Clean 后为 /a/b
		{"/a/b", "/a", false},     // child 是 parent 的祖先
		{"", "/a", false},
		{"/a", "", false},
	}
	for _, c := range cases {
		if got := isStrictSubdir(c.parent, c.child); got != c.want {
			t.Errorf("isStrictSubdir(%q,%q)=%v want %v", c.parent, c.child, got, c.want)
		}
	}
}

func TestConflictFreeName(t *testing.T) {
	if got := conflictFreeName("video.mkv", "h1h2h3h4h5h6"); got != "video_h1h2h3h4.mkv" {
		t.Errorf("长 hash 应截前 8 位: %q", got)
	}
	if got := conflictFreeName("video.mkv", "abc"); got != "video_abc.mkv" {
		t.Errorf("短 hash 应用全量: %q", got)
	}
}

// TestManager_scan_FreshHashHandledOnce 覆盖 scan 对新完成任务的处理与 done 去重
func TestManager_scan_FreshHashHandledOnce(t *testing.T) {
	env := newFilemgrEnv(t, false, 15,
		`[{"hash":"h1h2h3h4h5h6","name":"Movie","state":"stoppedUP","progress":1.0,"save_path":"{SAVE}"}]`)
	// 目录不存在 → handleCompleted 提前返回，但 done 标记已经落库
	env.m.scan()
	if !env.m.done[testHash] {
		t.Fatal("首个完成任务应被标记 done")
	}
	env.m.scan()
	if len(env.m.done) != 1 {
		t.Errorf("重复 scan 不应重复处理: %v", env.m.done)
	}
}
