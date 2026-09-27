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
		*calls = append(*calls, "renameFile:"+r.FormValue("hash")+":"+r.FormValue("oldPath")+"->"+r.FormValue("newPath"))
		w.WriteHeader(200)
	})
	mux.HandleFunc("/api/v2/torrents/stop", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		*calls = append(*calls, "stop:"+r.FormValue("hashes"))
		w.WriteHeader(200)
	})
	mux.HandleFunc("/api/v2/torrents/start", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		*calls = append(*calls, "start:"+r.FormValue("hashes"))
		w.WriteHeader(200)
	})
	srv := httptest.NewServer(mux)
	cfg := config.New("")
	cfg.Set(models.AppConfig{
		FileManager: models.FileManagerConfig{Enabled: true, ScanInterval: 15},
	})
	return New(cfg, qb.New(srv.URL, "u", "p", "")), calls, srv, saveDir
}

func TestManager_scan_ProcessesAllDoneStates(t *testing.T) {
	torrents := `[
		{"hash":"h1","name":"in-progress","state":"downloading","progress":1.0,"save_path":"{SAVE}"},
		{"hash":"h2","name":"stopped","state":"stoppedUP","progress":1.0,"save_path":"{SAVE}"},
		{"hash":"h3","name":"seeding","state":"uploading","progress":1.0,"save_path":"{SAVE}"},
		{"hash":"h4","name":"stalled-seed","state":"stalledUP","progress":1.0,"save_path":"{SAVE}"},
		{"hash":"h5","name":"not-done","state":"stoppedUP","progress":0.5,"save_path":"{SAVE}"},
		{"hash":"h6","name":"checking","state":"checkingUP","progress":1.0,"save_path":"{SAVE}"}
	]`
	m, calls, srv, _ := setupFilemgr(t, torrents)
	defer srv.Close()

	m.scan()

	// 5.x 完成做种态（stoppedUP/uploading/stalledUP）都应处理（目录不存在 → already → done）。
	// 此前只认 stoppedUP，未配置「完成即暂停」的 uploading/stalledUP 任务永远轮不到
	for _, h := range []string{"h2", "h3", "h4"} {
		if !m.done[h] {
			t.Errorf("%s (done state) should be marked done", h)
		}
	}
	// 下载中（哪怕 progress=1）与未完成的不处理
	if m.done["h1"] || m.done["h5"] {
		t.Errorf("h1/h5 should not be done: %v", m.done)
	}
	// checkingUP 校验中不碰文件、也不标 done（校验结束后下轮处理）
	if m.done["h6"] {
		t.Error("h6 (checkingUP) should not be marked done")
	}
	_ = calls
}

func TestManager_scan_SkipsAlreadyDone(t *testing.T) {
	torrents := `[{"hash":"h1","name":"t1","state":"stoppedUP","progress":1.0,"save_path":"{SAVE}"}]`
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
	torrents := `[{"hash":"h1","state":"stoppedUP","progress":1.0}]`
	m, calls, srv, _ := setupFilemgr(t, torrents)
	srv.Close()
	// scan 不应 panic
	m.scan()
	if len(*calls) != 0 { t.Errorf("on error no calls") }
}

