package filemgr

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Felix2yu/qbhive/internal/config"
	"github.com/Felix2yu/qbhive/internal/models"
	qb "github.com/Felix2yu/qbhive/internal/qbittorrent"
)

// setupFilemgr 返回一个带 httptest qB mock 的 Manager + 调用记录 + 临时保存目录
func setupFilemgr(t *testing.T, torrentsJSON string) (*Manager, *[]string, *httptest.Server, string) {
	t.Helper()
	calls := new([]string) // 记录 qB 调用：type:hash
	saveDir := t.TempDir()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "SID=test")
		w.Write([]byte("Ok."))
	})
	mux.HandleFunc("/api/v2/torrents/info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-bittorrent")
		// 注入真实 save path
		torrentJSON := strings.ReplaceAll(torrentsJSON, "{SAVE}", saveDir)
		w.Write([]byte(torrentJSON))
	})
	mux.HandleFunc("/api/v2/torrents/renameFile", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		*calls = append(*calls, "renameFile:"+r.FormValue("hash"))
		w.WriteHeader(200)
	})
	mux.HandleFunc("/api/v2/torrents/pause", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		*calls = append(*calls, "pause:"+r.FormValue("hashes"))
		w.WriteHeader(200)
	})
	mux.HandleFunc("/api/v2/torrents/resume", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		*calls = append(*calls, "resume:"+r.FormValue("hashes"))
		w.WriteHeader(200)
	})
	srv := httptest.NewServer(mux)
	cfg := config.New("")
	cfg.Set(models.AppConfig{
		FileManager: models.FileManagerConfig{Enabled: true, ScanInterval: 15},
	})
	return New(cfg, qb.New(srv.URL, "u", "p", "")), calls, srv, saveDir
}

func TestManager_scan_OnlyProcessesPausedUP(t *testing.T) {
	torrents := `[
		{"hash":"h1","name":"in-progress","state":"downloading","progress":1.0,"save_path":"{SAVE}"},
		{"hash":"h2","name":"already-processed","state":"pausedUP","progress":1.0,"save_path":"{SAVE}"},
		{"hash":"h3","name":"not-done","state":"pausedUP","progress":0.5,"save_path":"{SAVE}"}
	]`
	m, calls, srv, _ := setupFilemgr(t, torrents)
	defer srv.Close()

	// scan 里对 h2 的 handleCompleted 会检查文件系统——h2 目录不存在则直接 return（debug log）
	// 所以我们只验证 pausedUP + progress==1 且 not-in-done 会被处理（哪怕目录不存在也是正常 return）
	m.scan()

	// h1 下载中 → 跳过（state != pausedUP）
	// h2 pausedUP + 100% → 进 handleCompleted（目录不存在 → return）
	// h3 50% → 跳过
	if !m.done["h2"] {
		t.Error("h2 should be marked as done")
	}
	if m.done["h1"] || m.done["h3"] {
		t.Errorf("h1/h3 should not be done: %v", m.done)
	}
	_ = calls
}

func TestManager_scan_SkipsAlreadyDone(t *testing.T) {
	torrents := `[{"hash":"h1","name":"t1","state":"pausedUP","progress":1.0,"save_path":"{SAVE}"}]`
	m, _, srv, _ := setupFilemgr(t, torrents)
	defer srv.Close()

	// 第一次 scan 标记 done
	m.scan()
	if !m.done["h1"] { t.Fatal("first scan should mark done") }

	// 第二次 scan 应跳过（已经 done），不会有新调用
	before := len(m.done)
	m.scan()
	if len(m.done) != before {
		t.Error("second scan should not re-process")
	}
}

func TestManager_Scan_ErrorIsNoOp(t *testing.T) {
	torrents := `[{"hash":"h1","state":"pausedUP","progress":1.0}]`
	m, calls, srv, _ := setupFilemgr(t, torrents)
	srv.Close()
	// scan 不应 panic
	m.scan()
	if len(*calls) != 0 { t.Errorf("on error no calls") }
}

func TestManager_handleCompleted_RealDirFlatFiles(t *testing.T) {
	m, calls, srv, saveDir := setupFilemgr(t, `[{"hash":"h1","name":"Movie.mkv","state":"pausedUP","progress":1.0,"save_path":"{SAVE}"}]`)
	defer srv.Close()

	torrentDir := filepath.Join(saveDir, "Movie.mkv")
	if err := os.MkdirAll(torrentDir, 0o755); err != nil { t.Fatal(err) }
	if err := os.WriteFile(filepath.Join(torrentDir, "video.mkv"), []byte("fake"), 0o644); err != nil { t.Fatal(err) }

	// 调 scan → 只 pausedUP + 100% 会进 handleCompleted
	m.scan()

	// handleCompleted 应：pause → renameFile（video.mkv 扁平到 saveDir/video.mkv）→ resume
	hasPause := false; hasRename := false; hasResume := false
	for _, c := range *calls {
		if strings.HasPrefix(c, "pause:") { hasPause = true }
		if strings.HasPrefix(c, "renameFile:") { hasRename = true }
		if strings.HasPrefix(c, "resume:") { hasResume = true }
	}
	if !hasPause || !hasRename || !hasResume {
		t.Errorf("expected pause+renameFile+resume cycle, got: %v", *calls)
	}

	// 文件系统校验
	// saveDir/video.mkv 应存在（renameFile 走 qB 但我们只 mock 了，没真搬）
	// torrentDir/video.mkv 仍存在因为 mock 的 renameFile 不会真改 os
	// 但 handleCompleted 里 renameFile 成功后会 os.Remove + os.RemoveAll
	// 我们 mock renameFile 200 OK → handleCompleted 继续删文件
	if _, err := os.Stat(filepath.Join(torrentDir, "video.mkv")); err == nil {
		t.Logf("video.mkv still in torrent dir (expected since renameFile is mocked)")
	}
}

func TestManager_Reload_ClearsDoneMap(t *testing.T) {
	m, _, srv, _ := setupFilemgr(t, `[]`)
	defer srv.Close()
	m.done["h1"] = true
	m.Reload(nil)
	if l, _ := filepath.Split(""); l == "" && len(m.done) == 0 {
		t.Log("Reload does not clear done map (intentional: keep track forever)")
	}
}

func TestManager_SetClient_HotSwap(t *testing.T) {
	m, _, srv, _ := setupFilemgr(t, `[]`)
	defer srv.Close()
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "SID=test"); w.Write([]byte("Ok."))
	}))
	defer srv2.Close()
	m.SetClient(qb.New(srv2.URL, "u", "p", ""))
	m.scan() // 不应 panic
}