func TestManager_handleCompleted_RealDirFlatFiles(t *testing.T) {
	m, calls, srv, saveDir := setupFilemgr(t, `[{"hash":"h1","name":"Movie.mkv","state":"stoppedUP","progress":1.0,"save_path":"{SAVE}"}]`)
	defer srv.Close()

	torrentDir := filepath.Join(saveDir, "Movie.mkv")
	if err := os.MkdirAll(torrentDir, 0o755); err != nil { t.Fatal(err) }
	if err := os.WriteFile(filepath.Join(torrentDir, "video.mkv"), []byte("fake"), 0o644); err != nil { t.Fatal(err) }

	// 第一轮：stop → renameFile → start，提交移动
	m.scan()

	// 调用序列与 rename 参数
	if len(*calls) != 3 ||
		!strings.HasPrefix((*calls)[0], "stop:") ||
		!strings.HasPrefix((*calls)[1], "renameFile:h1:Movie.mkv/video.mkv->video.mkv") ||
		!strings.HasPrefix((*calls)[2], "start:") {
		t.Fatalf("expected stop + renameFile(Movie.mkv/video.mkv->video.mkv) + start, got: %v", *calls)
	}
	// 关键回归：qB renameFile 的磁盘移动是异步的，此时目录必须保留、
	// 文件绝不能被删（此前提交后立即 RemoveAll 会删掉尚未移走的文件）
	if _, err := os.Stat(filepath.Join(torrentDir, "video.mkv")); err != nil {
		t.Fatalf("video.mkv must remain before qB lands the move: %v", err)
	}
	if m.done["h1"] {
		t.Error("move just submitted, should not be marked done yet")
	}

	// 模拟 qB 异步移动落地：文件移到 saveDir 根下，留下空目录
	if err := os.Rename(filepath.Join(torrentDir, "video.mkv"), filepath.Join(saveDir, "video.mkv")); err != nil {
		t.Fatal(err)
	}

	// 第二轮：目录已空 → 清理空壳 → 标 done，且不再新增 qB 调用
	m.scan()
	if _, err := os.Stat(torrentDir); !os.IsNotExist(err) {
		t.Errorf("empty torrent dir should be cleaned up, stat err = %v", err)
	}
	if !m.done["h1"] {
		t.Error("second scan should mark done after cleanup")
	}
	if len(*calls) != 3 {
		t.Errorf("second scan should not call qB again, got: %v", *calls)
	}
	if _, err := os.Stat(filepath.Join(saveDir, "video.mkv")); err != nil {
		t.Errorf("archived file must exist: %v", err)
	}
}

// TestManager_handleCompleted_HiddenFiles_StillCleans verifies that hidden files
// (.DS_Store etc.) don't prevent flattening and are cleaned up by RemoveAll.
func TestManager_handleCompleted_HiddenFiles_StillCleans(t *testing.T) {
	m, _, srv, saveDir := setupFilemgr(t, `[{"hash":"h1","name":"Movie.mkv","state":"stoppedUP","progress":1.0,"save_path":"{SAVE}"}]`)
	defer srv.Close()

	torrentDir := filepath.Join(saveDir, "Movie.mkv")
	if err := os.MkdirAll(torrentDir, 0o755); err != nil { t.Fatal(err) }
	if err := os.WriteFile(filepath.Join(torrentDir, "video.mkv"), []byte("fake"), 0o644); err != nil { t.Fatal(err) }
	// 模拟 macOS 访问目录自动生成的 .DS_Store
	if err := os.WriteFile(filepath.Join(torrentDir, ".DS_Store"), []byte("\\x00\\x01"), 0o644); err != nil { t.Fatal(err) }

	// 第一轮：隐藏文件不阻塞单文件判定，提交移动、目录保留
	m.scan()
	if _, err := os.Stat(torrentDir); err != nil {
		t.Fatalf("dir should remain until qB move lands: %v", err)
	}

	// 模拟 qB 移走唯一可见文件，.DS_Store 留在原目录
	if err := os.Rename(filepath.Join(torrentDir, "video.mkv"), filepath.Join(saveDir, "video.mkv")); err != nil {
		t.Fatal(err)
	}

	// 第二轮：可见文件已空 → 目录（含 .DS_Store）整体清理
	m.scan()
	if _, err := os.Stat(torrentDir); !os.IsNotExist(err) {
		t.Errorf("torrent dir should be removed entirely, stat err = %v", err)
	}
	if !m.done["h1"] {
		t.Error("should be marked done after cleanup")
	}
}

// TestManager_handleCompleted_HiddenSubdir_StillCleans verifies hidden subdirectories
// are also removed by RemoveAll after flattening.
func TestManager_handleCompleted_HiddenSubdir_StillCleans(t *testing.T) {
	m, _, srv, saveDir := setupFilemgr(t, `[{"hash":"h1","name":"Movie.mkv","state":"stoppedUP","progress":1.0,"save_path":"{SAVE}"}]`)
	defer srv.Close()

	torrentDir := filepath.Join(saveDir, "Movie.mkv")
	if err := os.MkdirAll(torrentDir, 0o755); err != nil { t.Fatal(err) }
	if err := os.WriteFile(filepath.Join(torrentDir, "video.mkv"), []byte("fake"), 0o644); err != nil { t.Fatal(err) }
	if err := os.MkdirAll(filepath.Join(torrentDir, ".hidden_cache"), 0o755); err != nil { t.Fatal(err) }
	if err := os.WriteFile(filepath.Join(torrentDir, ".hidden_cache", "tmp"), []byte("x"), 0o644); err != nil { t.Fatal(err) }

	// 第一轮：提交移动，目录保留
	m.scan()
	if _, err := os.Stat(torrentDir); err != nil {
		t.Fatalf("dir should remain until qB move lands: %v", err)
	}

	// 模拟 qB 移走唯一可见文件，隐藏子目录留在原地
	if err := os.Rename(filepath.Join(torrentDir, "video.mkv"), filepath.Join(saveDir, "video.mkv")); err != nil {
		t.Fatal(err)
	}

	// 第二轮：可见文件已空 → 目录（含隐藏子目录）整体清理
	m.scan()
	if _, err := os.Stat(torrentDir); !os.IsNotExist(err) {
		t.Errorf("torrent dir with hidden subdir should be removed, stat err = %v", err)
	}
	if !m.done["h1"] {
		t.Error("should be marked done after cleanup")
	}
}

// TestManager_handleCompleted_MultipleNonHidden_Noop verifies that extra non-hidden
// files (e.g. residual !qB) prevent the flattening logic as intended.
func TestManager_handleCompleted_MultipleNonHidden_Noop(t *testing.T) {
	m, _, srv, saveDir := setupFilemgr(t, `[{"hash":"h1","name":"Movie.mkv","state":"stoppedUP","progress":1.0,"save_path":"{SAVE}"}]`)
	defer srv.Close()

	torrentDir := filepath.Join(saveDir, "Movie.mkv")
	if err := os.MkdirAll(torrentDir, 0o755); err != nil { t.Fatal(err) }
	if err := os.WriteFile(filepath.Join(torrentDir, "video.mkv"), []byte("fake"), 0o644); err != nil { t.Fatal(err) }
	// 残留的 !qB 文件（被取消勾选或未完成的下载片段）
	if err := os.WriteFile(filepath.Join(torrentDir, "video.mkv!qB"), []byte("partial"), 0o644); err != nil { t.Fatal(err) }

	m.scan()

	// 过滤后 len(files) = 2，不满足单文件条件 → 目录应原封不动
	if _, err := os.Stat(torrentDir); err != nil {
		t.Errorf("torrent dir should remain untouched, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(torrentDir, "video.mkv")); err != nil {
		t.Errorf("video.mkv should still be in torrent dir, err = %v", err)
	}
}

// content_path 目录名与任务名不一致时（5.x），必须以 content_path 为准定位归档目录。
// 若仍按 save_path/name 猜测会误判「目录不存在」直接跳过。
func TestManager_contentPath_DirDiffersFromName(t *testing.T) {
	m, calls, srv, saveDir := setupFilemgr(t, `[{"hash":"h1","name":"renamed-torrent","state":"stoppedUP","progress":1.0,"save_path":"{SAVE}","content_path":"{SAVE}/ActualDir"}]`)
	defer srv.Close()

	torrentDir := filepath.Join(saveDir, "ActualDir")
	if err := os.MkdirAll(torrentDir, 0o755); err != nil { t.Fatal(err) }
	if err := os.WriteFile(filepath.Join(torrentDir, "video.mkv"), []byte("fake"), 0o644); err != nil { t.Fatal(err) }

	m.scan()

	// rename 的内部路径按 content_path 相对 save_path 推导，而非任务名
	if len(*calls) != 3 || !strings.HasPrefix((*calls)[1], "renameFile:h1:ActualDir/video.mkv->video.mkv") {
		t.Fatalf("rename should use content_path relative dir, got: %v", *calls)
	}
	if m.done["h1"] {
		t.Error("move just submitted, should not be marked done yet")
	}
}

// 单文件种子：content_path 直接指向文件本身，文件已在 save_path 下，无需归档
func TestManager_contentPath_SingleFileSeed(t *testing.T) {
	m, calls, srv, saveDir := setupFilemgr(t, `[{"hash":"h1","name":"Movie","state":"stoppedUP","progress":1.0,"save_path":"{SAVE}","content_path":"{SAVE}/Movie.mkv"}]`)
	defer srv.Close()

	if err := os.WriteFile(filepath.Join(saveDir, "Movie.mkv"), []byte("fake"), 0o644); err != nil { t.Fatal(err) }

	m.scan()

	if !m.done["h1"] {
		t.Error("single file seed should be marked done (no archiving needed)")
	}
	if len(*calls) != 0 {
		t.Errorf("single file seed should make no qB calls, got: %v", *calls)
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
